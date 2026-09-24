import type { GameConsoleAdapterId } from "./game-console.js";

interface GameConsolePreset {
  adapter: GameConsoleAdapterId;
  name?: string;
  placeholder?: string;
  defaultPort?: number;
  passwordEnvCandidates?: readonly string[];
}

export interface GameIntegration {
  gameType: string;
  repositories: readonly string[];
  aliases?: readonly string[];
  console?: GameConsolePreset;
}

// Repository suffixes intentionally omit registries. Recognition selects an
// integration; it does not attest to the provenance or safety of an image.
export const GAME_INTEGRATIONS: readonly GameIntegration[] = [
  {
    gameType: "minecraft",
    repositories: ["itzg/minecraft-server"],
    console: { adapter: "minecraft-rcon" },
  },
  {
    gameType: "factorio",
    repositories: ["factoriotools/factorio"],
    console: {
      adapter: "source-rcon",
      name: "Factorio RCON",
      defaultPort: 27015,
      placeholder: "/players, /server-save, /config get",
    },
  },
  {
    gameType: "palworld",
    repositories: [
      "thijsvanloef/palworld-server-docker",
      "jammsen/palworld-dedicated-server",
    ],
    console: {
      adapter: "source-rcon",
      name: "Palworld RCON",
      defaultPort: 25575,
      placeholder: "Info, ShowPlayers, Broadcast Hello",
    },
  },
  {
    gameType: "ark-survival-evolved",
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
    },
  },
  {
    gameType: "ark-survival-ascended",
    repositories: ["sknnr/ark-ascended-server", "mschnitzer/asa-linux-server"],
    aliases: ["asa"],
    console: {
      adapter: "source-rcon",
      name: "ARK RCON",
      defaultPort: 27020,
      placeholder: "ListPlayers, SaveWorld, Broadcast Hello",
    },
  },
  {
    gameType: "cs2",
    repositories: ["joedwards32/cs2"],
    aliases: ["csgo", "counter-strike-2"],
    console: {
      adapter: "source-rcon",
      name: "Source RCON",
      defaultPort: 27015,
      placeholder: "status, changelevel de_dust2, say Hello",
      passwordEnvCandidates: ["CS2_RCONPW", "SRCDS_RCONPW", "RCON_PASSWORD"],
    },
  },
  {
    gameType: "project-zomboid",
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
    },
  },
  {
    gameType: "conan-exiles",
    repositories: ["indifferentbroccoli/conan-exiles-enhanced-server-docker"],
    console: {
      adapter: "source-rcon",
      name: "Conan Exiles RCON",
      defaultPort: 25575,
    },
  },
  {
    gameType: "v-rising",
    repositories: ["trueosiris/vrising"],
    console: {
      adapter: "source-rcon",
      name: "V Rising RCON",
      defaultPort: 25575,
    },
  },
  {
    gameType: "rust",
    repositories: ["didstopia/rust-server"],
    console: {
      adapter: "rust-webrcon",
      passwordEnvCandidates: ["RUST_RCON_PASSWORD", "RCON_PASSWORD"],
    },
  },
  {
    gameType: "7-days-to-die",
    repositories: ["vinanrra/7dtd-server"],
    aliases: ["7dtd"],
    console: {
      adapter: "telnet-console",
      name: "7 Days to Die Telnet",
      defaultPort: 8081,
      placeholder: "listplayers, saveworld, say Hello",
    },
  },
  {
    gameType: "valheim",
    repositories: [
      "community-valheim-tools/valheim-server",
      "lloesche/valheim-server",
    ],
  },
  {
    gameType: "terraria",
    repositories: [
      "hexlo/terraria-server-docker",
      "beardedio/terraria",
      "ryshe/terraria",
    ],
    console: {
      adapter: "stdin-console",
      name: "Terraria console",
      placeholder: "playing, save, say Hello",
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
