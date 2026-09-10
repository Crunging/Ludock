import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  getGameConsoleAdapterSummary,
  resolveGameConsoleAdapter,
} from "../src/game-console.js";

describe("game console adapters", () => {
  it("selects Minecraft RCON for Minecraft servers", () => {
    const server = { gameType: "minecraft", labels: {} };
    assert.deepEqual(getGameConsoleAdapterSummary(server), {
      id: "minecraft-rcon",
      name: "Minecraft RCON",
      commandPlaceholder:
        "difficulty hard, whitelist add PlayerName, say Hello",
    });
  });

  it("can explicitly enable or disable an adapter", () => {
    assert.equal(
      resolveGameConsoleAdapter({
        gameType: "minecraft",
        labels: { "ludock.console": "disabled" },
      }),
      null
    );
    assert.equal(
      resolveGameConsoleAdapter({
        gameType: "custom",
        labels: { "ludock.console": "minecraft-rcon" },
      })?.id,
      "minecraft-rcon"
    );
  });

  it("passes native commands to rcon-cli without a shell", () => {
    const adapter = resolveGameConsoleAdapter({
      gameType: "minecraft",
      labels: {},
    });
    assert.ok(adapter);
    assert.deepEqual(adapter.createExecOptions?.("/whitelist add PlayerName"), {
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
      assert.equal(
        resolveGameConsoleAdapter({ gameType, labels: {} })?.id,
        adapterId,
        gameType
      );
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
      assert.equal(
        resolveGameConsoleAdapter({
          gameType: "custom",
          labels: { "ludock.console": adapterId },
        })?.id,
        adapterId
      );
    }
  });

  it("does not guess an adapter for games without a remote protocol", () => {
    assert.equal(
      resolveGameConsoleAdapter({ gameType: "valheim", labels: {} }),
      null
    );
    assert.equal(
      resolveGameConsoleAdapter({ gameType: "satisfactory", labels: {} }),
      null
    );
  });

  it("recognizes credential variables used by popular CS2 and Rust images", () => {
    assert.deepEqual(
      resolveGameConsoleAdapter({ gameType: "cs2", labels: {} })
        ?.passwordEnvCandidates,
      ["CS2_RCONPW", "SRCDS_RCONPW", "RCON_PASSWORD"]
    );
    assert.deepEqual(
      resolveGameConsoleAdapter({ gameType: "rust", labels: {} })
        ?.passwordEnvCandidates,
      ["RUST_RCON_PASSWORD", "RCON_PASSWORD"]
    );
  });
});
