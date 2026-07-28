export interface ManagedContainer {
  id: string;
  shortId: string;
  name: string;
  displayName: string;
  image: string;
  state: string;
  status: string;
  gameType: string;
  gameConsole: {
    id:
      | "minecraft-rcon"
      | "source-rcon"
      | "rust-webrcon"
      | "telnet-console"
      | "stdin-console";
    name: string;
    commandPlaceholder: string;
  } | null;
  fileRoots: Array<{
    id: string;
    name: string;
    path: string;
  }>;
  ports: Array<{ private: number; public: number; type: string }>;
  created: number;
  labels: Record<string, string>;
}

export interface ConsoleMessage {
  type: "stdout" | "stderr" | "system" | "error";
  data: string;
}

export interface ContainerEvent {
  type: "container_event";
  action: string;
  containerId: string;
  name: string;
  time: number;
}

const GAME_ABBREVIATIONS: Record<string, string> = {
  minecraft: "MC",
  valheim: "VH",
  terraria: "TR",
  factorio: "FA",
  ark: "ARK",
  rust: "RS",
  csgo: "CS",
  cs2: "CS2",
  palworld: "PW",
  satisfactory: "SF",
};

export function getGameAbbreviation(gameType: string): string {
  const normalized = gameType.trim().toLowerCase();
  return (
    GAME_ABBREVIATIONS[normalized] ||
    normalized.replace(/[^a-z0-9]/g, "").slice(0, 3).toUpperCase() ||
    "GAME"
  );
}
