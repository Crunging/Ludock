import { demuxDockerStream } from "./docker-stream.js";
import type * as Docker from "./docker-client.js";
import { DockerApiError, DockerStreamOpenError, type DockerConnection } from "./docker-transport.js";
import {
  LABEL_CONSOLE_PASSWORD_ENV,
  LABEL_CONSOLE_PORT,
  LABEL_CONSOLE_HOST,
  type GameConsoleAdapter,
} from "./game-console.js";
import type { ManagedContainer } from "./docker.js";
import { executeSourceRcon, executeRustWebRcon, executeTelnetCommand } from "./helpers/network-console.js";
import { executeNetworkConsoleHelper } from "./network-console-helper.js";

export const MAX_DOCKER_EXEC_OUTPUT_BYTES = 4 * 1024 * 1024;
const DOCKER_EXEC_TIMEOUT_SECONDS = 15;
const DOCKER_EXEC_KILL_GRACE_SECONDS = 3;
const DOCKER_EXEC_STREAM_GRACE_MS = 5_000;
const DOCKER_EXEC_CREATE_TIMEOUT_MS = 5_000;
const DOCKER_EXEC_INSPECT_TIMEOUT_MS = 5_000;
const STDIN_COMMAND_TIMEOUT_MS = 10_000;

// Docker has no API for signaling an individual exec process, and closing its
// hijacked stream does not stop it. Keep the watchdog inside the container so
// the command really terminates even if Ludock disconnects or is interrupted.
// User input remains a positional argument to "$@" and is never shell source.
const BOUNDED_DOCKER_EXEC_SCRIPT = String.raw`
control="$1"; deadline="$2"; grace="$3"; independent_timeout="$4"; shift 4
ready="$control/ready"; cancel="$control/cancel"; reason="$control/reason"
umask 077
if ! mkdir "$control" 2>/dev/null; then
  if [ "$independent_timeout" = 1 ]; then exec "$@"; fi
  exit 126
fi
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
  /** Flush redaction tails only after the entire output stream was received. */
  complete?(): void;
}

export async function executeShellCommand(
  container: Docker.Container,
  command: string,
  output: GameCommandOutput,
  assertAccess?: () => void,
  signal?: AbortSignal,
): Promise<void> {
  // timeout also signals the shell's process group. If control storage is
  // unavailable, this deadline still bounds execution while we retain the lock
  // until Docker confirms exit. Writable storage enables earlier cancellation.
  await executeInContainer(container, {
    Cmd: ["timeout", "-k", "5", "60", "/bin/sh", "-c", command],
    AttachStdout: true,
    AttachStderr: true,
    Tty: false,
  }, output, assertAccess, signal, { timeoutSeconds: 65, independentTimeout: true });
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
      break;
    case "source-rcon":
    case "rust-webrcon":
    case "telnet": {
      const target = await resolveNetworkTarget(container, server, adapter);
      assertAccess?.();
      if (signal?.aborted) throw new Error("Console command cancelled");
      const execute = adapter.transport === "source-rcon" ? executeSourceRcon
        : adapter.transport === "rust-webrcon" ? executeRustWebRcon : executeTelnetCommand;
      const response = target.host
        ? await execute(target.host, target.port, target.password, command, assertAccess, { signal })
        : await executeNetworkConsoleHelper(container.id, {
          transport: adapter.transport, port: target.port, password: target.password, command,
        }, assertAccess, signal);
      assertAccess?.();
      if (signal?.aborted) throw new Error("Console command cancelled");
      output.stdout(response || "Command completed with no response");
      break;
    }
  }
  output.complete?.();
}

async function resolveNetworkTarget(
  container: Docker.Container,
  server: Pick<ManagedContainer, "labels">,
  adapter: GameConsoleAdapter
): Promise<{ host?: string; port: number; password: string }> {
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
  return { host: configuredHost || undefined, port, password };
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
  { timeoutSeconds = DOCKER_EXEC_TIMEOUT_SECONDS, independentTimeout = false }: {
    timeoutSeconds?: number;
    /** Only commands already bounded by timeout may run without control files. */
    independentTimeout?: boolean;
  } = {},
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
      String(timeoutSeconds),
      String(DOCKER_EXEC_KILL_GRACE_SECONDS),
      independentTimeout ? "1" : "0",
      ...options.Cmd,
    ],
  }, signal);
  assertAccess?.();
  if (signal?.aborted) throw new Error("Console command cancelled");
  // Starting is a mutation: an HTTP timeout cannot prove Docker rejected it.
  // Keep the caller's lock until the outcome and subsequent cleanup are known.
  let cancellationRequested = false;
  let cancelled = false;
  let outputLimited = false;
  let forcedDeadline = false;
  const requestCancellation = () => {
    if (cancellationRequested) return;
    cancellationRequested = true;
    void cancelDockerExec(container, controlPath, options.User).catch(() => {
      cancellationRequested = false;
    });
  };
  let stream: DockerConnection;
  try {
    stream = await exec.start();
  } catch (error) {
    // HTTP refusals precede process start; zero transmitted bytes also prove
    // that Docker could not have accepted this request.
    if (error instanceof DockerApiError ||
        (error instanceof DockerStreamOpenError && !error.requestStarted)) throw error;
    // A rejected start can still have reached Docker. Keep the server locked
    // until the daemon confirms a terminal outcome, including delayed startup.
    requestCancellation();
    await waitForDockerExecExit(exec, requestCancellation);
    throw error;
  }
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
    complete: () => { if (!cancelled) output.complete?.(); },
  };
  const hardDeadline = setTimeout(() => {
    forcedDeadline = true;
    requestCancellation();
    stream.abort(
      new Error("Game console command timed out"),
    );
  },
  (timeoutSeconds + DOCKER_EXEC_KILL_GRACE_SECONDS) * 1000 +
    DOCKER_EXEC_STREAM_GRACE_MS);
  hardDeadline.unref();
  let streamFailure: Error | undefined;
  try {
    await streamExecOutput(stream.readable, guardedOutput, () => {
      outputLimited = true;
      requestCancellation();
    });
  } catch (error) {
    streamFailure = error instanceof Error ? error : new Error("Game console stream failed");
    requestCancellation();
  } finally {
    clearTimeout(hardDeadline);
    signal?.removeEventListener("abort", onAbort);
  }

  // Stream EOF/error does not prove the exec exited. Retain the caller's lock
  // during cancellation and daemon outages until an inspect confirms exit.
  const result = await waitForDockerExecExit(exec, requestCancellation);
  if (outputLimited)
    throw new Error("Game console output exceeded its limit");
  if (cancelled) throw new Error("Console command cancelled");
  if (forcedDeadline) throw new Error("Game console command timed out");
  if (streamFailure) throw streamFailure;
  if (!result) throw new Error("Game console process is no longer available");
  if (result.ExitCode === 124)
    throw new Error("Game console command timed out");
  if (result.ExitCode === 125)
    throw new Error("Console command cancelled");
  if (result.Running || result.ExitCode !== 0)
    throw new Error("Game console command failed");
}

async function waitForDockerExecExit(
  exec: Docker.Exec,
  requestCancellation: () => void,
): Promise<Docker.ExecInspectInfo | null> {
  while (true) {
    try {
      const result = await inspectDockerExec(exec);
      // A newly prepared exec is also non-running, but has no exit code yet.
      if (result.Running === false && Number.isInteger(result.ExitCode)) return result;
    } catch (error) {
      // Docker removes exec records when the container/daemon is replaced.
      if (error instanceof DockerApiError && error.statusCode === 404) return null;
    }
    requestCancellation();
    await new Promise<void>((resolve) => setTimeout(resolve, 250));
  }
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
  const stream = await cancellation.start();
  const timeout = setTimeout(() => stream.abort(new Error("Console cancellation timed out")), 10_000);
  try { await demuxDockerStream(stream.readable); }
  finally { clearTimeout(timeout); stream.abort(); }
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
  let stream: DockerConnection | undefined;
  let timedOut = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const assertReady = () => {
    if (timedOut) throw new Error("Game console command timed out");
    assertAccess?.();
  };
  try {
    await Promise.race([
      (async () => {
        const info = await container.inspect();
        assertReady();
        if (!info.Config.OpenStdin)
          throw new Error("Container standard input is closed; recreate it with stdin_open: true");
        if (info.Config.StdinOnce)
          throw new Error("Container standard input closes after an attached client disconnects; recreate it with StdinOnce disabled");
        stream = await container.attach({ stream: true, stdin: true, stdout: false, stderr: false });
        try {
          assertReady();
          await writeAttachedInput(stream, `${command}\n`);
          output.system("Command sent to the server process");
        } finally { stream.abort(); }
      })(),
      new Promise<never>((_resolve, reject) => {
        timer = setTimeout(() => {
          timedOut = true;
          stream?.abort();
          reject(new Error("Game console command timed out"));
        }, STDIN_COMMAND_TIMEOUT_MS);
      }),
    ]);
  } finally {
    if (timer) clearTimeout(timer);
  }
}

async function writeAttachedInput(stream: DockerConnection, data: string): Promise<void> {
  const writer = stream.writable.getWriter();
  try { await writer.write(new TextEncoder().encode(data)); }
  finally { writer.releaseLock(); }
}

async function streamExecOutput(
  stream: ReadableStream<Uint8Array>,
  output: GameCommandOutput,
  onLimit: () => void,
): Promise<void> {
  const decoders = {
    stdout: new TextDecoder("utf-8", { ignoreBOM: true }),
    stderr: new TextDecoder("utf-8", { ignoreBOM: true }),
  };
  let receivedBytes = 0;
  let limited = false;
  const receive = (type: "stdout" | "stderr", chunk: Uint8Array) => {
    if (limited) return;
    receivedBytes += chunk.length;
    if (receivedBytes > MAX_DOCKER_EXEC_OUTPUT_BYTES) {
      limited = true;
      onLimit();
      return;
    }
    const value = decoders[type].decode(chunk, { stream: true });
    if (value) output[type](value);
  };
  await demuxDockerStream(stream, chunk => receive("stdout", chunk), chunk => receive("stderr", chunk));
  if (!limited) {
    const finalStdout = decoders.stdout.decode();
    const finalStderr = decoders.stderr.decode();
    if (finalStdout) output.stdout(finalStdout);
    if (finalStderr) output.stderr(finalStderr);
    output.complete?.();
  }
}

function isValidConsoleHost(host: string): boolean {
  return (
    host.length <= 253 &&
    !host.includes("://") &&
    !/[/\\\s@?#]/.test(host) &&
    /^[A-Za-z0-9_.:[\]-]+$/.test(host)
  );
}
