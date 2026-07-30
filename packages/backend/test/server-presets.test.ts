import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  getServerImagePreset,
  inferGameType,
} from "../src/server-presets.js";

describe("server image presets", () => {
  it("recognizes supported repositories with tags, registries, and digests", () => {
    assert.equal(inferGameType("itzg/minecraft-server:latest"), "minecraft");
    assert.equal(
      inferGameType("docker.io/hexlo/terraria-server-docker:1.4.5.6"),
      "terraria"
    );
    assert.equal(
      inferGameType(
        "ghcr.io/community-valheim-tools/valheim-server@sha256:abcdef"
      ),
      "valheim"
    );
  });

  it("matches only exact known image repositories", () => {
    assert.deepEqual(getServerImagePreset("hexlo/terraria-server-docker"), {
      gameType: "terraria",
    });
    assert.equal(
      getServerImagePreset("example/hexlo/terraria-server-docker"),
      undefined
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
      "indifferentbroccoli/conan-exiles-enhanced-server-docker":
        "conan-exiles",
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
