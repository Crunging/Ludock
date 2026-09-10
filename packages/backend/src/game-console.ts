import type Docker from "dockerode";
import type { ManagedContainer } from "./docker.js";
import { getGameIntegration } from "./server-presets.js";

export const LABEL_CONSOLE = "ludock.console";
export const LABEL_CONSOLE_PORT = "ludock.console.port";
export const LABEL_CONSOLE_HOST = "ludock.console.host";
export const LABEL_CONSOLE_PASSWORD_ENV = "ludock.console.password-env";

export type GameConsoleAdapterId =
  | "minecraft-rcon"
  | "source-rcon"
  | "rust-webrcon"
  | "telnet-console"
  | "stdin-console";

export type GameConsoleTransport =
  | "docker-exec"
  | "source-rcon"
  | "rust-webrcon"
  | "telnet"
  | "container-stdin";

export interface GameConsoleAdapter {
  id: GameConsoleAdapterId;
  name: string;
  transport: GameConsoleTransport;
  commandPlaceholder: string;
  defaultPort?: number;
  passwordEnvCandidates?: readonly string[];
  createExecOptions?(command: string): Docker.ExecCreateOptions;
}

const minecraftRconAdapter: GameConsoleAdapter = {
  id: "minecraft-rcon",
  name: "Minecraft RCON",
  transport: "docker-exec",
  commandPlaceholder: "difficulty hard, whitelist add PlayerName, say Hello",
  createExecOptions(command) {
    const normalized = command.startsWith("/") ? command.slice(1) : command;
    return {
      Cmd: ["rcon-cli", normalized],
      AttachStdout: true,
      AttachStderr: true,
      Tty: false,
    };
  },
};

const sourceRconAdapter: GameConsoleAdapter = {
  id: "source-rcon",
  name: "RCON",
  transport: "source-rcon",
  commandPlaceholder: "status, save, broadcast Hello",
  passwordEnvCandidates: [
    "RCON_PASSWORD",
    "ADMIN_PASSWORD",
    "SERVER_ADMIN_PASSWORD",
    "ARK_ADMIN_PASSWORD",
    "SRCDS_RCONPW",
  ],
};

const rustWebRconAdapter: GameConsoleAdapter = {
  id: "rust-webrcon",
  name: "Rust WebRCON",
  transport: "rust-webrcon",
  defaultPort: 28016,
  commandPlaceholder: "status, say Hello, server.save",
  passwordEnvCandidates: ["RCON_PASSWORD"],
};

const stdinConsoleAdapter: GameConsoleAdapter = {
  id: "stdin-console",
  name: "Server console",
  transport: "container-stdin",
  commandPlaceholder: "help, playing, save",
};

const telnetConsoleAdapter: GameConsoleAdapter = {
  id: "telnet-console",
  name: "Telnet console",
  transport: "telnet",
  commandPlaceholder: "help, listplayers, say Hello",
  passwordEnvCandidates: ["TELNET_PASSWORD"],
};

const ADAPTERS: Record<GameConsoleAdapterId, GameConsoleAdapter> = {
  "minecraft-rcon": minecraftRconAdapter,
  "source-rcon": sourceRconAdapter,
  "rust-webrcon": rustWebRconAdapter,
  "telnet-console": telnetConsoleAdapter,
  "stdin-console": stdinConsoleAdapter,
};

export function resolveGameConsoleAdapter(
  server: Pick<ManagedContainer, "gameType" | "labels">,
): GameConsoleAdapter | null {
  const configured = server.labels[LABEL_CONSOLE]?.trim().toLowerCase();
  if (configured === "disabled" || configured === "none") return null;
  if (configured && isAdapterId(configured)) return ADAPTERS[configured];

  const preset = getGameIntegration(server.gameType)?.console;
  if (!preset) return null;
  const adapter = ADAPTERS[preset.adapter];
  return {
    ...adapter,
    name: preset.name || adapter.name,
    commandPlaceholder: preset.placeholder || adapter.commandPlaceholder,
    defaultPort: preset.defaultPort ?? adapter.defaultPort,
    passwordEnvCandidates:
      preset.passwordEnvCandidates || adapter.passwordEnvCandidates,
  };
}

export function getGameConsoleAdapterSummary(
  server: Pick<ManagedContainer, "gameType" | "labels">,
): {
  id: GameConsoleAdapterId;
  name: string;
  commandPlaceholder: string;
} | null {
  const adapter = resolveGameConsoleAdapter(server);
  return adapter
    ? {
        id: adapter.id,
        name: adapter.name,
        commandPlaceholder: adapter.commandPlaceholder,
      }
    : null;
}

function isAdapterId(value: string): value is GameConsoleAdapterId {
  return Object.prototype.hasOwnProperty.call(ADAPTERS, value);
}
