import { expect, describe, it } from "bun:test";
import {
  getServerImagePreset,
  getGameCapabilityMatrix,
  getGameIntegration,
  inferGameType,
  normalizeImageRepository,
} from "../src/server-presets.js";

describe("server image presets", () => {
  it("recognizes supported repositories with tags, registries, and digests", () => {
    expect(inferGameType("itzg/minecraft-server:latest")).toBe("minecraft");
    expect(inferGameType("docker.io/hexlo/terraria-server-docker:1.4.5.6")).toBe("terraria");
    expect(inferGameType(
        "ghcr.io/community-valheim-tools/valheim-server@sha256:abcdef",
      )).toBe("valheim");
  });

  it("recognizes repository suffixes under private mirror prefixes", () => {
    expect(getServerImagePreset("hexlo/terraria-server-docker")).toStrictEqual({
      gameType: "terraria",
    });
    for (const image of [
      "example/hexlo/terraria-server-docker",
      "registry.example.com:5000/proxy/hexlo/terraria-server-docker:latest",
      "localhost:5000/hexlo/terraria-server-docker@sha256:abc",
    ])
      expect(inferGameType(image)).toBe("terraria");
  });

  it("normalizes digests, tags, registry ports and case independently", () => {
    expect(normalizeImageRepository(
        " REGISTRY.EXAMPLE:5000/Itzg/Minecraft-Server:TAG@sha256:abc ",
      )).toBe("itzg/minecraft-server");
    expect(normalizeImageRepository("index.docker.io/itzg/minecraft-server:latest")).toBe("itzg/minecraft-server");
  });

  it("rejects repository suffix lookalikes", () => {
    for (const image of [
      "notitzg/minecraft-server",
      "itzg/minecraft-server-other",
      "itzg/minecraft-server/child",
      "minecraft-server",
      "registry.example/itzg-minecraft-server",
      "",
      "sha256:abcdef",
    ])
      expect(inferGameType(image), image).toBe("unknown");
  });

  it("keeps the capability matrix and console registry aligned", () => {
    const matrix = getGameCapabilityMatrix();
    expect(matrix.length).toBe(13);
    const repositories = new Set<string>();
    for (const game of matrix) {
      expect(getGameIntegration(game.gameType)?.gameType).toBe(game.gameType);
      for (const alias of game.aliases || [])
        expect(getGameIntegration(alias)?.gameType).toBe(game.gameType);
      for (const repository of game.repositories) {
        expect(repositories.has(repository), "repositories must not overlap").toBe(false);
        repositories.add(repository);
        expect(inferGameType(`mirror.example:5000/cache/${repository}:latest`)).toBe(game.gameType);
      }
      expect(game.capabilities.recognition.status).toBe("supported");
      expect(game.capabilities.platforms.status).toBe("unverified");
      expect(game.capabilities.backup.status).toBe("unverified");
      expect(game.capabilities.console.status).toBe(game.console ? "conditional" : "unsupported");
      expect(game.capabilities.update.description).toMatch(/startup game-update behavior is unverified/);
    }
    expect(getGameIntegration("7dtd")?.console?.adapter).toBe("telnet-console");
  });

  it("leaves unknown images unclassified", () => {
    expect(inferGameType("example/custom-server:latest")).toBe("unknown");
  });

  it("covers popular images for every supported game preset", () => {
    const expected = {
      "factoriotools/factorio": "factorio",
      "thijsvanloef/palworld-server-docker": "palworld",
      "hermsi/ark-server": "ark-survival-evolved",
      "mschnitzer/asa-linux-server": "ark-survival-ascended",
      "joedwards32/cs2": "cs2",
      "renegademaster/zomboid-dedicated-server": "project-zomboid",
      "indifferentbroccoli/conan-exiles-enhanced-server-docker": "conan-exiles",
      "trueosiris/vrising": "v-rising",
      "didstopia/rust-server": "rust",
      "vinanrra/7dtd-server": "7-days-to-die",
      "ghcr.io/community-valheim-tools/valheim-server": "valheim",
      "beardedio/terraria": "terraria",
      "itzg/minecraft-server": "minecraft",
    } as const;

    for (const [image, gameType] of Object.entries(expected)) {
      expect(inferGameType(image), image).toBe(gameType);
    }
  });
});
