import type { IncomingMessage } from "node:http";
import type { WebSocket } from "ws";
import type { WebSocketAuth } from "./auth.js";
import { writeAuditLog } from "./database.js";
import { getContainer } from "./docker.js";
import { createLogger } from "./logger.js";
import { authorizeServerSocket } from "./server-socket-access.js";
import {
  ConsoleOutputRedactor,
  observationSecrets,
} from "./console-redaction.js";

const logger = createLogger("container-logs");
const MAX_FRAME_BYTES = 16 * 1024 * 1024;

export async function handleContainerLogsConnection(
  ws: WebSocket,
  req: IncomingMessage,
  auth: WebSocketAuth,
): Promise<void> {
  const url = new URL(req.url || "", `http://${req.headers.host}`);
  const serverId = url.pathname.split("/").filter(Boolean).at(-1);
  if (!serverId) {
    sendMessage(ws, "error", "Missing server ID");
    ws.close(1008, "Missing server ID");
    return;
  }
  let access;
  try {
    access = await authorizeServerSocket(ws, auth, serverId, "logs.read");
  } catch {
    sendMessage(
      ws,
      "error",
      "Server logs are unavailable or access was denied",
    );
    ws.close(1008, "Server access unavailable");
    return;
  }
  if (!access.allowed()) return;
  const containerId = access.context.logical.containerId;
  const send = (
    type: "stdout" | "stderr" | "system" | "error",
    data: string,
  ) => {
    if (access.allowed()) sendMessage(ws, type, data);
  };
  const user = auth.user;
  const container = getContainer(containerId);
  logger.info("Docker log connection opened", {
    container: shortContainerId(containerId),
    role: user.role,
  });
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: "server.logs.opened",
    targetType: "server",
    targetId: serverId,
    details: {
      containerId,
      bindingRevision: access.context.logical.bindingRevision,
    },
    ipAddress: req.socket.remoteAddress,
  });
  send("system", "Following Docker logs");

  let logStream: NodeJS.ReadableStream | null = null;
  const secrets = observationSecrets(access.context.observation);
  const stdout = new ConsoleOutputRedactor(secrets, (value) =>
    send("stdout", value),
  );
  const stderr = new ConsoleOutputRedactor(secrets, (value) =>
    send("stderr", value),
  );
  const decoder = new DockerLogDecoder((type, data) =>
    (type === "stdout" ? stdout : stderr).push(data),
  );
  const destroyLogStream = () => {
    if (!logStream) return;
    (logStream as NodeJS.ReadableStream & { destroy?: () => void }).destroy?.();
    logStream = null;
  };
  ws.once("close", destroyLogStream);
  ws.once("error", destroyLogStream);
  try {
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: true,
      tail: 500,
      timestamps: true,
    });
    logStream = stream;
    if (!access.allowed()) {
      destroyLogStream();
      return;
    }
    stream.on("data", (chunk: Buffer) => decoder.push(chunk));
    stream.on("error", () => {
      logger.warn("Docker log stream failed", {
        container: shortContainerId(containerId),
      });
      send("error", "Docker log stream failed");
    });
    stream.on("end", () => {
      if (!decoder.end()) {
        logger.warn("Docker log stream ended with an incomplete frame", {
          container: shortContainerId(containerId),
        });
      }
      stdout.end();
      stderr.end();
      send("system", "Log stream ended (container may have stopped)");
    });
  } catch {
    logger.warn("Failed to attach Docker log stream", {
      container: shortContainerId(containerId),
    });
    send("error", "Failed to open Docker logs");
  }

  ws.on("message", () => {
    send("error", "Docker logs are read-only");
  });
  ws.on("close", (code) => {
    logger.info("Docker log connection closed", {
      container: shortContainerId(containerId),
      code,
    });
    destroyLogStream();
  });
  ws.on("error", () => {
    logger.warn("Docker log WebSocket failed", {
      container: shortContainerId(containerId),
    });
    destroyLogStream();
  });
}

export class DockerLogDecoder {
  private mode: "unknown" | "raw" | "multiplexed" = "unknown";
  private buffer = Buffer.alloc(0);

  constructor(
    private readonly output: (type: "stdout" | "stderr", data: string) => void,
  ) {}

  push(chunk: Buffer): void {
    if (chunk.length === 0) return;
    if (this.mode === "raw") {
      this.output("stdout", chunk.toString());
      return;
    }

    this.buffer = Buffer.concat([this.buffer, chunk]);
    if (this.mode === "unknown") {
      const firstByte = this.buffer[0];
      if (firstByte !== 1 && firstByte !== 2) {
        this.useRawMode();
        return;
      }
      if (this.buffer.length < 4) return;
      if (
        this.buffer[1] !== 0 ||
        this.buffer[2] !== 0 ||
        this.buffer[3] !== 0
      ) {
        this.useRawMode();
        return;
      }
      this.mode = "multiplexed";
    }

    while (this.buffer.length >= 8) {
      const streamType = this.buffer[0];
      const frameSize = this.buffer.readUInt32BE(4);
      if (
        (streamType !== 1 && streamType !== 2) ||
        this.buffer[1] !== 0 ||
        this.buffer[2] !== 0 ||
        this.buffer[3] !== 0 ||
        frameSize > MAX_FRAME_BYTES
      ) {
        this.useRawMode();
        return;
      }
      if (this.buffer.length < 8 + frameSize) return;
      const payload = this.buffer.subarray(8, 8 + frameSize);
      this.output(streamType === 2 ? "stderr" : "stdout", payload.toString());
      this.buffer = this.buffer.subarray(8 + frameSize);
    }
  }

  end(): boolean {
    if (this.mode === "unknown" && this.buffer.length > 0) {
      this.useRawMode();
    }
    return this.buffer.length === 0;
  }

  private useRawMode(): void {
    this.mode = "raw";
    if (this.buffer.length > 0) {
      this.output("stdout", this.buffer.toString());
      this.buffer = Buffer.alloc(0);
    }
  }
}

function sendMessage(
  ws: WebSocket,
  type: "stdout" | "stderr" | "system" | "error",
  data: string,
): void {
  if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type, data }));
}

function shortContainerId(containerId: string): string {
  return containerId.slice(0, 12);
}
