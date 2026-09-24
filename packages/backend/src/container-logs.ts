import type { DockerContainerId } from "@ludock/shared";
import { byteView } from "./bytes.js";
import type { SocketChannel } from "./socket-channel.js";
import type { WebSocketAuth } from "./auth.js";
import { writeAuditLog } from "./database.js";
import { getContainer } from "./docker.js";
import { createLogger } from "./logger.js";
import { openServerSocket, sendSocketMessage, type SocketOutput } from "./server-socket-access.js";
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
  const access = await openServerSocket(
    ws, req, auth, "logs.read", "Server logs are unavailable or access was denied",
  );
  if (!access) return;
  const { serverId, context } = access;
  const containerId = context.logical.containerId;
  const send = (type: SocketOutput, data: string) => {
    if (access.allowed()) sendSocketMessage(ws, type, data);
  };
  logger.info("Docker log connection opened", {
    container: containerId.slice(0, 12),
    role: auth.user.role,
  });
  writeAuditLog({
    userId: auth.user.id,
    action: "server.logs.opened",
    targetType: "server",
    targetId: serverId,
    details: { containerId, bindingRevision: context.logical.bindingRevision },
    ipAddress: remoteAddress,
  });
  send("system", "Following Docker logs");
  ws.onMessage(() => send("error", "Docker logs are read-only"));
  ws.onClose((code) => {
    logger.info("Docker log connection closed", { container: containerId.slice(0, 12), code });
  });
  await followContainerLogs(ws, containerId, {
    tail: 500,
    timestamps: true,
    secrets: observationSecrets(context.observation),
    allowed: () => access.allowed(),
    send,
    endedMessage: "Log stream ended (container may have stopped)",
  });
}

/** Stream redacted container output until the socket closes or access ends. */
export async function followContainerLogs(
  ws: SocketChannel,
  containerId: DockerContainerId,
  options: {
    tail: number;
    timestamps: boolean;
    secrets: readonly string[];
    allowed: () => boolean;
    send: (type: SocketOutput, data: string) => void;
    endedMessage: string;
  },
): Promise<void> {
  const { send } = options;
  const stdout = new ConsoleOutputRedactor(options.secrets, (value) => send("stdout", value));
  const stderr = new ConsoleOutputRedactor(options.secrets, (value) => send("stderr", value));
  const decoder = new DockerLogDecoder((type, value) =>
    (type === "stdout" ? stdout : stderr).push(value),
  );
  let reader: ReturnType<ReadableStream<Uint8Array>["getReader"]>;
  try {
    const stream = await getContainer(containerId).logs({
      follow: true, stdout: true, stderr: true,
      tail: options.tail, timestamps: options.timestamps,
    });
    reader = stream.getReader();
  } catch {
    logger.warn("Failed to attach Docker log stream", { container: containerId.slice(0, 12) });
    send("error", "Failed to open Docker logs");
    return;
  }
  let stopped = false;
  const stop = () => {
    stopped = true;
    void reader.cancel().catch(() => {});
  };
  ws.onClose(stop);
  if (!options.allowed()) stop();
  void (async () => {
    try {
      while (!stopped) {
        const { value, done } = await reader.read();
        if (stopped) return;
        if (done) break;
        decoder.push(value);
      }
      if (stopped) return;
      if (!decoder.end()) throw new Error("Incomplete Docker log frame");
      stdout.end();
      stderr.end();
      send("system", options.endedMessage);
    } catch {
      if (!stopped) {
        logger.warn("Docker log stream failed", { container: containerId.slice(0, 12) });
        send("error", "Docker log stream failed");
      }
    } finally {
      stopped = true;
      await reader.cancel().catch(() => {});
      reader.releaseLock();
    }
  })();
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
