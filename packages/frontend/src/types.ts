export type {
  Server as ManagedContainer,
  ServerCapability,
  ConsoleMessage,
  ServerEvent as ContainerEvent,
} from "@ludock/shared";

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
    normalized
      .replace(/[^a-z0-9]/g, "")
      .slice(0, 3)
      .toUpperCase() ||
    "GAME"
  );
}
