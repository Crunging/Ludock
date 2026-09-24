import { fixtureBytes } from "./fixtures/bytes.js";
import { byteView, concatBytes, decodeText } from "../src/bytes.js";
import { thrownBy, rejectedBy } from "./fixtures/errors.js";
import { StreamFixture, bytesStream } from "./fixtures/web-streams.js";
import { dockerStdout, DockerStreamError } from "../src/docker-stream.js";
import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import {
  FileStorageError,
  getFileRoots,
  normalizeRelativePath,
  isSafeWritableDataMount,
  acquireFileContainer,
  createDirectory,
  uploadFile,
  openDownload,
} from "../src/file-storage.js";
import { docker } from "../src/docker-client.js";
import type { ManagedContainer } from "../src/docker.js";
import type * as Docker from "../src/docker-client.js";
import { createMountProof } from "../src/mount-proof.js";
const fixtureHelperImage = `example/helper@sha256:${"a".repeat(64)}`;
const fixtureRuntimeImage = `sha256:${"b".repeat(64)}`;

function frame(channel: number, payload: string | Uint8Array): Uint8Array {
  const content = fixtureBytes(payload);
  const header = new Uint8Array(8);
  header[0] = channel;
  byteView(header).setUint32(4, content.length);
  return concatBytes([header, content]);
}

describe("container file storage", () => {
  it("selects writable bind and volume mount destinations", () => {
    expect(getFileRoots(
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
      )).toStrictEqual([
        { id: "root-0", name: "Minecraft data", path: "/data" },
        { id: "root-1", name: "backups", path: "/backups" },
      ]);
  });

  it("uses the actual Terraria mount instead of an image default", () => {
    expect(getFileRoots(
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
      )).toStrictEqual([
        {
          id: "root-0",
          name: "Terraria worlds",
          path: "/root/.local/share/Terraria/Worlds",
        },
      ]);
  });

  it("does not infer read-only, system, broad, or nested mounts", () => {
    expect(getFileRoots(
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
      )).toStrictEqual([{ id: "root-0", name: "data", path: "/data" }]);
  });

  it("lets explicit roots restrict actual writable mounts without granting arbitrary paths", () => {
    expect(getFileRoots(
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
      )).toStrictEqual([
        { id: "root-0", name: "game", path: "/srv/game" },
        { id: "root-1", name: "backups", path: "/srv/backups" },
      ]);
  });

  it("does not let labels open missing, read-only, socket, or sensitive roots", () => {
    const server = {
      gameType: "custom",
      image: "example/game",
      labels: {
        "ludock.files": "/data,/etc,/secret,/socket,/arbitrary,/data/private",
      },
    };
    expect(getFileRoots(server, [
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
      ])).toStrictEqual([{ id: "root-0", name: "data", path: "/data" }]);
    expect(getFileRoots({ ...server, labels: { "ludock.files": "/data/world" } }, [
        { Type: "bind", Source: "/srv/game", Destination: "/data", RW: true },
      ])).toStrictEqual([{ id: "root-0", name: "world", path: "/data/world" }]);
    expect(getFileRoots(server)).toStrictEqual([]);
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
      expect(isSafeWritableDataMount({
          Type: "bind",
          Source: source,
          Destination: "/data",
          RW: true,
        }), source).toBe(false);
    }
    const previous = process.env.LUDOCK_SENSITIVE_PATHS;
    process.env.LUDOCK_SENSITIVE_PATHS = "/srv/private/secrets";
    try {
      for (const source of [
        "/srv/private",
        "/srv/private/secrets",
        "/srv/private/secrets/passwords",
      ]) {
        expect(isSafeWritableDataMount({
            Type: "bind",
            Source: source,
            Destination: "/data",
            RW: true,
          }), source).toBe(false);
      }
      expect(isSafeWritableDataMount({
          Type: "bind",
          Source: "/srv/game",
          Destination: "/data",
          RW: true,
        })).toBe(true);
    } finally {
      if (previous === undefined) delete process.env.LUDOCK_SENSITIVE_PATHS;
      else process.env.LUDOCK_SENSITIVE_PATHS = previous;
    }
  });

  it("allows mount inference to be disabled with an empty files label", () => {
    expect(getFileRoots(
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
      )).toStrictEqual([]);
  });

  it("rejects absolute paths, traversal, and null bytes", () => {
    expect(normalizeRelativePath("worlds/main")).toBe("worlds/main");
    expect(normalizeRelativePath("worlds/./main")).toBe("worlds/main");
    for (const invalid of [
      "/etc/passwd",
      "../etc",
      "worlds/../../etc",
      "a\0b",
    ]) {
      expect(thrownBy(() => normalizeRelativePath(invalid))).toSatisfy((error) =>
          error instanceof FileStorageError && error.code === "INVALID_PATH");
    }
  });
});

describe("scoped file helper projections", () => {
  const originals = {
    getContainer: docker.getContainer,
    getVolume: docker.getVolume,
    createContainer: docker.createContainer,
    helperImage: process.env.FILE_HELPER_IMAGE,
    selfContainer: process.env.LUDOCK_SELF_CONTAINER,
  };
  beforeEach(() => { process.env.FILE_HELPER_IMAGE = fixtureHelperImage; });
  afterEach(() => {
    docker.getContainer = originals.getContainer;
    docker.getVolume = originals.getVolume;
    docker.createContainer = originals.createContainer;
    if (originals.helperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
    else process.env.FILE_HELPER_IMAGE = originals.helperImage;
    process.env.LUDOCK_SELF_CONTAINER = originals.selfContainer;
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
    await expect(await rejectedBy(createDirectory(server, "root-0", "", "new-folder", () => {}))).toSatisfy((error) =>
        error instanceof FileStorageError &&
        error.code === "FILE_TARGET_CHANGED");
    expect(inspectedVolumes).toBe(0);
    expect(createdHelpers).toBe(0);
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
      docker.createContainer = (async () => ({
        start: async () => { if (revokedAt === "startup") allowed = false; },
        remove: async () => { removed = true; },
        exec: async (options: Docker.ExecCreateOptions) => {
          const { operation } = JSON.parse(options.Cmd?.at(-1) || "{}") as { operation: string };
          if (operation === revokedAt) allowed = false;
          return {
            start: async () => {
              dispatched.push(operation);
              const stream = new StreamFixture();
              setImmediate(() => stream.close(frame(1, operation === "stat" ? '{"type":"file","size":8}' : '{"safe":true}')));
              return stream.connection;
            },
            inspect: async () => ({ ExitCode: 0, Running: false }),
          };
        },
      })) as unknown as typeof docker.createContainer;
      const assertAccess = () => { if (!allowed) throw new Error("Access revoked"); };
      await expect(revokedAt === "download"
          ? openDownload(server, "root-0", "file", assertAccess)
          : createDirectory(server, "root-0", "", "new-folder", assertAccess)).rejects.toThrow(/Access revoked/);
      expect(removed).toBe(true);
      expect(dispatched.includes("mkdir")).toBe(false);
      expect(dispatched.includes("download")).toBe(false);
    });
  }
  for (const [state, helperImage] of [
    ["running", undefined],
    ["exited", `example/custom-bun-helper:test@sha256:${"a".repeat(64)}`],
  ] as const) {
    it(`projects approved mounts for a ${state} server using the ${helperImage ? "custom" : "default"} helper without inheriting sockets`, async () => {
      if (helperImage === undefined) delete process.env.FILE_HELPER_IMAGE;
      else process.env.FILE_HELPER_IMAGE = helperImage;
      process.env.LUDOCK_SELF_CONTAINER = `fixture-${crypto.randomUUID()}`;
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
          Image: fixtureRuntimeImage,
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
      docker.createContainer = (async (
        options: Docker.ContainerCreateOptions,
      ) => {
        if (options.Labels?.["ludock.internal"] === "mount-validator") {
          validatorImage = options.Image;
          return {
            start: async () => {},
            remove: async () => {},
            logs: async () => {
              const stream = new StreamFixture();
              setImmediate(() =>
                stream.enqueue(
                  frame(1, JSON.stringify({
                    identities: { "/data": { dev: "1", ino: "2" } },
                  }) + "\n"),
                ),
              );
              return stream.readable;
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
              const stream = new StreamFixture();
              setImmediate(() => stream.close(frame(1, '{"safe":true}')));
              return stream.connection;
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
      expect(created?.Image).toBe(helperImage || fixtureRuntimeImage);
      expect(validatorImage).toBe(helperImage || fixtureRuntimeImage);
      expect(created?.HostConfig?.VolumesFrom).toBe(undefined);
      expect(created?.HostConfig?.Mounts?.map((mount) => [
          mount.Source,
          mount.Target,
          mount.ReadOnly,
        ])).toStrictEqual([
          ["/srv/game", "/data", true],
          ["config-data", "/data/config", true],
        ]);
      expect(access.blockedPaths).toStrictEqual(["/data/docker.sock"]);
      expect(created?.HostConfig?.ReadonlyRootfs).toBe(true);
      expect(created?.HostConfig?.NetworkMode).toBe("none");
      expect(created?.HostConfig?.CapDrop).toStrictEqual(["ALL"]);
      expect(created?.HostConfig?.CapAdd).toStrictEqual(["DAC_OVERRIDE"]);
      expect(created?.Labels?.["ludock.enable"]).toBe("false");
      await access.cleanup();
      expect(removed).toBe(true);
    });
  }

  for (const outcome of ["complete", "cancelled", "revoked"] as const) {
    it(`commits only a complete authorized upload and drains cleanup when ${outcome}`, async () => {
      let allowed = true;
      let removed = false;
      let helperEnded = false;
      let uploadId = "";
      let cleanupId = "";
      const sent: Uint8Array[] = [];
      const bodyReceived = Promise.withResolvers<void>();
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
      docker.createContainer = (async (options: Docker.ContainerCreateOptions) => {
        expect(options.HostConfig?.CapAdd).toStrictEqual(["DAC_OVERRIDE", "CHOWN"]);
        return {
          start: async () => {},
          remove: async () => { expect(helperEnded).toBe(true); removed = true; },
          exec: async (options: Docker.ExecCreateOptions) => {
            const request = JSON.parse(options.Cmd?.at(-1) || "{}") as { operation: string; uploadId?: string };
            if (request.operation === "upload") uploadId = request.uploadId || "";
            if (request.operation === "upload-cleanup") {
              expect(helperEnded, "Cleanup must await the interrupted helper's read side").toBe(true);
              cleanupId = request.uploadId || "";
            }
            return {
              start: async () => {
                if (request.operation !== "upload") {
                  const response = new StreamFixture();
                  setImmediate(() => response.close(frame(1, '{"ok":true}')));
                  return response.connection;
                }
                const duplex = new StreamFixture();
                duplex.onInput = chunk => { sent.push(fixtureBytes(chunk)); bodyReceived.resolve(); };
                duplex.onInputEnd = () => {
                  setTimeout(() => {
                    helperEnded = true;
                    duplex.close(frame(1, '{"ok":true}'));
                  }, 10);
                };
                return duplex.connection;
              },
              inspect: async () => ({ ExitCode: 0, Running: false }),
            };
          },
        };
      }) as unknown as typeof docker.createContainer;
      let pushed = false;
      const source = new ReadableStream<Uint8Array>({
        async pull(controller) {
          if (pushed) return;
          pushed = true;
          controller.enqueue(new TextEncoder().encode("complete-body"));
          await bodyReceived.promise;
          setImmediate(() => {
            if (outcome === "cancelled") controller.error(new Error("private request failure"));
            else {
              if (outcome === "revoked") allowed = false;
              controller.close();
            }
          });
        },
      });
      const uploading = uploadFile(server, "root-0", "", "world.cfg", 13, source, () => {
        if (!allowed) throw new Error("Access revoked");
      });
      if (outcome === "complete") await uploading;
      else await expect(await rejectedBy(uploading)).toSatisfy((error) => outcome === "revoked"
        ? error instanceof Error && error.message === "Access revoked"
        : error instanceof FileStorageError && error.code === "FILE_UPLOAD_FAILED");
      expect(removed).toBe(true);
      expect(uploadId).toMatch(/^[a-f0-9-]{36}$/);
      expect(cleanupId).toBe(uploadId);
      expect(decodeText(concatBytes(sent))).toBe("complete-body" + (outcome === "complete" ? uploadId : ""));
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
      await expect(createMountProof([
          {
            Type: "volume",
            Source: "game-volume",
            Name: "game-volume",
            Destination: "/data",
            RW: true,
          },
        ])).rejects.toThrow(/could not be verified/);
    }
    docker.getVolume = (() => ({
      inspect: async () => {
        throw new Error("private-host-path");
      },
    })) as unknown as typeof docker.getVolume;
    await expect(await rejectedBy(createMountProof([
        {
          Type: "volume",
          Source: "game-volume",
          Destination: "/data",
          RW: true,
        },
      ]))).toSatisfy((error) =>
        error instanceof Error && !error.message.includes("private-host-path"));
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
    docker.createContainer = (async () => ({
      start: async () => {
        started = true;
      },
      remove: async () => {
        removed = true;
      },
      exec: async () => ({
        start: async () => {
          const stream = new StreamFixture();
          setImmediate(() => stream.close(frame(1, '{"safe":true}')));
          return stream.connection;
        },
        inspect: async () => ({ ExitCode: 0, Running: false }),
      }),
    })) as unknown as typeof docker.createContainer;
    await expect(await rejectedBy(acquireFileContainer(
        {
          id: "physical",
          name: "game",
          image: "example/game",
          gameType: "unknown",
          labels: { "ludock.enable": "true" },
        } as ManagedContainer,
        { id: "root-0", name: "data", path: "/data" },
      ))).toSatisfy((error) =>
        error instanceof FileStorageError &&
        error.code === "FILE_TARGET_CHANGED");
    expect(removed).toBe(true);
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
          const stream = new StreamFixture();
          setImmediate(() =>
            stream.close(
              request.operation === "download"
                ? frame(1, "contents")
                : request.operation === "stat"
                  ? frame(1, '{"type":"file","size":8}')
                  : frame(1, '{"safe":true}'),
            ),
          );
          return stream.connection;
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
    for await (const chunk of download.stream)
      contents += decodeText(fixtureBytes(chunk));
    await started;
    expect(contents).toBe("contents");
    expect(completed).toBe(false);
    finishRemoval();
    await download.completed;
    expect(completed).toBe(true);
  });
});

describe("bounded Docker download transport", () => {
  it("decodes split frame headers and bodies while dropping stderr", async () => {
    const data = concatBytes([
      frame(1, "hello"),
      frame(2, "private stderr"),
      frame(1, " world"),
    ]);
    const output = dockerStdout(bytesStream([...data].map(value => fixtureBytes([value]))));
    expect(await new Response(output).text()).toBe("hello world");
    await expect(await rejectedBy(new Response(dockerStdout(bytesStream([data.subarray(0, data.length - 1)]))).arrayBuffer())).toSatisfy(error => error instanceof DockerStreamError && error.code === "INCOMPLETE_STREAM");
  });

  it("waits for a slow consumer instead of draining the entire Docker source into memory", async () => {
    let emitted = 0;
    const source = ReadableStream.from(
      (async function* () {
        for (let index = 0; index < 1000; index++) {
          emitted++;
          yield frame(1, new Uint8Array(1024));
        }
      })(),
    );
    const output = dockerStdout(source);
    await new Promise(resolve => setImmediate(resolve));
    expect(emitted < 150, "Queues must stay bounded; received " + emitted).toBeTruthy();
    await output.cancel("Client closed");
    const atCancellation = emitted;
    await new Promise(resolve => setImmediate(resolve));
    expect(emitted).toBe(atCancellation);

  });
});
