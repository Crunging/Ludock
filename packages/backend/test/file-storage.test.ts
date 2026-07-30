import assert from "node:assert/strict";
import { describe, it } from "node:test";
import {
  FileStorageError,
  getFileRoots,
  normalizeRelativePath,
} from "../src/file-storage.js";

describe("container file storage", () => {
  it("selects writable bind and volume mount destinations", () => {
    assert.deepEqual(
      getFileRoots(
        {
          gameType: "minecraft",
          image: "itzg/minecraft-server",
          labels: {},
        },
        [
          {
            Type: "volume",
            Source: "minecraft-data",
            Destination: "/data",
            RW: true,
          },
          {
            Type: "bind",
            Source: "/srv/minecraft-backups",
            Destination: "/backups/",
            RW: true,
          },
        ]
      ),
      [
        { id: "root-0", name: "Minecraft data", path: "/data" },
        { id: "root-1", name: "backups", path: "/backups" },
      ]
    );
  });

  it("uses the actual Terraria mount instead of an image default", () => {
    assert.deepEqual(
      getFileRoots(
        {
          gameType: "terraria",
          image: "hexlo/terraria-server-docker:latest",
          labels: {},
        },
        [
          {
            Type: "bind",
            Source: "/srv/terraria",
            Destination: "/root/.local/share/Terraria/Worlds",
            RW: true,
          },
        ]
      ),
      [
        {
          id: "root-0",
          name: "Terraria worlds",
          path: "/root/.local/share/Terraria/Worlds",
        },
      ]
    );
  });

  it("does not infer read-only, system, broad, or nested mounts", () => {
    assert.deepEqual(
      getFileRoots(
        {
          gameType: "custom",
          image: "example/game",
          labels: {},
        },
        [
          {
            Type: "bind",
            Source: "/srv/game",
            Destination: "/data",
            RW: true,
          },
          {
            Type: "volume",
            Source: "nested",
            Destination: "/data/config",
            RW: true,
          },
          {
            Type: "bind",
            Source: "/etc/localtime",
            Destination: "/etc/localtime",
            RW: false,
          },
          {
            Type: "bind",
            Source: "/",
            Destination: "/host",
            RW: true,
          },
          {
            Type: "bind",
            Source: "/var/run/docker.sock",
            Destination: "/var/run/docker.sock",
            RW: true,
          },
          {
            Type: "tmpfs",
            Source: "",
            Destination: "/cache",
            RW: true,
          },
        ]
      ),
      [{ id: "root-0", name: "data", path: "/data" }]
    );
  });

  it("supports explicit roots and discards paths that normalize to root", () => {
    assert.deepEqual(
      getFileRoots({
        gameType: "custom",
        image: "example/game",
        labels: {
          "ludock.files": "/srv/game, /srv/backups/, /srv/game, /data/..",
        },
      }),
      [
        { id: "root-0", name: "game", path: "/srv/game" },
        { id: "root-1", name: "backups", path: "/srv/backups" },
      ]
    );
  });

  it("allows mount inference to be disabled with an empty files label", () => {
    assert.deepEqual(
      getFileRoots(
        {
          gameType: "custom",
          image: "example/game",
          labels: { "ludock.files": "" },
        },
        [
          {
            Type: "volume",
            Source: "game-data",
            Destination: "/data",
            RW: true,
          },
        ]
      ),
      []
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
