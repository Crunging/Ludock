import { expect, describe, it } from "bun:test";
import {
  getGameConsoleAdapterSummary,
  resolveGameConsoleAdapter,
} from "../src/game-console.js";

describe("game console adapters", () => {
  it("selects Minecraft RCON for Minecraft servers", () => {
    const server = { gameType: "minecraft", labels: {} };
    expect(getGameConsoleAdapterSummary(server)).toStrictEqual({
      id: "minecraft-rcon",
      name: "Minecraft RCON",
      commandPlaceholder:
        "difficulty hard, whitelist add PlayerName, say Hello",
    });
  });

  it("can explicitly enable or disable an adapter", () => {
    expect(resolveGameConsoleAdapter({
        gameType: "minecraft",
        labels: { "ludock.console": "disabled" },
      })).toBe(null);
    expect(resolveGameConsoleAdapter({
        gameType: "custom",
        labels: { "ludock.console": "minecraft-rcon" },
      })?.id).toBe("minecraft-rcon");
  });

  it("passes native commands to rcon-cli without a shell", () => {
    const adapter = resolveGameConsoleAdapter({
      gameType: "minecraft",
      labels: {},
    });
    expect(adapter).toBeTruthy();
    expect(adapter.createExecOptions?.("/whitelist add PlayerName")).toStrictEqual({
      Cmd: ["rcon-cli", "whitelist add PlayerName"],
      AttachStdout: true,
      AttachStderr: true,
      Tty: false,
    });
  });

  it("selects native transports for popular dedicated servers", () => {
    const expected = {
      factorio: "source-rcon",
      palworld: "source-rcon",
      ark: "source-rcon",
      "ark-survival-ascended": "source-rcon",
      cs2: "source-rcon",
      "project-zomboid": "source-rcon",
      "conan-exiles": "source-rcon",
      "v-rising": "source-rcon",
      rust: "rust-webrcon",
      terraria: "stdin-console",
      "7-days-to-die": "telnet-console",
    } as const;
    for (const [gameType, adapterId] of Object.entries(expected)) {
      expect(resolveGameConsoleAdapter({ gameType, labels: {} })?.id, gameType).toBe(adapterId);
    }
  });

  it("allows any transport to be explicitly selected", () => {
    for (const adapterId of [
      "minecraft-rcon",
      "source-rcon",
      "rust-webrcon",
      "telnet-console",
      "stdin-console",
    ] as const) {
      expect(resolveGameConsoleAdapter({
          gameType: "custom",
          labels: { "ludock.console": adapterId },
        })?.id).toBe(adapterId);
    }
  });

  it("does not guess an adapter for games without a remote protocol", () => {
    expect(resolveGameConsoleAdapter({ gameType: "valheim", labels: {} })).toBe(null);
    expect(resolveGameConsoleAdapter({ gameType: "satisfactory", labels: {} })).toBe(null);
  });

  it("recognizes credential variables used by popular CS2 and Rust images", () => {
    expect(resolveGameConsoleAdapter({ gameType: "cs2", labels: {} })
        ?.passwordEnvCandidates).toStrictEqual(["CS2_RCONPW", "SRCDS_RCONPW", "RCON_PASSWORD"]);
    expect(resolveGameConsoleAdapter({ gameType: "rust", labels: {} })
        ?.passwordEnvCandidates).toStrictEqual(["RUST_RCON_PASSWORD", "RCON_PASSWORD"]);
  });
});
