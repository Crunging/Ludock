import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  FileStorageError,
  getFileRoots,
  normalizeRelativePath,
} from "../src/file-storage.js";

describe("container file storage", () => {
  it("selects the conventional Minecraft data directory", () => {
    assert.deepEqual(
      getFileRoots({
        gameType: "minecraft",
        image: "itzg/minecraft-server",
        labels: {},
      }),
      [{ id: "root-0", name: "Minecraft data", path: "/data" }]
    );
  });

  it("selects /config for known Valheim images", () => {
    assert.deepEqual(
      getFileRoots({
        gameType: "valheim",
        image: "ghcr.io/community-valheim-tools/valheim-server:latest",
        labels: {},
      }),
      [{ id: "root-0", name: "Valheim config", path: "/config" }]
    );
  });

  it("supports explicit roots and discards paths that normalize to root", () => {
    assert.deepEqual(
      getFileRoots({
        gameType: "custom",
        image: "example/game",
        labels: {
          "game-panel.files": "/srv/game, /srv/backups/, /srv/game, /data/..",
        },
      }),
      [
        { id: "root-0", name: "game", path: "/srv/game" },
        { id: "root-1", name: "backups", path: "/srv/backups" },
      ]
    );
  });

  it("rejects absolute paths, traversal, and null bytes", () => {
    assert.equal(normalizeRelativePath("worlds/main"), "worlds/main");
    assert.equal(normalizeRelativePath("worlds/./main"), "worlds/main");
    for (const invalid of ["/etc/passwd", "../etc", "worlds/../../etc", "a\0b"]) {
      assert.throws(
        () => normalizeRelativePath(invalid),
        (error) =>
          error instanceof FileStorageError && error.code === "INVALID_PATH"
      );
    }
  });
});
