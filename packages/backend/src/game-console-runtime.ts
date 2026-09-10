import net from "node:net";
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
        container, adapter.createExecOptions(command), output, assertAccess,
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
  return new Promise<string>((resolve, reject) => {
    const socket = net.createConnection({ host, port });
    const authId = randomRequestId();
    const commandId = randomRequestId();
    let buffer: Buffer<ArrayBufferLike> = Buffer.alloc(0);
    let authenticated = false;
    const decoder = new StringDecoder("utf8");
    let response = "";
    let settled = false;
    let responseTimer: NodeJS.Timeout | undefined;
    const timeout = setTimeout(
      () => finish(new Error("RCON connection timed out")),
      COMMAND_TIMEOUT_MS
    );

    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      if (responseTimer) clearTimeout(responseTimer);
      socket.destroy();
      if (error) reject(error);
      else resolve(response + decoder.end());
    };

    socket.setNoDelay(true);
    socket.setTimeout(CONNECT_TIMEOUT_MS, () => {
      finish(new Error("RCON connection timed out"));
    });
    socket.once("error", () => {
      finish(new Error("Could not connect to the server's RCON endpoint"));
    });
    socket.once("connect", () => {
      socket.setTimeout(0);
      try {
        assertAccess?.();
        socket.write(encodeRconPacket(authId, 3, password));
      } catch {
        finish(new Error("Console access changed"));
      }
    });
    socket.on("data", (chunk) => {
      buffer = Buffer.concat([buffer, chunk]);
      try {
        const decoded = decodeRconPackets(buffer);
        buffer = decoded.remaining;
        for (const packet of decoded.packets) {
          if (!authenticated) {
            if (packet.id === -1) {
              finish(new Error("RCON authentication failed"));
              return;
            }
            if (packet.id === authId && packet.type === 2) {
              assertAccess?.();
              authenticated = true;
              socket.write(encodeRconPacket(commandId, 2, command));
            }
            continue;
          }
          if (packet.id !== commandId) continue;
          response += decoder.write(packet.body);
          if (responseTimer) clearTimeout(responseTimer);
          responseTimer = setTimeout(() => finish(), 150);
        }
      } catch (error) {
        finish(error instanceof Error ? error : new Error(String(error)));
      }
    });
    socket.once("end", () => finish(
      authenticated ? undefined : new Error("RCON authentication did not complete"),
    ));
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
  return new Promise<string>((resolve, reject) => {
    const socket = net.createConnection({ host, port });
    let output = "";
    const decoder = new StringDecoder("utf8");
    let passwordSent = false;
    let commandSent = false;
    let settled = false;
    let quietTimer: NodeJS.Timeout | undefined;
    const timeout = setTimeout(
      () => finish(new Error("Telnet console command timed out")),
      COMMAND_TIMEOUT_MS
    );

    const finish = (error?: Error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      if (quietTimer) clearTimeout(quietTimer);
      socket.destroy();
      if (error) reject(error);
      else resolve(cleanTelnetOutput(output + decoder.end(), command));
    };

    socket.setTimeout(CONNECT_TIMEOUT_MS, () =>
      finish(new Error("Telnet console connection timed out"))
    );
    socket.once("error", () =>
      finish(new Error("Could not connect to the server's Telnet console"))
    );
    socket.once("connect", () => socket.setTimeout(0));
    socket.on("data", (chunk) => {
      respondToTelnetNegotiation(socket, chunk);
      const text = decoder.write(stripTelnetNegotiation(chunk));
      output += text;
      if (!passwordSent && /password\s*[:>]?/i.test(output)) {
        try {
          assertAccess?.();
          socket.write(`${password}\n`);
          passwordSent = true;
          setTimeout(() => {
            if (settled) return;
            try {
              assertAccess?.();
              socket.write(`${command}\n`);
              commandSent = true;
              quietTimer = setTimeout(() => finish(), 250);
            } catch {
              finish(new Error("Console access changed"));
            }
          }, 50);
        } catch {
          finish(new Error("Console access changed"));
        }
        return;
      }
      if (passwordSent) {
        if (/incorrect|invalid password|authentication failed/i.test(output)) {
          finish(new Error("Telnet console authentication failed"));
          return;
        }
        if (commandSent) {
          if (quietTimer) clearTimeout(quietTimer);
          quietTimer = setTimeout(() => finish(), 250);
        }
      }
    });
    socket.once("end", () => finish(
      commandSent ? undefined : new Error("Telnet console closed before the command was sent"),
    ));
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
): Promise<void> {
  const exec = await container.exec(options);
  assertAccess?.();
  const stream = await exec.start({ hijack: true, stdin: false });
  await streamExecOutput(stream, output);
  const result = await exec.inspect();
  if (result.Running || result.ExitCode !== 0)
    throw new Error("Game console command failed");
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
  output: GameCommandOutput
): Promise<void> {
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
      stream.once("error", reject);
      getDockerInstance().modem.demuxStream(stream, stdout, stderr);
    });
  } finally {
    stdout.end();
    stderr.end();
  }
}

function encodeRconPacket(id: number, type: number, body: string): Buffer {
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

function respondToTelnetNegotiation(socket: net.Socket, chunk: Buffer): void {
  for (let index = 0; index + 2 < chunk.length; index += 1) {
    if (chunk[index] !== 255) continue;
    const command = chunk[index + 1];
    const option = chunk[index + 2];
    if (command === 251 || command === 252) {
      socket.write(Buffer.from([255, 254, option]));
    } else if (command === 253 || command === 254) {
      socket.write(Buffer.from([255, 252, option]));
    }
    index += 2;
  }
}

function stripTelnetNegotiation(chunk: Buffer): Buffer {
  const bytes: number[] = [];
  for (let index = 0; index < chunk.length; index += 1) {
    if (chunk[index] === 255 && index + 2 < chunk.length) {
      index += 2;
      continue;
    }
    bytes.push(chunk[index]);
  }
  return Buffer.from(bytes);
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
