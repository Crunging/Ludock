import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  getServerImagePreset,
  getGameCapabilityMatrix,
  getGameIntegration,
  inferGameType,
  normalizeImageRepository,
} from "../src/server-presets.js";

describe("server image presets", () => {
  it("recognizes supported repositories with tags, registries, and digests", () => {
    assert.equal(inferGameType("itzg/minecraft-server:latest"), "minecraft");
    assert.equal(
      inferGameType("docker.io/hexlo/terraria-server-docker:1.4.5.6"),
      "terraria",
    );
    assert.equal(
      inferGameType(
        "ghcr.io/community-valheim-tools/valheim-server@sha256:abcdef",
      ),
      "valheim",
    );
  });

  it("recognizes repository suffixes under private mirror prefixes", () => {
    assert.deepEqual(getServerImagePreset("hexlo/terraria-server-docker"), {
      gameType: "terraria",
    });
    for (const image of [
      "example/hexlo/terraria-server-docker",
      "registry.example.com:5000/proxy/hexlo/terraria-server-docker:latest",
      "localhost:5000/hexlo/terraria-server-docker@sha256:abc",
    ])
      assert.equal(inferGameType(image), "terraria");
  });

  it("normalizes digests, tags, registry ports and case independently", () => {
    assert.equal(
      normalizeImageRepository(
        " REGISTRY.EXAMPLE:5000/Itzg/Minecraft-Server:TAG@sha256:abc ",
      ),
      "itzg/minecraft-server",
    );
    assert.equal(
      normalizeImageRepository("index.docker.io/itzg/minecraft-server:latest"),
      "itzg/minecraft-server",
    );
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
      assert.equal(inferGameType(image), "unknown", image);
  });

  it("keeps the capability matrix and console registry aligned", () => {
    const matrix = getGameCapabilityMatrix();
    assert.equal(matrix.length, 13);
    const repositories = new Set<string>();
    for (const game of matrix) {
      assert.equal(getGameIntegration(game.gameType)?.gameType, game.gameType);
      for (const alias of game.aliases || [])
        assert.equal(getGameIntegration(alias)?.gameType, game.gameType);
      for (const repository of game.repositories) {
        assert.equal(
          repositories.has(repository),
          false,
          "repositories must not overlap",
        );
        repositories.add(repository);
        assert.equal(
          inferGameType(`mirror.example:5000/cache/${repository}:latest`),
          game.gameType,
        );
      }
      assert.equal(game.capabilities.recognition.status, "supported");
      assert.equal(game.capabilities.platforms.status, "unverified");
      assert.equal(game.capabilities.backup.status, "unverified");
      assert.equal(
        game.capabilities.console.status,
        game.console ? "conditional" : "unsupported",
      );
      assert.match(
        game.capabilities.update.description,
        /startup game-update behavior is unverified/,
      );
    }
    assert.equal(
      getGameIntegration("7dtd")?.console?.adapter,
      "telnet-console",
    );
  });

  it("leaves unknown images unclassified", () => {
    assert.equal(inferGameType("example/custom-server:latest"), "unknown");
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
      assert.equal(inferGameType(image), gameType, image);
    }
  });
});
