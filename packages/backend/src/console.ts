import { PassThrough } from "node:stream";
import type Docker from "dockerode";
import type { SocketChannel } from "./socket-channel.js";
import { getContainer, getDockerInstance } from "./docker.js";
import { resolveGameConsoleAdapter } from "./game-console.js";
import { rawDataToString } from "./ws-message.js";
import {
  executeGameCommand,
  type GameCommandOutput,
} from "./game-console-runtime.js";
import type { WebSocketAuth } from "./auth.js";
import { writeAuditLog } from "./database.js";
import { hasServerCapability } from "./authorization.js";
import { authorizeServerSocket } from "./server-socket-access.js";
import { withLocks } from "./operation-locks.js";
import { createLogger } from "./logger.js";
import { DockerLogDecoder } from "./container-logs.js";
import {
  ConsoleOutputRedactor,
  observationSecrets,
} from "./console-redaction.js";

export type ConsoleMode = "game" | "shell";
const logger = createLogger("console");
const MAX_PENDING_MESSAGES = 10;

export async function handleConsoleConnection(
  ws: SocketChannel,
  req: Request,
  auth: WebSocketAuth,
  mode: ConsoleMode,
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
    access = await authorizeServerSocket(
      ws,
      auth,
      serverId,
      mode === "shell" ? "console.shell" : "console.execute",
    );
  } catch {
    sendMessage(
      ws,
      "error",
      "Server console is unavailable or access was denied",
    );
    ws.close(1008, "Server access unavailable");
    return;
  }
  if (!access.allowed()) return;
  const adapter = resolveGameConsoleAdapter(access.context.container);
  if (mode === "game" && !adapter) {
    sendMessage(
      ws,
      "error",
      "This server has no supported game console adapter",
    );
    ws.close(1008, "Console unavailable");
    return;
  }
  const containerId = access.context.logical.containerId;
  const secrets = observationSecrets(access.context.observation);
  const send = (
    type: "stdout" | "stderr" | "system" | "error",
    data: string,
  ) => {
    if (access.allowed()) sendMessage(ws, type, data);
  };
  send(
    "system",
    mode === "game"
      ? `Connected to ${adapter!.name}`
      : "Administrator shell. Commands require the container's timeout utility and run for at most 60 seconds.",
  );
  writeAuditLog({
    userId: auth.user.id === "api-token" ? undefined : auth.user.id,
    action:
      mode === "game" ? "server.game-console.opened" : "server.shell.opened",
    targetType: "server",
    targetId: serverId,
    details: {
      containerId,
      bindingRevision: access.context.logical.bindingRevision,
    },
    ipAddress: remoteAddress,
  });

  let logStream: (NodeJS.ReadableStream & { destroy?: () => void }) | null =
    null;
  const cleanup = () => {
    logStream?.destroy?.();
    logStream = null;
    pendingMessages.length = 0;
  };
  const pendingMessages: string[] = [];
  let processing = false;
  ws.onClose(cleanup);

  const execute = async (raw: string): Promise<void> => {
    if (!access.allowed()) return;
    let message: { type?: unknown; data?: unknown };
    try {
      message = JSON.parse(raw) as { type?: unknown; data?: unknown };
    } catch {
      send("error", "Invalid message format (expected JSON)");
      return;
    }
    if (message.type !== "input" || typeof message.data !== "string") {
      send("error", 'Expected { type: "input", data: "..." }');
      return;
    }
    const command = message.data.trim();
    if (!command) return;
    const limit = mode === "game" ? 1024 : 4096;
    if (command.length > limit) {
      send("error", `Command exceeds the ${limit} character limit`);
      return;
    }
    try {
      await withLocks(access.context.lockKeys, async () => {
        const current = await access.refresh();
        const assertAccess = () => {
          if (!access.allowed()) throw new Error("Console access changed");
        };
        const currentAdapter = resolveGameConsoleAdapter(current.container);
        if (mode === "game" && !currentAdapter)
          throw new Error("Console unavailable");
        const stdout = new ConsoleOutputRedactor(secrets, (value) =>
          send("stdout", value),
        );
        const stderr = new ConsoleOutputRedactor(secrets, (value) =>
          send("stderr", value),
        );
        const output: GameCommandOutput = {
          stdout: (value) => stdout.push(value),
          stderr: (value) => stderr.push(value),
          system: (value) => {
            const redactor = new ConsoleOutputRedactor(secrets, (safe) =>
              send("system", safe),
            );
            redactor.push(value);
            redactor.end();
          },
        };
        const actorId = auth.user.id === "api-token" ? undefined : auth.user.id;
        writeAuditLog({
          userId: actorId,
          action:
            mode === "game"
              ? "server.game-command.started"
              : "server.shell.started",
          targetType: "server",
          targetId: serverId,
          details: {
            containerId,
            bindingRevision: current.logical.bindingRevision,
          },
          ipAddress: remoteAddress,
        });
        try {
          if (mode === "game")
            await executeGameCommand(
              getContainer(containerId),
              current.container,
              currentAdapter!,
              command,
              output,
              assertAccess,
            );
          else await executeShell(getContainer(containerId), command, output, assertAccess);
          writeAuditLog({
            userId: actorId,
            action:
              mode === "game"
                ? "server.game-command.succeeded"
                : "server.shell.succeeded",
            targetType: "server",
            targetId: serverId,
          });
        } catch {
          writeAuditLog({
            userId: actorId,
            action:
              mode === "game"
                ? "server.game-command.failed"
                : "server.shell.failed",
            targetType: "server",
            targetId: serverId,
          });
          throw new Error("Console command failed");
        } finally {
          stdout.end();
          stderr.end();
        }
      });
    } catch {
      logger.warn("Console command could not complete", { serverId, mode });
      send(
        "error",
        mode === "game"
          ? "Game command failed or a conflicting operation is running"
          : "Shell command failed. Ensure timeout is installed; commands are limited to 60 seconds.",
      );
    }
  };
  const drain = async () => {
    if (processing) return;
    processing = true;
    try {
      while (pendingMessages.length && access.allowed())
        await execute(pendingMessages.shift()!);
    } finally {
      processing = false;
    }
  };
  ws.onMessage((raw) => {
    if (!access.allowed()) return;
    if (pendingMessages.length >= MAX_PENDING_MESSAGES) {
      send("error", "Too many commands queued");
      return;
    }
    pendingMessages.push(rawDataToString(raw));
    void drain();
  });

  // A console command grant never grants general log reading, including the
  // initial history traditionally shown in game consoles.
  if (hasServerCapability(auth.user, serverId, "logs.read")) {
    const logOutput = (type: "stdout" | "stderr", value: string) => {
      if (access.allowed("logs.read")) sendMessage(ws, type, value);
    };
    const stdout = new ConsoleOutputRedactor(secrets, (value) =>
      logOutput("stdout", value),
    );
    const stderr = new ConsoleOutputRedactor(secrets, (value) =>
      logOutput("stderr", value),
    );
    const decoder = new DockerLogDecoder((type, value) =>
      (type === "stdout" ? stdout : stderr).push(value),
    );
    try {
      logStream = await getContainer(containerId).logs({
        follow: true,
        stdout: true,
        stderr: true,
        tail: 200,
        timestamps: false,
      });
      if (!access.allowed("logs.read")) {
        cleanup();
        return;
      }
      logStream.on("data", (chunk: Buffer) => decoder.push(chunk));
      logStream.on("error", () => send("error", "Docker log stream failed"));
      logStream.on("end", () => {
        decoder.end();
        stdout.end();
        stderr.end();
        send("system", "Log stream ended");
      });
    } catch {
      send("error", "Failed to open Docker logs");
    }
  }
}

async function executeShell(
  container: Docker.Container,
  command: string,
  output: GameCommandOutput,
  assertAccess: () => void,
): Promise<void> {
  // Run the timeout inside the managed container. Closing a Docker exec stream
  // alone does not terminate its process and must never be used as a substitute.
  const exec = await container.exec({
    Cmd: ["timeout", "-k", "5", "60", "/bin/sh", "-c", command],
    AttachStdout: true,
    AttachStderr: true,
    Tty: false,
  });
  assertAccess();
  const stream = await exec.start({ hijack: true, stdin: false });
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  stdout.setEncoding("utf8");
  stderr.setEncoding("utf8");
  stdout.on("data", (chunk: string) => output.stdout(chunk));
  stderr.on("data", (chunk: string) => output.stderr(chunk));
  try {
    await new Promise<void>((resolve, reject) => {
      stream.once("end", resolve);
      stream.once("close", resolve);
      stream.once("error", () => reject(new Error("Shell stream failed")));
      getDockerInstance().modem.demuxStream(stream, stdout, stderr);
    });
  } finally {
    stdout.end();
    stderr.end();
  }
  const result = await exec.inspect();
  if (result.Running || result.ExitCode !== 0)
    throw new Error("Shell command failed or timed out");
}

function sendMessage(
  ws: SocketChannel,
  type: "stdout" | "stderr" | "system" | "error",
  data: string,
): void {
  if (ws.isOpen) ws.send(JSON.stringify({ type, data }));
}
