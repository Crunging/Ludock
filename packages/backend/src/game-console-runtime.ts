import { PassThrough } from "node:stream";
import { StringDecoder } from "node:string_decoder";
import type Docker from "dockerode";
import { getDockerInstance } from "./docker.js";
import {
  LABEL_CONSOLE_PASSWORD_ENV,
  LABEL_CONSOLE_PORT,
  LABEL_CONSOLE_HOST,
  type GameConsoleAdapter,
} from "./game-console.js";
import type { ManagedContainer } from "./docker.js";
import { rawDataToString } from "./ws-message.js";

const CONNECT_TIMEOUT_MS = 5_000;
const COMMAND_TIMEOUT_MS = 10_000;
const MAX_RCON_PACKET_SIZE = 4 * 1024 * 1024;
const MAX_TCP_RECEIVED_BYTES = MAX_RCON_PACKET_SIZE + 64 * 1024;
export const MAX_DOCKER_EXEC_OUTPUT_BYTES = 4 * 1024 * 1024;
const DOCKER_EXEC_TIMEOUT_SECONDS = 15;
const DOCKER_EXEC_KILL_GRACE_SECONDS = 3;
const DOCKER_EXEC_STREAM_GRACE_MS = 5_000;
const DOCKER_EXEC_CREATE_TIMEOUT_MS = 5_000;
const DOCKER_EXEC_INSPECT_TIMEOUT_MS = 5_000;

// Docker has no API for signaling an individual exec process, and closing its
// hijacked stream does not stop it. Keep the watchdog inside the container so
// the command really terminates even if Ludock disconnects or is interrupted.
// User input remains a positional argument to "$@" and is never shell source.
const BOUNDED_DOCKER_EXEC_SCRIPT = String.raw`
control="$1"; deadline="$2"; grace="$3"; shift 3
ready="$control/ready"; cancel="$control/cancel"; reason="$control/reason"
umask 077
if ! mkdir "$control"; then exit 126; fi
cleanup() {
  rm -f "$ready" "$cancel" "$reason"
  rmdir "$control" 2>/dev/null || true
}
if ! : > "$ready"; then cleanup; exit 126; fi
if [ -e "$cancel" ]; then cleanup; exit 125; fi
"$@" &
child=$!
(
  remaining="$deadline"
  while [ "$remaining" -gt 0 ] && [ ! -e "$cancel" ]; do
    sleep 1
    remaining=$((remaining - 1))
  done
  if [ -e "$cancel" ]; then
    printf '%s\n' cancelled > "$reason"
  else
    printf '%s\n' timeout > "$reason"
  fi
  kill -TERM "$child" 2>/dev/null || exit 0
  sleep "$grace"
  kill -KILL "$child" 2>/dev/null || true
) &
watchdog=$!
wait "$child"
status=$?
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
cause=""
if [ -r "$reason" ]; then IFS= read -r cause < "$reason" || true; fi
cleanup
if [ "$cause" = timeout ]; then exit 124; fi
if [ "$cause" = cancelled ]; then exit 125; fi
exit "$status"
`;

export interface GameCommandOutput {
  stdout(data: string): void;
  stderr(data: string): void;
  system(data: string): void;
}

export async function executeGameCommand(
  container: Docker.Container,
  server: Pick<ManagedContainer, "labels" | "state">,
  adapter: GameConsoleAdapter,
  command: string,
  output: GameCommandOutput,
  assertAccess?: () => void,
  signal?: AbortSignal,
): Promise<void> {
  assertAccess?.();
  if (server.state !== "running") {
    throw new Error("The game server must be running to accept console commands");
  }

  switch (adapter.transport) {
    case "docker-exec": {
      if (!adapter.createExecOptions) {
        throw new Error("The console adapter is missing its command configuration");
      }
      await executeInContainer(
        container,
        adapter.createExecOptions(command),
        output,
        assertAccess,
        signal,
      );
      return;
    }
    case "container-stdin":
      await writeContainerStdin(container, command, output, assertAccess);
      return;
    case "source-rcon": {
      const target = await resolveNetworkTarget(container, server, adapter);
      const response = await executeSourceRcon(
        target.host,
        target.port,
        target.password,
        command,
        assertAccess,
      );
      output.stdout(response || "Command completed with no response");
      return;
    }
    case "rust-webrcon": {
      const target = await resolveNetworkTarget(container, server, adapter);
      const response = await executeRustWebRcon(
        target.host,
        target.port,
        target.password,
        command,
        assertAccess,
      );
      output.stdout(response || "Command completed with no response");
      return;
    }
    case "telnet": {
      const target = await resolveNetworkTarget(container, server, adapter);
      const response = await executeTelnetCommand(
        target.host,
        target.port,
        target.password,
        command,
        assertAccess,
      );
      output.stdout(response || "Command completed with no response");
    }
  }
}

export async function executeSourceRcon(
  host: string,
  port: number,
  password: string,
  command: string,
  assertAccess?: () => void,
): Promise<string> {
  assertAccess?.();
  const authId = randomRequestId();
  const commandId = randomRequestId();
  let buffer: Buffer<ArrayBufferLike> = Buffer.alloc(0);
  let authenticated = false;
  let commandSent = false;
  const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
  let response = "";
  let responseBytes = 0;
  let receivedResponse = false;
  let responseTimer: ReturnType<typeof setTimeout> | undefined;

  return runConsoleTcp({
    host, port, assertAccess,
    connectTimeoutMessage: "RCON connection timed out",
    commandTimeoutMessage: "RCON connection timed out",
    connectionErrorMessage: "Could not connect to the server's RCON endpoint",
    responseLimitMessage: "RCON response is too large",
    open(tcp) {
      tcp.write(encodeRconPacket(authId, 3, password));
    },
    data(tcp, chunk) {
      buffer = Buffer.concat([buffer, chunk]);
      const decoded = decodeRconPackets(buffer);
      buffer = decoded.remaining;
      for (const packet of decoded.packets) {
        if (tcp.settled) return;
        if (!authenticated) {
          if (packet.id === -1) {
            tcp.finish(new Error("RCON authentication failed"));
            return;
          }
          if (packet.id === authId && packet.type === 2) {
            authenticated = true;
            tcp.write(encodeRconPacket(commandId, 2, command), () => { commandSent = true; });
          }
          continue;
        }
        if (!commandSent || packet.id !== commandId) continue;
        receivedResponse = true;
        responseBytes += packet.body.length;
        if (responseBytes > MAX_RCON_PACKET_SIZE) {
          tcp.finish(new Error("RCON response is too large"));
          return;
        }
        response += decoder.decode(packet.body, { stream: true });
      }
      if (responseTimer) clearTimeout(responseTimer);
      if (receivedResponse && buffer.length === 0)
        responseTimer = setTimeout(() => tcp.finish(undefined, response + decoder.decode()), 150);
    },
    end(tcp) {
      if (!authenticated) tcp.finish(new Error("RCON authentication did not complete"));
      else if (!commandSent) tcp.finish(new Error("RCON connection closed before the command was sent"));
      else if (buffer.length) tcp.finish(new Error("The RCON server returned an incomplete packet"));
      else tcp.finish(undefined, response + decoder.decode());
    },
    cleanup() { if (responseTimer) clearTimeout(responseTimer); },
  });
}

export async function executeRustWebRcon(
  host: string,
  port: number,
  password: string,
  command: string,
  assertAccess?: () => void,
): Promise<string> {
  assertAccess?.();
  return new Promise<string>((resolve, reject) => {
    const identifier = randomRequestId();
    const url = `ws://${formatHost(host)}:${port}/${encodeURIComponent(password)}`;
    const socket = new globalThis.WebSocket(url);
    socket.binaryType = "arraybuffer";
    let settled = false;
    const timeout = setTimeout(() => {
      finish(new Error("WebRCON command timed out"));
    }, COMMAND_TIMEOUT_MS);
    const handshakeTimeout = setTimeout(() => {
      finish(new Error("WebRCON connection timed out"));
    }, CONNECT_TIMEOUT_MS);

    const finish = (error?: Error, response = "") => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      clearTimeout(handshakeTimeout);
      if (error) socket.terminate();
      else socket.close();
      if (error) reject(error);
      else resolve(response);
    };

    socket.addEventListener("open", () => {
      clearTimeout(handshakeTimeout);
      if (settled) return;
      try {
        assertAccess?.();
        socket.send(
          JSON.stringify({
            Identifier: identifier,
            Message: command,
            Name: "Ludock",
          }),
        );
      } catch {
        finish(new Error("Console access changed"));
      }
    });
    socket.addEventListener("message", (event) => {
      if (settled) return;
      try {
        const raw: unknown = event.data;
        if (typeof raw !== "string" && !(raw instanceof ArrayBuffer))
          throw new Error("Unsupported WebRCON message");
        if ((typeof raw === "string" ? Buffer.byteLength(raw) : raw.byteLength) > MAX_RCON_PACKET_SIZE)
          throw new Error("WebRCON response is too large");
        const message = JSON.parse(rawDataToString(raw)) as {
          Identifier?: unknown;
          Message?: unknown;
        };
        if (message.Identifier !== identifier) return;
        finish(
          undefined,
          typeof message.Message === "string"
            ? message.Message
            : JSON.stringify(message.Message ?? "")
        );
      } catch {
        finish(new Error("The WebRCON server returned an invalid response"));
      }
    });
    socket.addEventListener("error", () => {
      finish(new Error("Could not connect or authenticate with Rust WebRCON"));
    });
    socket.addEventListener("close", () => finish(
      new Error("WebRCON connection closed before the command response"),
    ));
  });
}

export async function executeTelnetCommand(
  host: string,
  port: number,
  password: string,
  command: string,
  assertAccess?: () => void,
): Promise<string> {
  assertAccess?.();
  let output = "";
  const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
  let passwordSent = false;
  let commandSent = false;
  let quietTimer: ReturnType<typeof setTimeout> | undefined;
  let commandTimer: ReturnType<typeof setTimeout> | undefined;
  const telnet = createTelnetDecoder();
  const finishResponse = (tcp: ConsoleTcpSession) => {
    tcp.finish(undefined, cleanTelnetOutput(output + decoder.decode(), command));
  };

  return runConsoleTcp({
    host, port, assertAccess,
    connectTimeoutMessage: "Telnet console connection timed out",
    commandTimeoutMessage: "Telnet console command timed out",
    connectionErrorMessage: "Could not connect to the server's Telnet console",
    responseLimitMessage: "Telnet console response is too large",
    data(tcp, chunk) {
      output += decoder.decode(telnet(chunk, tcp), { stream: true });
      if (tcp.settled) return;
      if (!passwordSent && /password\s*[:>]?/i.test(output)) {
        passwordSent = true;
        tcp.write(Buffer.from(`${password}\n`), () => {
          commandTimer = setTimeout(() => {
            if (tcp.settled) return;
            tcp.write(Buffer.from(`${command}\n`), () => {
              commandSent = true;
              quietTimer = setTimeout(() => finishResponse(tcp), 250);
            });
          }, 50);
        });
        return;
      }
      if (passwordSent) {
        if (/incorrect|invalid password|authentication failed/i.test(output)) {
          tcp.finish(new Error("Telnet console authentication failed"));
          return;
        }
        if (commandSent) {
          if (quietTimer) clearTimeout(quietTimer);
          quietTimer = setTimeout(() => finishResponse(tcp), 250);
        }
      }
    },
    end(tcp) {
      if (commandSent) finishResponse(tcp);
      else tcp.finish(new Error("Telnet console closed before the command was sent"));
    },
    cleanup() {
      if (quietTimer) clearTimeout(quietTimer);
      if (commandTimer) clearTimeout(commandTimer);
    },
  });
}

interface ConsoleTcpSession {
  readonly settled: boolean;
  write(data: Buffer, written?: () => void): void;
  finish(error?: Error, response?: string): void;
}

function runConsoleTcp(options: {
  host: string;
  port: number;
  assertAccess?: () => void;
  connectTimeoutMessage: string;
  commandTimeoutMessage: string;
  connectionErrorMessage: string;
  responseLimitMessage: string;
  open?(tcp: ConsoleTcpSession): void;
  data(tcp: ConsoleTcpSession, chunk: Buffer): void;
  end(tcp: ConsoleTcpSession): void;
  cleanup(): void;
}): Promise<string> {
  return new Promise((resolve, reject) => {
    let socket: Bun.Socket | undefined;
    let settled = false;
    let receivedBytes = 0;
    let queuedBytes = 0;
    let draining = false;
    let blocked = false;
    const writes: Array<{ data: Buffer; offset: number; written?: () => void }> = [];
    const connectTimer = setTimeout(() => {
      tcp.finish(new Error(options.connectTimeoutMessage));
    }, CONNECT_TIMEOUT_MS);
    const commandTimer = setTimeout(() => {
      tcp.finish(new Error(options.commandTimeoutMessage));
    }, COMMAND_TIMEOUT_MS);
    const tcp: ConsoleTcpSession = {
      get settled() { return settled; },
      finish(error, response = "") {
        if (settled) return;
        settled = true;
        clearTimeout(connectTimer);
        clearTimeout(commandTimer);
        options.cleanup();
        writes.length = 0;
        queuedBytes = 0;
        socket?.terminate();
        if (error) reject(error);
        else resolve(response);
      },
      write(data, written) {
        if (settled) return;
        if (queuedBytes + data.length > MAX_RCON_PACKET_SIZE + 4) {
          tcp.finish(new Error("Console command or credential is too large"));
          return;
        }
        writes.push({ data, offset: 0, written });
        queuedBytes += data.length;
        drainWrites();
      },
    };
    const invoke = (callback: () => void) => {
      if (settled) return;
      try { callback(); }
      catch (error) {
        tcp.finish(error instanceof Error ? error : new Error("The console server returned an invalid response"));
      }
    };
    const drainWrites = () => {
      if (!socket || settled || draining || blocked) return;
      draining = true;
      try {
        while (writes.length && !settled) {
          const next = writes[0];
          try { options.assertAccess?.(); }
          catch {
            tcp.finish(new Error("Console access changed"));
            return;
          }
          let count: number;
          try { count = socket.write(next.data, next.offset, next.data.length - next.offset); }
          catch {
            tcp.finish(new Error(options.connectionErrorMessage));
            return;
          }
          if (count < 0) {
            tcp.finish(new Error(options.connectionErrorMessage));
            return;
          }
          next.offset += count;
          queuedBytes -= count;
          // Bun does not queue the remaining bytes; drain resumes this exact frame.
          if (next.offset < next.data.length) {
            blocked = true;
            return;
          }
          writes.shift();
          if (next.written) invoke(next.written);
        }
      } finally { draining = false; }
    };
    try {
      void Bun.connect({
        hostname: options.host,
        port: options.port,
        socket: {
          open(connected) {
            socket = connected;
            if (settled) { connected.terminate(); return; }
            clearTimeout(connectTimer);
            connected.setNoDelay(true);
            invoke(() => options.open?.(tcp));
          },
          data(_socket, chunk) {
            if (settled) return;
            receivedBytes += chunk.length;
            if (receivedBytes > MAX_TCP_RECEIVED_BYTES) {
              tcp.finish(new Error(options.responseLimitMessage));
              return;
            }
            invoke(() => options.data(tcp, chunk));
          },
          drain() { blocked = false; drainWrites(); },
          end() { invoke(() => options.end(tcp)); },
          close(_socket, error) {
            if (error) tcp.finish(new Error(options.connectionErrorMessage));
            else invoke(() => options.end(tcp));
          },
          error() { tcp.finish(new Error(options.connectionErrorMessage)); },
          connectError(failed) {
            if (settled) { failed.terminate(); return; }
            socket = failed;
            tcp.finish(new Error(options.connectionErrorMessage));
          },
        },
      }).then((connected) => {
        // A deadline may expire while DNS/connect is still pending.
        if (settled) connected.terminate();
      }, () => tcp.finish(new Error(options.connectionErrorMessage)));
    } catch {
      tcp.finish(new Error(options.connectionErrorMessage));
    }
  });
}

async function resolveNetworkTarget(
  container: Docker.Container,
  server: Pick<ManagedContainer, "labels">,
  adapter: GameConsoleAdapter
): Promise<{ host: string; port: number; password: string }> {
  const info = await container.inspect();
  const env = parseEnvironment(info.Config.Env || []);
  const port = resolvePort(server.labels[LABEL_CONSOLE_PORT], env, adapter);
  const passwordEnvName = resolvePasswordEnvName(
    server.labels[LABEL_CONSOLE_PASSWORD_ENV],
    env,
    adapter
  );
  const password = env[passwordEnvName];
  if (!password) {
    throw new Error(
      `Console credential environment variable ${passwordEnvName} is empty or unavailable`
    );
  }

  const configuredHost = server.labels[LABEL_CONSOLE_HOST]?.trim();
  if (configuredHost && !isValidConsoleHost(configuredHost)) {
    throw new Error(
      `${LABEL_CONSOLE_HOST} must be an IP address or plain hostname`
    );
  }
  const network = Object.values(info.NetworkSettings.Networks || {}).find(
    (candidate) => candidate.IPAddress
  );
  const host = configuredHost || network?.IPAddress;
  if (!host) {
    throw new Error(
      `The container has no Docker network address; configure ${LABEL_CONSOLE_HOST}`
    );
  }
  return { host, port, password };
}

function resolvePort(
  configuredPort: string | undefined,
  env: Record<string, string>,
  adapter: GameConsoleAdapter
): number {
  const raw = configuredPort || env.RCON_PORT;
  const port = raw ? Number(raw) : adapter.defaultPort;
  if (!Number.isInteger(port) || !port || port < 1 || port > 65_535) {
    throw new Error(
      `Set ${LABEL_CONSOLE_PORT} to the container's console port (1-65535)`
    );
  }
  return port;
}

function resolvePasswordEnvName(
  configuredName: string | undefined,
  env: Record<string, string>,
  adapter: GameConsoleAdapter
): string {
  if (configuredName) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(configuredName)) {
      throw new Error(`${LABEL_CONSOLE_PASSWORD_ENV} is not a valid environment name`);
    }
    return configuredName;
  }
  const inferred = adapter.passwordEnvCandidates?.find((name) => env[name]);
  if (!inferred) {
    throw new Error(
      `Set ${LABEL_CONSOLE_PASSWORD_ENV} to the name of the container environment variable that holds its console password`
    );
  }
  return inferred;
}

function parseEnvironment(values: string[]): Record<string, string> {
  return Object.fromEntries(
    values.map((value) => {
      const separator = value.indexOf("=");
      return separator === -1
        ? [value, ""]
        : [value.slice(0, separator), value.slice(separator + 1)];
    })
  );
}

async function executeInContainer(
  container: Docker.Container,
  options: Docker.ExecCreateOptions,
  output: GameCommandOutput,
  assertAccess?: () => void,
  signal?: AbortSignal,
): Promise<void> {
  if (!options.Cmd?.length)
    throw new Error("The console adapter is missing its command configuration");
  if (signal?.aborted) throw new Error("Console command cancelled");

  const controlPath = `/tmp/.ludock-console-${crypto.randomUUID()}`;
  const exec = await createDockerExec(container, {
    ...options,
    Cmd: [
      "/bin/sh",
      "-c",
      BOUNDED_DOCKER_EXEC_SCRIPT,
      "ludock-console",
      controlPath,
      String(DOCKER_EXEC_TIMEOUT_SECONDS),
      String(DOCKER_EXEC_KILL_GRACE_SECONDS),
      ...options.Cmd,
    ],
  }, signal);
  assertAccess?.();
  if (signal?.aborted) throw new Error("Console command cancelled");
  // Starting is a mutation: an HTTP timeout cannot prove Docker rejected it.
  // Keep the caller's lock until the outcome and subsequent cleanup are known.
  const stream = await exec.start({ hijack: true, stdin: false });
  let cancellationRequested = false;
  let cancelled = false;
  let outputLimited = false;
  let forcedDeadline = false;
  const requestCancellation = () => {
    if (cancellationRequested) return;
    cancellationRequested = true;
    void cancelDockerExec(container, controlPath, options.User).catch(() => {});
  };
  const onAbort = () => {
    cancelled = true;
    requestCancellation();
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  if (signal?.aborted) onAbort();

  const guardedOutput: GameCommandOutput = {
    stdout(value) {
      try {
        assertAccess?.();
        if (!cancelled) output.stdout(value);
      } catch {
        cancelled = true;
        requestCancellation();
      }
    },
    stderr(value) {
      try {
        assertAccess?.();
        if (!cancelled) output.stderr(value);
      } catch {
        cancelled = true;
        requestCancellation();
      }
    },
    system: (value) => output.system(value),
  };
  const hardDeadline = setTimeout(() => {
    forcedDeadline = true;
    requestCancellation();
    (stream as NodeJS.ReadWriteStream & { destroy(error?: Error): void }).destroy(
      new Error("Game console command timed out"),
    );
  },
  (DOCKER_EXEC_TIMEOUT_SECONDS + DOCKER_EXEC_KILL_GRACE_SECONDS) * 1000 +
    DOCKER_EXEC_STREAM_GRACE_MS);
  hardDeadline.unref();
  let streamFailure: unknown;
  try {
    await streamExecOutput(stream, guardedOutput, () => {
      outputLimited = true;
      requestCancellation();
    });
  } catch (error) {
    streamFailure = error;
  } finally {
    clearTimeout(hardDeadline);
    signal?.removeEventListener("abort", onAbort);
  }

  if (outputLimited)
    throw new Error("Game console output exceeded its limit");
  if (cancelled) throw new Error("Console command cancelled");
  if (forcedDeadline) throw new Error("Game console command timed out");
  if (streamFailure !== undefined) {
    if (streamFailure instanceof Error) throw streamFailure;
    throw new Error("Game console stream failed");
  }
  const result = await inspectDockerExec(exec);
  if (result.ExitCode === 124)
    throw new Error("Game console command timed out");
  if (result.ExitCode === 125)
    throw new Error("Console command cancelled");
  if (result.Running || result.ExitCode !== 0)
    throw new Error("Game console command failed");
}

async function createDockerExec(
  container: Docker.Container,
  options: Docker.ExecCreateOptions,
  signal?: AbortSignal,
): Promise<Docker.Exec> {
  if (signal?.aborted) throw new Error("Console command cancelled");
  let deadline: ReturnType<typeof setTimeout> | undefined;
  let onAbort: (() => void) | undefined;
  try {
    return await new Promise<Docker.Exec>((resolve, reject) => {
      onAbort = () => reject(new Error("Console command cancelled"));
      signal?.addEventListener("abort", onAbort, { once: true });
      deadline = setTimeout(
        () => reject(new Error("Game console preparation timed out")),
        DOCKER_EXEC_CREATE_TIMEOUT_MS,
      );
      deadline.unref();
      // Creation only allocates an exec configuration. A late result is ignored
      // and can never proceed to start after this promise has been rejected.
      void container.exec(options).then(resolve, reject);
    });
  } finally {
    if (deadline) clearTimeout(deadline);
    if (onAbort) signal?.removeEventListener("abort", onAbort);
  }
}

async function cancelDockerExec(
  container: Docker.Container,
  controlPath: string,
  user?: string,
): Promise<void> {
  const cancellation = await container.exec({
    Cmd: [
      "/bin/sh",
      "-c",
      'remaining=5; while [ "$remaining" -gt 0 ] && [ ! -r "$1/ready" ]; do sleep 1; remaining=$((remaining - 1)); done; if [ -r "$1/ready" ]; then umask 077; : > "$1/cancel"; fi',
      "ludock-console-cancel",
      controlPath,
    ],
    AttachStdout: false,
    AttachStderr: false,
    AttachStdin: false,
    Tty: false,
    ...(user ? { User: user } : {}),
  });
  const stream = await cancellation.start({ hijack: true, stdin: false });
  (stream as NodeJS.ReadableStream & { resume?: () => void }).resume?.();
}

async function inspectDockerExec(exec: Docker.Exec): Promise<Docker.ExecInspectInfo> {
  let deadline: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      exec.inspect(),
      new Promise<never>((_, reject) => {
        deadline = setTimeout(
          () => reject(new Error("Game console status check timed out")),
          DOCKER_EXEC_INSPECT_TIMEOUT_MS,
        );
        deadline.unref();
      }),
    ]);
  } finally {
    if (deadline) clearTimeout(deadline);
  }
}

async function writeContainerStdin(
  container: Docker.Container,
  command: string,
  output: GameCommandOutput,
  assertAccess?: () => void,
): Promise<void> {
  const info = await container.inspect();
  if (!info.Config.OpenStdin) {
    throw new Error(
      "Container standard input is closed; recreate it with stdin_open: true"
    );
  }
  if (info.Config.StdinOnce) {
    throw new Error(
      "Container standard input closes after an attached client disconnects; recreate it with StdinOnce disabled"
    );
  }

  const stream = await container.attach({
    stream: true,
    stdin: true,
    stdout: false,
    stderr: false,
    hijack: true,
  });
  try {
    assertAccess?.();
    await writeAttachedInput(stream, `${command}\n`);
  } finally {
    (stream as NodeJS.ReadWriteStream & { destroy(): void }).destroy();
  }
  output.system("Command sent to the server process");
}

async function writeAttachedInput(
  stream: NodeJS.ReadWriteStream,
  data: string
): Promise<void> {
  await new Promise<void>((resolve, reject) => {
    stream.write(data, (error) => {
      if (error) reject(error);
      else resolve();
    });
  });
}

async function streamExecOutput(
  stream: NodeJS.ReadWriteStream,
  output: GameCommandOutput,
  onLimit: () => void,
): Promise<void> {
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  const decoders = {
    stdout: new StringDecoder("utf8"),
    stderr: new StringDecoder("utf8"),
  };
  let receivedBytes = 0;
  let limited = false;
  const receive = (type: "stdout" | "stderr", chunk: Buffer) => {
    if (limited) return;
    receivedBytes += chunk.length;
    if (receivedBytes > MAX_DOCKER_EXEC_OUTPUT_BYTES) {
      limited = true;
      onLimit();
      return;
    }
    const value = decoders[type].write(chunk);
    if (value) output[type](value);
  };
  stdout.on("data", (chunk: Buffer) => receive("stdout", chunk));
  stderr.on("data", (chunk: Buffer) => receive("stderr", chunk));
  try {
    await new Promise<void>((resolve, reject) => {
      stream.once("end", resolve);
      stream.once("close", resolve);
      stream.once("error", reject);
      getDockerInstance().modem.demuxStream(stream, stdout, stderr);
    });
    if (!limited) {
      const finalStdout = decoders.stdout.end();
      const finalStderr = decoders.stderr.end();
      if (finalStdout) output.stdout(finalStdout);
      if (finalStderr) output.stderr(finalStderr);
    }
  } finally {
    stdout.end();
    stderr.end();
  }
}

function encodeRconPacket(id: number, type: number, body: string): Buffer {
  if (Buffer.byteLength(body, "utf8") > MAX_RCON_PACKET_SIZE - 10)
    throw new Error("RCON command or credential is too large");
  const payload = Buffer.from(body, "utf8");
  const packet = Buffer.alloc(payload.length + 14);
  packet.writeInt32LE(payload.length + 10, 0);
  packet.writeInt32LE(id, 4);
  packet.writeInt32LE(type, 8);
  payload.copy(packet, 12);
  return packet;
}

function decodeRconPackets(buffer: Buffer): {
  packets: Array<{ id: number; type: number; body: Buffer }>;
  remaining: Buffer;
} {
  const packets: Array<{ id: number; type: number; body: Buffer }> = [];
  let offset = 0;
  while (buffer.length - offset >= 4) {
    const size = buffer.readInt32LE(offset);
    if (size < 10 || size > MAX_RCON_PACKET_SIZE) {
      throw new Error("The RCON server returned an invalid packet");
    }
    if (buffer.length - offset < size + 4) break;
    const end = offset + size + 4;
    packets.push({
      id: buffer.readInt32LE(offset + 4),
      type: buffer.readInt32LE(offset + 8),
      body: buffer.subarray(offset + 12, end - 2),
    });
    offset = end;
  }
  return { packets, remaining: buffer.subarray(offset) };
}

function randomRequestId(): number {
  return Math.floor(Math.random() * 2_000_000_000) + 1;
}

function formatHost(host: string): string {
  return host.includes(":") ? `[${host}]` : host;
}

function isValidConsoleHost(host: string): boolean {
  return (
    host.length <= 253 &&
    !host.includes("://") &&
    !/[/\\\s@?#]/.test(host) &&
    /^[A-Za-z0-9_.:[\]-]+$/.test(host)
  );
}

function createTelnetDecoder(): (chunk: Buffer, tcp: ConsoleTcpSession) => Buffer {
  let state: "text" | "command" | "option" | "subnegotiation" | "subcommand" = "text";
  let command = 0;
  return (chunk, tcp) => {
    const output = Buffer.allocUnsafe(chunk.length);
    let length = 0;
    for (const byte of chunk) {
      if (tcp.settled) break;
      switch (state) {
        case "text":
          if (byte === 255) state = "command";
          else output[length++] = byte;
          break;
        case "command":
          if (byte === 255) {
            output[length++] = byte;
            state = "text";
          } else if (byte >= 251 && byte <= 254) {
            command = byte;
            state = "option";
          } else state = byte === 250 ? "subnegotiation" : "text";
          break;
        case "option":
          tcp.write(Buffer.from([255, command <= 252 ? 254 : 252, byte]));
          state = "text";
          break;
        case "subnegotiation":
          if (byte === 255) state = "subcommand";
          break;
        case "subcommand":
          state = byte === 240 ? "text" : "subnegotiation";
          break;
      }
    }
    return output.subarray(0, length);
  };
}

function cleanTelnetOutput(output: string, command: string): string {
  return output
    .replace(/.*password\s*[:>]?\s*/is, "")
    .replace(new RegExp(`^\\s*${escapeRegExp(command)}\\s*`, "i"), "")
    .trim();
}

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}
