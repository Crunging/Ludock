import type { GameConsoleAdapterId, QuickCommand } from "./game-console.js";

interface GameConsolePreset {
  adapter: GameConsoleAdapterId;
  name?: string;
  placeholder?: string;
  /** Common commands offered as shortcuts; a trailing space expects more input. */
  commands?: readonly QuickCommand[];
  defaultPort?: number;
  passwordEnvCandidates?: readonly string[];
}

interface GameIntegration {
  gameType: string;
  name: string;
  /** The container port players connect to, used to pick the address to share. */
  gamePort?: number;
  repositories: readonly string[];
  aliases?: readonly string[];
  console?: GameConsolePreset;
}

// Repository suffixes intentionally omit registries. Recognition selects an
// integration; it does not attest to the provenance or safety of an image.
export const GAME_INTEGRATIONS: readonly GameIntegration[] = [
  {
    gameType: "minecraft",
    name: "Minecraft",
    gamePort: 25565,
    repositories: ["itzg/minecraft-server"],
    console: { adapter: "minecraft-rcon" },
  },
  {
    gameType: "factorio",
    name: "Factorio",
    gamePort: 34197,
    repositories: ["factoriotools/factorio"],
    console: {
      adapter: "source-rcon",
      name: "Factorio RCON",
      defaultPort: 27015,
      placeholder: "/players, /server-save, /config get",
      commands: [
        { label: "List players", command: "/players online" },
        { label: "Save world", command: "/server-save" },
      ],
    },
  },
  {
    gameType: "palworld",
    name: "Palworld",
    gamePort: 8211,
    repositories: [
      "thijsvanloef/palworld-server-docker",
      "jammsen/palworld-dedicated-server",
    ],
    console: {
      adapter: "source-rcon",
      name: "Palworld RCON",
      defaultPort: 25575,
      placeholder: "Info, ShowPlayers, Broadcast Hello",
      commands: [
        { label: "List players", command: "ShowPlayers" },
        { label: "Save world", command: "Save" },
        { label: "Server info", command: "Info" },
      ],
    },
  },
  {
    gameType: "ark-survival-evolved",
    name: "ARK: Survival Evolved",
    gamePort: 7777,
    repositories: [
      "hermsi/ark-server",
      "hermsi1337/ark-server",
      "indifferentbroccoli/ark-server-docker",
    ],
    aliases: ["ark"],
    console: {
      adapter: "source-rcon",
      name: "ARK RCON",
      defaultPort: 27020,
      placeholder: "ListPlayers, SaveWorld, Broadcast Hello",
      commands: [
        { label: "List players", command: "ListPlayers" },
        { label: "Save world", command: "SaveWorld" },
        { label: "Message players", command: "Broadcast " },
      ],
    },
  },
  {
    gameType: "ark-survival-ascended",
    name: "ARK: Survival Ascended",
    gamePort: 7777,
    repositories: ["sknnr/ark-ascended-server", "mschnitzer/asa-linux-server"],
    aliases: ["asa"],
    console: {
      adapter: "source-rcon",
      name: "ARK RCON",
      defaultPort: 27020,
      placeholder: "ListPlayers, SaveWorld, Broadcast Hello",
      commands: [
        { label: "List players", command: "ListPlayers" },
        { label: "Save world", command: "SaveWorld" },
        { label: "Message players", command: "Broadcast " },
      ],
    },
  },
  {
    gameType: "cs2",
    name: "Counter-Strike 2",
    gamePort: 27015,
    repositories: ["joedwards32/cs2"],
    aliases: ["csgo", "counter-strike-2"],
    console: {
      adapter: "source-rcon",
      name: "Source RCON",
      defaultPort: 27015,
      placeholder: "status, changelevel de_dust2, say Hello",
      commands: [
        { label: "Server status", command: "status" },
        { label: "Message players", command: "say " },
      ],
      passwordEnvCandidates: ["CS2_RCONPW", "SRCDS_RCONPW", "RCON_PASSWORD"],
    },
  },
  {
    gameType: "project-zomboid",
    name: "Project Zomboid",
    gamePort: 16261,
    repositories: [
      "renegademaster/zomboid-dedicated-server",
      "renegade-master/zomboid-dedicated-server",
      "renegade_master/zomboid-dedicated-server",
    ],
    aliases: ["projectzomboid"],
    console: {
      adapter: "source-rcon",
      name: "Project Zomboid RCON",
      defaultPort: 27015,
      placeholder: "players, save, servermsg Hello",
      commands: [
        { label: "List players", command: "players" },
        { label: "Save world", command: "save" },
        { label: "Message players", command: "servermsg " },
      ],
    },
  },
  {
    gameType: "conan-exiles",
    name: "Conan Exiles",
    gamePort: 7777,
    repositories: ["indifferentbroccoli/conan-exiles-enhanced-server-docker"],
    console: {
      adapter: "source-rcon",
      name: "Conan Exiles RCON",
      defaultPort: 25575,
      commands: [
        { label: "List players", command: "ListPlayers" },
        { label: "Message players", command: "Broadcast " },
      ],
    },
  },
  {
    gameType: "v-rising",
    name: "V Rising",
    gamePort: 9876,
    repositories: ["trueosiris/vrising"],
    console: {
      adapter: "source-rcon",
      name: "V Rising RCON",
      defaultPort: 25575,
      commands: [{ label: "Message players", command: "announce " }],
    },
  },
  {
    gameType: "rust",
    name: "Rust",
    gamePort: 28015,
    repositories: ["didstopia/rust-server"],
    console: {
      adapter: "rust-webrcon",
      passwordEnvCandidates: ["RUST_RCON_PASSWORD", "RCON_PASSWORD"],
    },
  },
  {
    gameType: "7-days-to-die",
    name: "7 Days to Die",
    gamePort: 26900,
    repositories: ["vinanrra/7dtd-server"],
    aliases: ["7dtd"],
    console: {
      adapter: "telnet-console",
      name: "7 Days to Die Telnet",
      defaultPort: 8081,
      placeholder: "listplayers, saveworld, say Hello",
      commands: [
        { label: "List players", command: "listplayers" },
        { label: "Save world", command: "saveworld" },
        { label: "Message players", command: "say " },
      ],
    },
  },
  {
    gameType: "valheim",
    name: "Valheim",
    gamePort: 2456,
    repositories: [
      "community-valheim-tools/valheim-server",
      "lloesche/valheim-server",
    ],
  },
  {
    gameType: "terraria",
    name: "Terraria",
    gamePort: 7777,
    repositories: [
      "hexlo/terraria-server-docker",
      "beardedio/terraria",
      "ryshe/terraria",
    ],
    console: {
      adapter: "stdin-console",
      name: "Terraria console",
      placeholder: "playing, save, say Hello",
      commands: [
        { label: "List players", command: "playing" },
        { label: "Save world", command: "save" },
        { label: "Message players", command: "say " },
      ],
    },
  },
];

export function inferGameType(image: string): string {
  const repository = normalizeImageRepository(image);
  const integration = GAME_INTEGRATIONS.find(({ repositories }) =>
    repositories.some(
      (known) => repository === known || repository.endsWith(`/${known}`),
    ),
  );
  return integration?.gameType ?? "unknown";
}

/** Recognized games use their published name; label overrides keep their own text. */
export function gameDisplayName(gameType: string): string {
  if (gameType === "unknown") return "Other game";
  return getGameIntegration(gameType)?.name ?? gameType;
}

export function getGameIntegration(
  gameType: string,
): GameIntegration | undefined {
  const normalized = gameType
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-");
  return GAME_INTEGRATIONS.find(
    (game) =>
      game.gameType === normalized || game.aliases?.includes(normalized),
  );
}

export function normalizeImageRepository(image: string): string {
  let repository = image.trim().toLowerCase().split("@", 1)[0] || "";
  const lastSlash = repository.lastIndexOf("/");
  const lastColon = repository.lastIndexOf(":");
  if (lastColon > lastSlash) repository = repository.slice(0, lastColon);
  const firstSlash = repository.indexOf("/");
  const firstPart = repository.slice(0, firstSlash);
  if (
    firstSlash !== -1 &&
    (firstPart.includes(".") ||
      firstPart.includes(":") ||
      firstPart === "localhost")
  ) {
    repository = repository.slice(firstSlash + 1);
  }
  return repository;
}
