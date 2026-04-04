import type { WebSocket } from "ws";
import type { IncomingMessage } from "node:http";
import { getContainer, getManagedContainer, getDockerInstance } from "./docker.js";

// Uses a dual-stream approach: logs for output, exec for input.
// Never calls container.attach(), so no risk of killing PID 1.

export async function handleConsoleConnection(
  ws: WebSocket,
  req: IncomingMessage
): Promise<void> {
  const url = new URL(req.url || "", `http://${req.headers.host}`);
  const segments = url.pathname.split("/").filter(Boolean);
  const containerId = segments[segments.length - 1];

  if (!containerId) {
    sendMessage(ws, "error", "Missing container ID");
    ws.close(1008, "Missing container ID");
    return;
  }

  try {
    await getManagedContainer(containerId);
  } catch {
    sendMessage(ws, "error", `Container ${containerId} is not managed`);
    ws.close(1008, "Container not managed");
    return;
  }

  const container = getContainer(containerId);
  sendMessage(ws, "system", `Connected to container ${containerId.substring(0, 12)}`);

  let logStream: NodeJS.ReadableStream | null = null;

  try {
    const stream = await container.logs({
      follow: true,
      stdout: true,
      stderr: true,
      tail: 200,
      timestamps: false,
    });

    logStream = stream;

    // Docker multiplexes stdout/stderr when tty=false. Each frame has an 8-byte
    // header: [stream_type(1), 0, 0, 0, size(4)]. When tty=true, it's raw.
    stream.on("data", (chunk: Buffer) => {
      if (ws.readyState !== ws.OPEN) return;

      const firstByte = chunk[0];
      if ((firstByte === 1 || firstByte === 2) && chunk.length >= 8) {
        let offset = 0;
        while (offset < chunk.length - 7) {
          const streamType = chunk[offset];
          const frameSize = chunk.readUInt32BE(offset + 4);
          const payload = chunk.subarray(offset + 8, offset + 8 + frameSize);
          const type = streamType === 2 ? "stderr" : "stdout";
          sendMessage(ws, type, payload.toString("utf-8"));
          offset += 8 + frameSize;
        }
      } else {
        sendMessage(ws, "stdout", chunk.toString("utf-8"));
      }
    });

    stream.on("error", (err) => {
      sendMessage(ws, "error", `Log stream error: ${err.message}`);
    });

    stream.on("end", () => {
      sendMessage(ws, "system", "Log stream ended (container may have stopped)");
    });
  } catch (err: any) {
    sendMessage(ws, "error", `Failed to attach log stream: ${err.message}`);
  }

  ws.on("message", async (raw) => {
    let msg: { type: string; data: string };
    try {
      msg = JSON.parse(raw.toString());
    } catch {
      sendMessage(ws, "error", "Invalid message format (expected JSON)");
      return;
    }

    if (msg.type !== "input" || typeof msg.data !== "string") {
      sendMessage(ws, "error", 'Expected { type: "input", data: "..." }');
      return;
    }

    const command = msg.data.trim();
    if (!command) return;

    try {
      const exec = await container.exec({
        Cmd: ["/bin/sh", "-c", command],
        AttachStdout: true,
        AttachStderr: true,
        Tty: false,
      });

      const execStream = await exec.start({ hijack: true, stdin: false });

      const chunks: Buffer[] = [];
      execStream.on("data", (chunk: Buffer) => {
        chunks.push(chunk);
      });

      execStream.on("end", () => {
        const output = Buffer.concat(chunks).toString("utf-8");
        const cleaned = stripMultiplexHeaders(output);
        if (cleaned.trim()) {
          sendMessage(ws, "stdout", cleaned);
        }
      });

      execStream.on("error", (err: Error) => {
        sendMessage(ws, "error", `Exec error: ${err.message}`);
      });
    } catch (err: any) {
      sendMessage(ws, "error", `Failed to exec command: ${err.message}`);
    }
  });

  ws.on("close", () => {
    if (logStream) {
      (logStream as any).destroy?.();
      logStream = null;
    }
  });

  ws.on("error", (err) => {
    console.error(`[Console] WebSocket error for ${containerId}:`, err.message);
    if (logStream) {
      (logStream as any).destroy?.();
      logStream = null;
    }
  });
}

function sendMessage(
  ws: WebSocket,
  type: "stdout" | "stderr" | "system" | "error",
  data: string
): void {
  if (ws.readyState === ws.OPEN) {
    ws.send(JSON.stringify({ type, data }));
  }
}

function stripMultiplexHeaders(raw: string): string {
  const buf = Buffer.from(raw, "utf-8");
  const firstByte = buf[0];

  if ((firstByte === 1 || firstByte === 2) && buf.length >= 8) {
    const parts: string[] = [];
    let offset = 0;
    while (offset < buf.length - 7) {
      const frameSize = buf.readUInt32BE(offset + 4);
      if (offset + 8 + frameSize > buf.length) break;
      parts.push(buf.subarray(offset + 8, offset + 8 + frameSize).toString("utf-8"));
      offset += 8 + frameSize;
    }
    if (parts.length > 0) return parts.join("");
  }

  return raw;
}
