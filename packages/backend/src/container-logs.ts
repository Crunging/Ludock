import type { IncomingMessage } from "node:http";
import type { WebSocket } from "ws";
import type { WebSocketAuth } from "./auth.js";
import { writeAuditLog } from "./database.js";
import { getContainer, getManagedContainer } from "./docker.js";
import { createLogger, errorMessage } from "./logger.js";

const logger = createLogger("container-logs");
const MAX_FRAME_BYTES = 16 * 1024 * 1024;

export async function handleContainerLogsConnection(
  ws: WebSocket,
  req: IncomingMessage,
  auth: WebSocketAuth
): Promise<void> {
  const url = new URL(req.url || "", `http://${req.headers.host}`);
  const containerId = url.pathname.split("/").filter(Boolean).at(-1);
  if (!containerId) {
    sendMessage(ws, "error", "Missing container ID");
    ws.close(1008, "Missing container ID");
    return;
  }

  try {
    await getManagedContainer(containerId);
  } catch (error) {
    logger.warn("Rejected log connection for unmanaged container", {
      container: shortContainerId(containerId),
      error: errorMessage(error),
    });
    sendMessage(ws, "error", `Container ${containerId} is not managed`);
    ws.close(1008, "Container not managed");
    return;
  }

  const user = auth.user;
  const container = getContainer(containerId);
  logger.info("Docker log connection opened", {
    container: shortContainerId(containerId),
    role: user.role,
  });
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action: "container.logs.opened",
    targetType: "container",
    targetId: containerId,
    ipAddress: req.socket.remoteAddress,
  });
  sendMessage(
    ws,
    "system",
    `Following Docker logs for ${shortContainerId(containerId)}`
  );

  let logStream: NodeJS.ReadableStream | null = null;
  const decoder = new DockerLogDecoder((type, data) =>
    sendMessage(ws, type, data)
  );
  try {
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: true,
      tail: 500,
      timestamps: true,
    });
    logStream = stream;
    stream.on("data", (chunk: Buffer) => decoder.push(chunk));
    stream.on("error", (error: Error) => {
      logger.warn("Docker log stream failed", {
        container: shortContainerId(containerId),
        error: error.message,
      });
      sendMessage(ws, "error", "Docker log stream failed");
    });
    stream.on("end", () => {
      if (!decoder.end()) {
        logger.warn("Docker log stream ended with an incomplete frame", {
          container: shortContainerId(containerId),
        });
      }
      sendMessage(ws, "system", "Log stream ended (container may have stopped)");
    });
  } catch (error) {
    logger.warn("Failed to attach Docker log stream", {
      container: shortContainerId(containerId),
      error: errorMessage(error),
    });
    sendMessage(ws, "error", "Failed to open Docker logs");
  }

  ws.on("message", () => {
    const currentUser = auth.validate();
    if (!currentUser) {
      ws.close(1008, "Session expired or access revoked");
      return;
    }
    auth.user = currentUser;
    sendMessage(ws, "error", "Docker logs are read-only");
  });

  const destroyLogStream = () => {
    if (!logStream) return;
    (logStream as NodeJS.ReadableStream & { destroy?: () => void }).destroy?.();
    logStream = null;
  };
  ws.on("close", (code) => {
    logger.info("Docker log connection closed", {
      container: shortContainerId(containerId),
      code,
    });
    destroyLogStream();
  });
  ws.on("error", (error) => {
    logger.warn("Docker log WebSocket failed", {
      container: shortContainerId(containerId),
      error: error.message,
    });
    destroyLogStream();
  });
}

export class DockerLogDecoder {
  private mode: "unknown" | "raw" | "multiplexed" = "unknown";
  private buffer = Buffer.alloc(0);

  constructor(
    private readonly output: (type: "stdout" | "stderr", data: string) => void
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
  data: string
): void {
  if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type, data }));
}

function shortContainerId(containerId: string): string {
  return containerId.slice(0, 12);
}
