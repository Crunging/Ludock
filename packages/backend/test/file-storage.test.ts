import assert from "node:assert/strict";
import { afterEach, describe, it } from "bun:test";
import {
  FileStorageError,
  getFileRoots,
  normalizeRelativePath,
  isSafeWritableDataMount,
  acquireFileContainer,
  createDirectory,
  uploadFile,
  openDownload,
  pumpDockerDownload,
} from "../src/file-storage.js";
import { getDockerInstance, type ManagedContainer } from "../src/docker.js";
import { Duplex, PassThrough, Readable } from "node:stream";
import type Docker from "dockerode";
import { createMountProof } from "../src/mount-proof.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";

function frame(channel: number, payload: string | Buffer): Buffer {
  const content = Buffer.from(payload);
  const header = Buffer.alloc(8);
  header[0] = channel;
  header.writeUInt32BE(content.length, 4);
  return Buffer.concat([header, content]);
}

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
        ],
      ),
      [
        { id: "root-0", name: "Minecraft data", path: "/data" },
        { id: "root-1", name: "backups", path: "/backups" },
      ],
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
        ],
      ),
      [
        {
          id: "root-0",
          name: "Terraria worlds",
          path: "/root/.local/share/Terraria/Worlds",
        },
      ],
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
        ],
      ),
      [{ id: "root-0", name: "data", path: "/data" }],
    );
  });

  it("lets explicit roots restrict actual writable mounts without granting arbitrary paths", () => {
    assert.deepEqual(
      getFileRoots(
        {
          gameType: "custom",
          image: "example/game",
          labels: {
            "ludock.files": "/srv/game, /srv/backups/, /srv/game, /data/..",
          },
        },
        [
          {
            Type: "bind",
            Source: "/srv/instance/game",
            Destination: "/srv/game",
            RW: true,
          },
          {
            Type: "bind",
            Source: "/srv/instance/backups",
            Destination: "/srv/backups",
            RW: true,
          },
        ],
      ),
      [
        { id: "root-0", name: "game", path: "/srv/game" },
        { id: "root-1", name: "backups", path: "/srv/backups" },
      ],
    );
  });

  it("does not let labels open missing, read-only, socket, or sensitive roots", () => {
    const server = {
      gameType: "custom",
      image: "example/game",
      labels: {
        "ludock.files": "/data,/etc,/secret,/socket,/arbitrary,/data/private",
      },
    };
    assert.deepEqual(
      getFileRoots(server, [
        { Type: "bind", Source: "/srv/game", Destination: "/data", RW: true },
        { Type: "bind", Source: "/etc", Destination: "/etc", RW: true },
        {
          Type: "bind",
          Source: "/srv/read-only",
          Destination: "/secret",
          RW: false,
        },
        {
          Type: "bind",
          Source: "/srv/docker.sock",
          Destination: "/socket",
          RW: true,
        },
        {
          Type: "bind",
          Source: "/srv/private",
          Destination: "/data/private",
          RW: false,
        },
      ]),
      [{ id: "root-0", name: "data", path: "/data" }],
    );
    assert.deepEqual(
      getFileRoots({ ...server, labels: { "ludock.files": "/data/world" } }, [
        { Type: "bind", Source: "/srv/game", Destination: "/data", RW: true },
      ]),
      [{ id: "root-0", name: "world", path: "/data/world" }],
    );
    assert.deepEqual(getFileRoots(server), []);
  });

  it("rejects ancestors of protected host directories and deployment-sensitive paths", () => {
    for (const source of [
      "/",
      "/var",
      "/var/lib",
      "/usr",
      "/root",
      "/home",
      "/var/lib/docker",
      "/srv/docker.sock",
    ]) {
      assert.equal(
        isSafeWritableDataMount({
          Type: "bind",
          Source: source,
          Destination: "/data",
          RW: true,
        }),
        false,
        source,
      );
    }
    const previous = process.env.LUDOCK_SENSITIVE_PATHS;
    process.env.LUDOCK_SENSITIVE_PATHS = "/srv/private/secrets";
    try {
      for (const source of [
        "/srv/private",
        "/srv/private/secrets",
        "/srv/private/secrets/passwords",
      ]) {
        assert.equal(
          isSafeWritableDataMount({
            Type: "bind",
            Source: source,
            Destination: "/data",
            RW: true,
          }),
          false,
          source,
        );
      }
      assert.equal(
        isSafeWritableDataMount({
          Type: "bind",
          Source: "/srv/game",
          Destination: "/data",
          RW: true,
        }),
        true,
      );
    } finally {
      if (previous === undefined) delete process.env.LUDOCK_SENSITIVE_PATHS;
      else process.env.LUDOCK_SENSITIVE_PATHS = previous;
    }
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
        ],
      ),
      [],
    );
  });

  it("rejects absolute paths, traversal, and null bytes", () => {
    assert.equal(normalizeRelativePath("worlds/main"), "worlds/main");
    assert.equal(normalizeRelativePath("worlds/./main"), "worlds/main");
    for (const invalid of [
      "/etc/passwd",
      "../etc",
      "worlds/../../etc",
      "a\0b",
    ]) {
      assert.throws(
        () => normalizeRelativePath(invalid),
        (error) =>
          error instanceof FileStorageError && error.code === "INVALID_PATH",
      );
    }
  });
});

describe("scoped file helper projections", () => {
  const docker = getDockerInstance();
  const originals = {
    getContainer: docker.getContainer,
    getVolume: docker.getVolume,
    createContainer: docker.createContainer,
    demux: docker.modem.demuxStream,
    helperImage: process.env.FILE_HELPER_IMAGE,
  };
  afterEach(() => {
    docker.getContainer = originals.getContainer;
    docker.getVolume = originals.getVolume;
    docker.createContainer = originals.createContainer;
    docker.modem.demuxStream = originals.demux;
    if (originals.helperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
    else process.env.FILE_HELPER_IMAGE = originals.helperImage;
  });
  it("rejects a rename after authorization before proving or exposing mounts", async () => {
    let inspectedVolumes = 0;
    let createdHelpers = 0;
    const server = {
      id: "physical",
      name: "before-rename",
      image: "example/game",
      gameType: "unknown",
      labels: { "ludock.enable": "true" },
      fileRoots: [{ id: "root-0", name: "data", path: "/data" }],
    } as ManagedContainer;
    docker.getContainer = (() => ({
      inspect: async () => ({
        Id: server.id,
        Name: "/after-rename",
        Config: { Image: server.image, Labels: server.labels },
        Mounts: [
          {
            Type: "volume",
            Name: "game-data",
            Source: "game-data",
            Destination: "/data",
            RW: true,
          },
        ],
      }),
    })) as unknown as typeof docker.getContainer;
    docker.getVolume = (() => {
      inspectedVolumes++;
      return { inspect: async () => ({ Driver: "local", Options: {} }) };
    }) as unknown as typeof docker.getVolume;
    docker.createContainer = (async () => {
      createdHelpers++;
      throw new Error("Unexpected helper creation");
    }) as unknown as typeof docker.createContainer;
    await assert.rejects(
      createDirectory(server, "root-0", "", "new-folder", () => {}),
      (error) =>
        error instanceof FileStorageError &&
        error.code === "FILE_TARGET_CHANGED",
    );
    assert.equal(inspectedVolumes, 0);
    assert.equal(createdHelpers, 0);
  });
  for (const revokedAt of ["startup", "mkdir", "download"] as const) {
    it(`does not dispatch a file request after access is revoked during ${revokedAt}`, async () => {
      let allowed = true;
      let removed = false;
      const dispatched: string[] = [];
      const server = {
        id: "physical", name: "game", image: "example/game", gameType: "unknown",
        labels: { "ludock.enable": "true" },
        fileRoots: [{ id: "root-0", name: "data", path: "/data" }],
      } as ManagedContainer;
      docker.getContainer = (() => ({
        inspect: async () => ({
          Id: server.id, Name: "/game", Config: { Image: server.image, Labels: server.labels },
          Mounts: [{ Type: "volume", Name: "game-data", Source: "game-data", Destination: "/data", RW: true }],
        }),
      })) as unknown as typeof docker.getContainer;
      docker.getVolume = (() => ({ inspect: async () => ({ Driver: "local", Options: {} }) })) as unknown as typeof docker.getVolume;
      docker.modem.demuxStream = ((input, stdout) =>
        input.on("data", (chunk: Buffer) => (stdout as PassThrough).write(chunk))) as typeof docker.modem.demuxStream;
      docker.createContainer = (async () => ({
        start: async () => { if (revokedAt === "startup") allowed = false; },
        remove: async () => { removed = true; },
        exec: async (options: Docker.ExecCreateOptions) => {
          const { operation } = JSON.parse(options.Cmd?.at(-1) || "{}") as { operation: string };
          if (operation === revokedAt) allowed = false;
          return {
            start: async () => {
              dispatched.push(operation);
              const stream = new PassThrough();
              setImmediate(() => stream.end(operation === "stat" ? '{"type":"file","size":8}' : '{"safe":true}'));
              return stream;
            },
            inspect: async () => ({ ExitCode: 0, Running: false }),
          };
        },
      })) as unknown as typeof docker.createContainer;
      const assertAccess = () => { if (!allowed) throw new Error("Access revoked"); };
      await assert.rejects(
        revokedAt === "download"
          ? openDownload(server, "root-0", "file", assertAccess)
          : createDirectory(server, "root-0", "", "new-folder", assertAccess),
        /Access revoked/,
      );
      assert.equal(removed, true);
      assert.equal(dispatched.includes("mkdir"), false);
      assert.equal(dispatched.includes("download"), false);
    });
  }
  for (const [state, helperImage] of [
    ["running", undefined],
    ["exited", `example/custom-bun-helper:test@sha256:${"a".repeat(64)}`],
  ] as const) {
    it(`projects approved mounts for a ${state} server using the ${helperImage ? "custom" : "default"} helper without inheriting sockets`, async () => {
      if (helperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
      else process.env.FILE_HELPER_IMAGE = helperImage;
      let created: Docker.ContainerCreateOptions | undefined;
      let validatorImage: string | undefined;
      let removed = false;
      const mounts = [
        { Type: "bind", Source: "/srv/game", Destination: "/data", RW: true },
        {
          Type: "volume",
          Name: "config-data",
          Source: "/var/lib/docker/volumes/config-data/_data",
          Destination: "/data/config",
          RW: true,
        },
        {
          Type: "bind",
          Source: "/var/run/docker.sock",
          Destination: "/data/docker.sock",
          RW: true,
        },
        {
          Type: "bind",
          Source: "/srv/other-server",
          Destination: "/other",
          RW: true,
        },
      ];
      docker.getContainer = (() => ({
        inspect: async () => ({
          Id: "physical",
          Name: "/game",
          Config: {
            Image: "example/game",
            Labels: { "ludock.enable": "true" },
          },
          Mounts: mounts,
        }),
      })) as unknown as typeof docker.getContainer;
      docker.getVolume = (() => ({
        inspect: async () => ({ Driver: "local", Options: {} }),
      })) as unknown as typeof docker.getVolume;
      docker.modem.demuxStream = ((_input, stdout) => {
        _input.on("data", (chunk: Buffer) =>
          (stdout as PassThrough).write(chunk),
        );
      }) as typeof docker.modem.demuxStream;
      docker.createContainer = (async (
        options: Docker.ContainerCreateOptions,
      ) => {
        if (options.Labels?.["ludock.internal"] === "mount-validator") {
          validatorImage = options.Image;
          return {
            start: async () => {},
            remove: async () => {},
            logs: async () => {
              const stream = new PassThrough();
              setImmediate(() =>
                stream.write(
                  JSON.stringify({
                    identities: { "/data": { dev: "1", ino: "2" } },
                  }) + "\n",
                ),
              );
              return stream;
            },
          };
        }
        created = options;
        return {
          start: async () => {},
          remove: async () => {
            removed = true;
          },
          exec: async () => ({
            start: async () => {
              const stream = new PassThrough();
              setImmediate(() => stream.end('{"safe":true}'));
              return stream;
            },
            inspect: async () => ({ ExitCode: 0, Running: false }),
          }),
        };
      }) as unknown as typeof docker.createContainer;
      const access = await acquireFileContainer(
        {
          id: "physical",
          name: "game",
          state,
          image: "example/game",
          gameType: "unknown",
          labels: { "ludock.enable": "true" },
        } as ManagedContainer,
        { id: "root-0", name: "data", path: "/data" },
        { readOnly: true },
      );
      assert.equal(created?.Image, helperImage || DEFAULT_HELPER_IMAGE);
      assert.equal(validatorImage, helperImage || DEFAULT_HELPER_IMAGE);
      assert.equal(created?.HostConfig?.VolumesFrom, undefined);
      assert.deepEqual(
        created?.HostConfig?.Mounts?.map((mount) => [
          mount.Source,
          mount.Target,
          mount.ReadOnly,
        ]),
        [
          ["/srv/game", "/data", true],
          ["config-data", "/data/config", true],
        ],
      );
      assert.deepEqual(access.blockedPaths, ["/data/docker.sock"]);
      assert.equal(created?.HostConfig?.ReadonlyRootfs, true);
      assert.equal(created?.HostConfig?.NetworkMode, "none");
      assert.deepEqual(created?.HostConfig?.CapDrop, ["ALL"]);
      assert.deepEqual(created?.HostConfig?.CapAdd, ["DAC_OVERRIDE"]);
      assert.equal(created?.Labels?.["ludock.enable"], "false");
      await access.cleanup();
      assert.equal(removed, true);
    });
  }

  for (const outcome of ["complete", "cancelled", "revoked"] as const) {
    it(`commits only a complete authorized upload and drains cleanup when ${outcome}`, async () => {
      let allowed = true;
      let removed = false;
      let helperEnded = false;
      let uploadId = "";
      let cleanupId = "";
      const sent: Buffer[] = [];
      const server = {
        id: "physical", name: "game", image: "example/game", gameType: "unknown",
        labels: { "ludock.enable": "true" },
        fileRoots: [{ id: "root-0", name: "data", path: "/data" }],
      } as ManagedContainer;
      docker.getContainer = (() => ({
        inspect: async () => ({
          Id: server.id, Name: "/game", Config: { Image: server.image, Labels: server.labels },
          Mounts: [{ Type: "volume", Name: "game-data", Source: "game-data", Destination: "/data", RW: true }],
        }),
      })) as unknown as typeof docker.getContainer;
      docker.getVolume = (() => ({ inspect: async () => ({ Driver: "local", Options: {} }) })) as unknown as typeof docker.getVolume;
      docker.modem.demuxStream = ((input, stdout) =>
        input.on("data", (chunk: Buffer) => (stdout as PassThrough).write(chunk))) as typeof docker.modem.demuxStream;
      docker.createContainer = (async (options: Docker.ContainerCreateOptions) => {
        assert.deepEqual(options.HostConfig?.CapAdd, ["DAC_OVERRIDE", "CHOWN"]);
        return {
          start: async () => {},
          remove: async () => { assert.equal(helperEnded, true); removed = true; },
          exec: async (options: Docker.ExecCreateOptions) => {
            const request = JSON.parse(options.Cmd?.at(-1) || "{}") as { operation: string; uploadId?: string };
            if (request.operation === "upload") uploadId = request.uploadId || "";
            if (request.operation === "upload-cleanup") {
              assert.equal(helperEnded, true, "Cleanup must await the interrupted helper's read side");
              cleanupId = request.uploadId || "";
            }
            return {
              start: async () => {
                if (request.operation !== "upload") {
                  const response = new PassThrough();
                  setImmediate(() => response.end('{"ok":true}'));
                  return response;
                }
                const duplex = new Duplex({
                  read() {},
                  write(chunk: Buffer, _encoding, callback) { sent.push(Buffer.from(chunk)); callback(); },
                  final(callback) {
                    callback();
                    setTimeout(() => {
                      helperEnded = true;
                      duplex.push('{"ok":true}');
                      duplex.push(null);
                    }, 10);
                  },
                });
                return duplex;
              },
              inspect: async () => ({ ExitCode: 0, Running: false }),
            };
          },
        };
      }) as unknown as typeof docker.createContainer;
      let pushed = false;
      const source = new Readable({
        read() {
          if (pushed) return;
          pushed = true;
          this.push("complete-body");
          setImmediate(() => {
            if (outcome === "cancelled") this.destroy(new Error("private request failure"));
            else {
              if (outcome === "revoked") allowed = false;
              this.push(null);
            }
          });
        },
      });
      const uploading = uploadFile(server, "root-0", "", "world.cfg", 13, source, () => {
        if (!allowed) throw new Error("Access revoked");
      });
      if (outcome === "complete") await uploading;
      else await assert.rejects(uploading, outcome === "revoked" ? /Access revoked/ : /FILE_UPLOAD_FAILED/);
      assert.equal(removed, true);
      assert.match(uploadId, /^[a-f0-9-]{36}$/);
      assert.equal(cleanupId, uploadId);
      assert.equal(Buffer.concat(sent).toString(), "complete-body" + (outcome === "complete" ? uploadId : ""));
    });
  }

  it("rejects named volumes using driver plugins or host remapping options before exposing them", async () => {
    for (const info of [
      { Driver: "remote-plugin", Options: {} },
      { Driver: "local", Options: { type: "none", device: "/etc", o: "bind" } },
    ]) {
      docker.getVolume = (() => ({
        inspect: async () => info,
      })) as unknown as typeof docker.getVolume;
      await assert.rejects(
        createMountProof([
          {
            Type: "volume",
            Source: "game-volume",
            Name: "game-volume",
            Destination: "/data",
            RW: true,
          },
        ]),
        /could not be verified/,
      );
    }
    docker.getVolume = (() => ({
      inspect: async () => {
        throw new Error("private-host-path");
      },
    })) as unknown as typeof docker.getVolume;
    await assert.rejects(
      createMountProof([
        {
          Type: "volume",
          Source: "game-volume",
          Destination: "/data",
          RW: true,
        },
      ]),
      (error) =>
        error instanceof Error && !error.message.includes("private-host-path"),
    );
  });

  it("cleans up without exposing data when the original game is replaced during helper startup", async () => {
    let started = false;
    let removed = false;
    docker.getContainer = (() => ({
      inspect: async () => {
        if (started)
          throw Object.assign(new Error("original removed"), {
            statusCode: 404,
          });
        return {
          Id: "physical",
          Name: "/game",
          Config: {
            Image: "example/game",
            Labels: { "ludock.enable": "true" },
          },
          Mounts: [
            {
              Type: "volume",
              Name: "game-data",
              Source: "game-data",
              Destination: "/data",
              RW: true,
            },
          ],
        };
      },
    })) as unknown as typeof docker.getContainer;
    docker.getVolume = (() => ({
      inspect: async () => ({ Driver: "local", Options: {} }),
    })) as unknown as typeof docker.getVolume;
    docker.modem.demuxStream = ((input, stdout) =>
      input.on("data", (chunk: Buffer) =>
        (stdout as PassThrough).write(chunk),
      )) as typeof docker.modem.demuxStream;
    docker.createContainer = (async () => ({
      start: async () => {
        started = true;
      },
      remove: async () => {
        removed = true;
      },
      exec: async () => ({
        start: async () => {
          const stream = new PassThrough();
          setImmediate(() => stream.end('{"safe":true}'));
          return stream;
        },
        inspect: async () => ({ ExitCode: 0, Running: false }),
      }),
    })) as unknown as typeof docker.createContainer;
    await assert.rejects(
      acquireFileContainer(
        {
          id: "physical",
          name: "game",
          image: "example/game",
          gameType: "unknown",
          labels: { "ludock.enable": "true" },
        } as ManagedContainer,
        { id: "root-0", name: "data", path: "/data" },
      ),
      (error) =>
        error instanceof FileStorageError &&
        error.code === "FILE_TARGET_CHANGED",
    );
    assert.equal(removed, true);
  });

  it("keeps download completion pending until the helper has been removed", async () => {
    let finishRemoval!: () => void;
    let removalStarted!: () => void;
    const removal = new Promise<void>((resolve) => {
      finishRemoval = resolve;
    });
    const started = new Promise<void>((resolve) => {
      removalStarted = resolve;
    });
    docker.getContainer = (() => ({
      inspect: async () => ({
        Id: "physical",
        Name: "/game",
        Config: { Image: "example/game", Labels: { "ludock.enable": "true" } },
        Mounts: [
          {
            Type: "volume",
            Name: "game-data",
            Source: "game-data",
            Destination: "/data",
            RW: true,
          },
        ],
      }),
    })) as unknown as typeof docker.getContainer;
    docker.getVolume = (() => ({
      inspect: async () => ({ Driver: "local", Options: {} }),
    })) as unknown as typeof docker.getVolume;
    docker.modem.demuxStream = ((input, stdout) =>
      input.on("data", (chunk: Buffer) =>
        (stdout as PassThrough).write(chunk),
      )) as typeof docker.modem.demuxStream;
    docker.createContainer = (async () => ({
      start: async () => {},
      remove: async () => {
        removalStarted();
        await removal;
      },
      exec: async (options: Docker.ExecCreateOptions) => ({
        start: async () => {
          const request = JSON.parse(options.Cmd?.at(-1) || "{}") as {
            operation: string;
          };
          const stream = new PassThrough();
          setImmediate(() =>
            stream.end(
              request.operation === "download"
                ? frame(1, "contents")
                : request.operation === "stat"
                  ? '{"type":"file","size":8}'
                  : '{"safe":true}',
            ),
          );
          return stream;
        },
        inspect: async () => ({ ExitCode: 0, Running: false }),
      }),
    })) as unknown as typeof docker.createContainer;
    const download = await openDownload(
      {
        id: "physical",
        name: "game",
        image: "example/game",
        gameType: "unknown",
        labels: { "ludock.enable": "true" },
        fileRoots: [{ id: "root-0", name: "data", path: "/data" }],
      } as ManagedContainer,
      "root-0",
      "file",
    );
    let completed = false;
    void download.completed.then(() => {
      completed = true;
    });
    let contents = "";
    for await (const chunk of download.stream as Readable)
      contents += (chunk as Buffer).toString();
    await started;
    assert.equal(contents, "contents");
    assert.equal(completed, false);
    finishRemoval();
    await download.completed;
    assert.equal(completed, true);
  });
});

describe("bounded Docker download transport", () => {
  it("decodes split frame headers and bodies while dropping stderr", async () => {
    const data = Buffer.concat([
      frame(1, "hello"),
      frame(2, "private stderr"),
      frame(1, " world"),
    ]);
    const output = new PassThrough();
    let contents = "";
    const collected = (async () => {
      for await (const value of output)
        contents += (value as Buffer).toString();
    })();
    await pumpDockerDownload(
      Readable.from([...data].map((value) => Buffer.from([value]))),
      output,
    );
    output.end();
    await collected;
    assert.equal(contents, "hello world");
    await assert.rejects(
      pumpDockerDownload(
        Readable.from([data.subarray(0, data.length - 1)]),
        new PassThrough(),
      ),
      /INCOMPLETE_DOWNLOAD_STREAM/,
    );
  });

  it("waits for a slow consumer instead of draining the entire Docker source into memory", async () => {
    let emitted = 0;
    const source = Readable.from(
      (async function* () {
        for (let index = 0; index < 1000; index++) {
          emitted++;
          yield frame(1, Buffer.alloc(1024));
        }
      })(),
    );
    const output = new PassThrough({ highWaterMark: 16 });
    output.on("error", () => {});
    const pumping = pumpDockerDownload(source, output);
    await new Promise<void>((resolve) => setImmediate(resolve));
    assert.ok(
      emitted < 10,
      `Only a bounded number of frames may be prefetched; received ${emitted}`,
    );
    output.destroy(new Error("Client closed"));
    await assert.rejects(pumping, /Client closed/);
  });
});
