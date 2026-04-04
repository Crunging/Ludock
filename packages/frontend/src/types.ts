export interface ManagedContainer {
  id: string;
  shortId: string;
  name: string;
  displayName: string;
  image: string;
  state: string;
  status: string;
  gameType: string;
  ports: Array<{ private: number; public: number; type: string }>;
  created: number;
  labels: Record<string, string>;
}

export interface ContainerStats {
  cpuPercent: number;
  memUsageMB: number;
  memLimitMB: number;
}

export interface ServerDetail {
  server: ManagedContainer;
  stats: ContainerStats | null;
}

export interface ConsoleMessage {
  type: "stdout" | "stderr" | "system" | "error" | "input";
  data: string;
}

export interface ContainerEvent {
  type: "container_event";
  action: string;
  containerId: string;
  name: string;
  time: number;
}

const GAME_ICONS: Record<string, string> = {
  minecraft: "⛏️",
  valheim: "⚔️",
  terraria: "🌳",
  factorio: "⚙️",
  ark: "🦕",
  rust: "🔫",
  csgo: "💣",
  palworld: "🐾",
  satisfactory: "🏭",
  unknown: "🎮",
};

export function getGameIcon(gameType: string): string {
  return GAME_ICONS[gameType.toLowerCase()] || GAME_ICONS.unknown;
}
