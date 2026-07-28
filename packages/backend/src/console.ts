import type { IncomingMessage } from "node:http";
import { PassThrough } from "node:stream";
import type Docker from "dockerode";
import type { WebSocket } from "ws";
import {
  getContainer,
  getDockerInstance,
  getManagedContainer,
} from "./docker.js";
import { resolveGameConsoleAdapter } from "./game-console.js";
import { executeGameCommand } from "./game-console-runtime.js";
import type { WebSocketAuth } from "./auth.js";
import { writeAuditLog } from "./database.js";

export type ConsoleMode = "game" | "shell";

export async function handleConsoleConnection(
  ws: WebSocket,
  req: IncomingMessage,
  auth: WebSocketAuth,
  mode: ConsoleMode
): Promise<void> {
  const user = auth.user;
  const url = new URL(req.url || "", `http://${req.headers.host}`);
  const containerId = url.pathname.split("/").filter(Boolean).at(-1);
  const pendingMessages: string[] = [];
  let processMessage: ((raw: string) => Promise<void>) | null = null;
  let processingMessages = false;

  async function drainMessages() {
    if (processingMessages || !processMessage) return;
    processingMessages = true;
    try {
      while (pendingMessages.length > 0 && processMessage) {
        await processMessage(pendingMessages.shift()!);
      }
    } finally {
      processingMessages = false;
    }
  }

  ws.on("message", (raw) => {
    const message = raw.toString();
    if (pendingMessages.length < 10) {
      pendingMessages.push(message);
      void drainMessages();
    } else {
      sendMessage(ws, "error", "Too many commands queued");
    }
  });

  if (!containerId) {
    sendMessage(ws, "error", "Missing container ID");
    ws.close(1008, "Missing container ID");
    return;
  }
  if (mode === "shell" && user.role !== "admin") {
    sendMessage(ws, "error", "Container shell access requires an administrator");
    ws.close(1008, "Administrator access required");
    return;
  }

  let server;
  try {
    server = await getManagedContainer(containerId);
  } catch {
    sendMessage(ws, "error", `Container ${containerId} is not managed`);
    ws.close(1008, "Container not managed");
    return;
  }

  const container = getContainer(containerId);
  const adapter = resolveGameConsoleAdapter(server);
  sendMessage(
    ws,
    "system",
    mode === "game"
      ? adapter
        ? `Connected to ${adapter.name}`
        : "No game console adapter is configured; showing logs only"
      : `Administrator shell connected to ${containerId.substring(0, 12)}`
  );
  writeAuditLog({
    userId: user.id === "api-token" ? undefined : user.id,
    action:
      mode === "game"
        ? "container.game-console.opened"
        : "container.shell.opened",
    targetType: "container",
    targetId: containerId,
    ipAddress: req.socket.remoteAddress,
  });

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
    stream.on("data", (chunk: Buffer) => sendLogChunk(ws, chunk));
    stream.on("error", (error) => {
      sendMessage(ws, "error", `Log stream error: ${error.message}`);
    });
    stream.on("end", () => {
      sendMessage(ws, "system", "Log stream ended (container may have stopped)");
    });
  } catch (error: unknown) {
    sendMessage(ws, "error", `Failed to open logs: ${errorMessage(error)}`);
  }

  processMessage = async (raw: string) => {
    const currentUser = auth.validate();
    if (!currentUser) {
      ws.close(1008, "Session expired or access revoked");
      return;
    }
    auth.user = currentUser;
    if (mode === "shell" && currentUser.role !== "admin") {
      ws.close(1008, "Administrator access required");
      return;
    }

    let message: { type?: unknown; data?: unknown };
    try {
      message = JSON.parse(raw) as { type?: unknown; data?: unknown };
    } catch {
      sendMessage(ws, "error", "Invalid message format (expected JSON)");
      return;
    }
    if (message.type !== "input" || typeof message.data !== "string") {
      sendMessage(ws, "error", 'Expected { type: "input", data: "..." }');
      return;
    }

    const command = message.data.trim();
    if (!command) return;
    if (mode === "game" && currentUser.role === "viewer") {
      sendMessage(ws, "error", "Your account has read-only log access");
      return;
    }
    if (mode === "game" && !adapter) {
      sendMessage(ws, "error", "This server has no game console adapter");
      return;
    }
    const limit = mode === "game" ? 1024 : 4096;
    if (command.length > limit) {
      sendMessage(ws, "error", `Command exceeds the ${limit} character limit`);
      return;
    }

    try {
      writeAuditLog({
        userId: currentUser.id === "api-token" ? undefined : currentUser.id,
        action:
          mode === "game"
            ? "container.game-command.execute"
            : "container.shell.execute",
        targetType: "container",
        targetId: containerId,
        ipAddress: req.socket.remoteAddress,
      });
      if (mode === "game") {
        await executeGameCommand(container, server, adapter!, command, {
          stdout: (data) => sendMessage(ws, "stdout", data),
          stderr: (data) => sendMessage(ws, "stderr", data),
          system: (data) => sendMessage(ws, "system", data),
        });
      } else {
        await executeInContainer(
          container,
          {
            Cmd: ["/bin/sh", "-c", command],
            AttachStdout: true,
            AttachStderr: true,
            Tty: false,
          },
          ws
        );
      }
    } catch (error: unknown) {
      const prefix =
        mode === "game" ? "Game command failed" : "Shell command failed";
      sendMessage(ws, "error", `${prefix}: ${errorMessage(error)}`);
    }
  };

  void drainMessages();

  const destroyLogStream = () => {
    if (logStream) {
      (logStream as NodeJS.ReadableStream & { destroy?: () => void }).destroy?.();
      logStream = null;
    }
  };
  ws.on("close", destroyLogStream);
  ws.on("error", (error) => {
    console.error(`[Console] WebSocket error for ${containerId}:`, error.message);
    destroyLogStream();
  });
}

async function executeInContainer(
  container: Docker.Container,
  options: Docker.ExecCreateOptions,
  ws: WebSocket
): Promise<void> {
  const exec = await container.exec(options);
  const execStream = await exec.start({ hijack: true, stdin: false });
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  stdout.on("data", (chunk: Buffer) => sendMessage(ws, "stdout", chunk.toString()));
  stderr.on("data", (chunk: Buffer) => sendMessage(ws, "stderr", chunk.toString()));
  execStream.on("error", (error: Error) => {
    sendMessage(ws, "error", `Command stream error: ${error.message}`);
  });
  getDockerInstance().modem.demuxStream(execStream, stdout, stderr);
}

function sendLogChunk(ws: WebSocket, chunk: Buffer): void {
  if (ws.readyState !== ws.OPEN) return;
  const firstByte = chunk[0];
  if ((firstByte === 1 || firstByte === 2) && chunk.length >= 8) {
    let offset = 0;
    while (offset <= chunk.length - 8) {
      const frameSize = chunk.readUInt32BE(offset + 4);
      if (offset + 8 + frameSize > chunk.length) break;
      const payload = chunk.subarray(offset + 8, offset + 8 + frameSize);
      sendMessage(ws, chunk[offset] === 2 ? "stderr" : "stdout", payload.toString());
      offset += 8 + frameSize;
    }
    return;
  }
  sendMessage(ws, "stdout", chunk.toString());
}

function sendMessage(
  ws: WebSocket,
  type: "stdout" | "stderr" | "system" | "error",
  data: string
): void {
  if (ws.readyState === ws.OPEN) ws.send(JSON.stringify({ type, data }));
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
