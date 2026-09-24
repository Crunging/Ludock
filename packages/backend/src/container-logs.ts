import { byteView } from "./bytes.js";
import type { SocketChannel } from "./socket-channel.js";
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
  ws: SocketChannel,
  req: Request,
  auth: WebSocketAuth,
  remoteAddress?: string,
): Promise<void> {
  const url = new URL(req.url);
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
    userId: user.id,
    action: "server.logs.opened",
    targetType: "server",
    targetId: serverId,
    details: {
      containerId,
      bindingRevision: access.context.logical.bindingRevision,
    },
    ipAddress: remoteAddress,
  });
  send("system", "Following Docker logs");

  let logReader: ReturnType<ReadableStream<Uint8Array>["getReader"]> | undefined;
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
    void logReader?.cancel().catch(() => {});
    logReader = undefined;
  };
  ws.onClose(destroyLogStream);
  try {
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: true,
      tail: 500,
      timestamps: true,
    });
    const reader = stream.getReader();
    logReader = reader;
    if (!access.allowed()) {
      destroyLogStream();
      reader.releaseLock();
      return;
    }
    void (async () => {
      try {
        while (logReader === reader) {
          const { value, done } = await reader.read();
          if (logReader !== reader) return;
          if (done) break;
          decoder.push(value);
        }
        if (!decoder.end()) throw new Error("Incomplete Docker log frame");
        stdout.end();
        stderr.end();
        send("system", "Log stream ended (container may have stopped)");
      } catch {
        if (logReader === reader) {
          logger.warn("Docker log stream failed", { container: shortContainerId(containerId) });
          send("error", "Docker log stream failed");
        }
      } finally {
        if (logReader === reader) logReader = undefined;
        await reader.cancel().catch(() => {});
        reader.releaseLock();
      }
    })();
  } catch {
    logger.warn("Failed to attach Docker log stream", {
      container: shortContainerId(containerId),
    });
    send("error", "Failed to open Docker logs");
  }

  ws.onMessage(() => {
    send("error", "Docker logs are read-only");
  });
  ws.onClose((code) => {
    logger.info("Docker log connection closed", {
      container: shortContainerId(containerId),
      code,
    });
    destroyLogStream();
  });

}

export class DockerLogDecoder {
  private mode: "unknown" | "raw" | "multiplexed" = "unknown";
  private readonly header = new Uint8Array(8);
  private headerBytes = 0;
  private remaining = 0;
  private channel: "stdout" | "stderr" = "stdout";
  private readonly text = {
    stdout: new TextDecoder("utf-8", { ignoreBOM: true }),
    stderr: new TextDecoder("utf-8", { ignoreBOM: true }),
  };

  constructor(
    private readonly output: (type: "stdout" | "stderr", data: string) => void,
  ) {}

  push(chunk: Uint8Array): void {
    if (chunk.length === 0) return;
    if (this.mode === "raw") {
      this.write("stdout", chunk);
      return;
    }

    let offset = 0;
    while (offset < chunk.length) {
      if (this.remaining) {
        const count = Math.min(this.remaining, chunk.length - offset);
        this.write(this.channel, chunk.subarray(offset, offset + count));
        this.remaining -= count;
        offset += count;
        continue;
      }
      // Only retain a partial header. Bodies can span many socket chunks and
      // must reach the consumer without accumulating or copying whole frames.
      const count = Math.min(8 - this.headerBytes, chunk.length - offset);
      this.header.set(chunk.subarray(offset, offset + count), this.headerBytes);
      this.headerBytes += count;
      offset += count;
      if (this.mode === "unknown") {
        if ((this.header[0] !== 1 && this.header[0] !== 2) ||
            (this.headerBytes >= 4 && (this.header[1] || this.header[2] || this.header[3]))) {
          this.useRawMode();
          this.write("stdout", chunk.subarray(offset));
          return;
        }
        if (this.headerBytes >= 4) this.mode = "multiplexed";
      }
      if (this.headerBytes < 8) return;
      const streamType = this.header[0];
      const frameSize = byteView(this.header).getUint32(4);
      if (
        (streamType !== 1 && streamType !== 2) ||
        this.header[1] !== 0 ||
        this.header[2] !== 0 ||
        this.header[3] !== 0 ||
        frameSize > MAX_FRAME_BYTES
      ) {
        this.useRawMode();
        this.write("stdout", chunk.subarray(offset));
        return;
      }
      this.channel = streamType === 2 ? "stderr" : "stdout";
      this.remaining = frameSize;
      this.headerBytes = 0;
    }
  }

  end(): boolean {
    if (this.mode === "unknown" && this.headerBytes > 0) {
      this.useRawMode();
    }
    // Do not flush incomplete UTF-8 or the downstream redaction tail after a
    // truncated frame: that tail may contain a fragment of a credential.
    if (this.headerBytes || this.remaining) return false;
    for (const type of ["stdout", "stderr"] as const) {
      const final = this.text[type].decode();
      if (final) this.output(type, final);
    }
    return true;
  }

  private write(type: "stdout" | "stderr", chunk: Uint8Array): void {
    const value = this.text[type].decode(chunk, { stream: true });
    if (value) this.output(type, value);
  }

  private useRawMode(): void {
    this.mode = "raw";
    if (this.headerBytes > 0) {
      this.write("stdout", this.header.subarray(0, this.headerBytes));
      this.headerBytes = 0;
    }
  }
}

function sendMessage(
  ws: SocketChannel,
  type: "stdout" | "stderr" | "system" | "error",
  data: string,
): void {
  if (ws.isOpen) ws.send(JSON.stringify({ type, data }));
}

function shortContainerId(containerId: string): string {
  return containerId.slice(0, 12);
}
