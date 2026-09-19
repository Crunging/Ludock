import { fixtureBytes } from "./fixtures/bytes.js";
import { decodeText } from "../src/bytes.js";
import { expect, describe, it } from "bun:test";
import type * as Docker from "../src/docker-client.js";
import { getDockerInstance, getManagedContainerObservation } from "../src/docker.js";
import {
  createDirectory,
  uploadFile,
  openDownload,
  listFiles,
  renameFileEntry,
  deleteFileEntry,
} from "../src/file-storage.js";
import { createMountProof, assertMountIdentities } from "../src/mount-proof.js";
import { resolveHelperImage } from "../src/runtime-images.js";
import { createHelperContainer, removeHelperContainer } from "../src/docker-helpers.js";

async function cleanupFixtures(
  cleanup: Array<() => Promise<unknown>>,
  testPassed: boolean,
) {
  const errors: unknown[] = [];
  for (const remove of cleanup.reverse()) {
    try {
      await remove();
    } catch (error) {
      errors.push(error);
    }
  }
  if (errors.length) {
    const failure = new AggregateError(errors, "Docker fixture cleanup failed");
    if (testPassed) throw failure;
    // Keep the original test failure while reporting any resources left behind.
    console.error(failure);
  }
}

/** Explicit Docker acceptance harness; never included by the ordinary test glob.
 * Run LUDOCK_DOCKER_TESTS=1 bun test ./test/docker-storage.integration.ts
 * from packages/backend. Every fixture uses a disposable named volume. */
describe.skipIf(process.env.LUDOCK_DOCKER_TESTS !== "1")(
  "Docker storage acceptance",
  () => {
    const docker = getDockerInstance();

    it("removes inherited anonymous helper volumes without removing named game data", async () => {
      const cleanup: Array<() => Promise<unknown>> = [];
      let testPassed = false;
      try {
        const image = await resolveHelperImage();
        const name = `ludock-helper-cleanup-${crypto.randomUUID()}`;
        const volume = await docker.createVolume({ Name: name });
        cleanup.push(() => volume.remove());
        const helper = await createHelperContainer({
          Image: image,
          Entrypoint: [],
          Cmd: ["bun", "--version"],
          Labels: { "ludock.enable": "false" },
          HostConfig: { NetworkMode: "none", Mounts: [{ Type: "volume", Source: name, Target: "/world" }] },
        });
        cleanup.push(() => removeHelperContainer(helper));
        const inherited = (await helper.inspect()).Mounts.find((mount) => mount.Destination === "/data");
        expect(inherited?.Type).toBe("volume");
        expect(inherited?.Name).toBeTruthy();
        expect(inherited?.Name).not.toBe(name);
        await removeHelperContainer(helper);
        await expect(docker.getVolume(inherited!.Name!).inspect()).rejects.toMatchObject({ statusCode: 404 });
        expect((await volume.inspect()).Name).toBe(name);
        testPassed = true;
      } finally {
        await cleanupFixtures(cleanup, testPassed);
      }
    }, 120_000);

    it("uses scoped helpers for complete file operations on stopped and running servers", async () => {
      const image = await resolveHelperImage();
      const cleanup: Array<() => Promise<unknown>> = [];
      let testPassed = false;
      try {
        const volumeName = `ludock-test-${crypto.randomUUID()}`;
        const volume = await docker.createVolume({ Name: volumeName });
        cleanup.push(() => volume.remove());
        const game = await docker.createContainer({
          Image: image,
          Entrypoint: [],
          Cmd: ["bun", "-e", "setInterval(() => {}, 3600000)"],
          Labels: { "ludock.enable": "true" },
          HostConfig: {
            Mounts: [{ Type: "volume", Source: volumeName, Target: "/data" }],
          },
        });
        cleanup.push(() => game.remove({ force: true, v: true }));
        for (const state of ["stopped", "running"]) {
          if (state === "running") await game.start();
          const { container: server } = await getManagedContainerObservation(game.id);
          const root = server.fileRoots[0];
          expect(root).toBeTruthy();
          await createDirectory(server, root.id, "", state);
          await uploadFile(
            server,
            root.id,
            state,
            "test.txt",
            13,
            new Response("hello helper!").body!,
          );
          expect((await listFiles(server, root.id, state)).entries[0].name).toBe("test.txt");
          const file = await openDownload(server, root.id, `${state}/test.txt`);
          let content = "";
          for await (const chunk of file.stream)
            content += decodeText(fixtureBytes(chunk));
          await file.completed;
          expect(content).toBe("hello helper!");
          await renameFileEntry(
            server,
            root.id,
            `${state}/test.txt`,
            "renamed.txt",
          );
          const archive = await openDownload(server, root.id, state);
          let bytes = 0;
          for await (const chunk of archive.stream)
            bytes += (chunk as Uint8Array).length;
          await archive.completed;
          expect(bytes >= 2048).toBeTruthy();
          await deleteFileEntry(server, root.id, state);
        }
        testPassed = true;
      } finally {
        await cleanupFixtures(cleanup, testPassed);
      }
    }, 120_000);

    it("proves bind source identity and rejects symlink or swapped source directories", async () => {
      const image = await resolveHelperImage();
      const cleanup: Array<() => Promise<unknown>> = [];
      let testPassed = false;
      try {
        const name = `ludock-proof-${crypto.randomUUID()}`;
        const volume = await docker.createVolume({ Name: name });
        cleanup.push(() => volume.remove());
        const mountpoint = (await volume.inspect()).Mountpoint;
        const setup = await docker.createContainer({
          Image: image,
          Entrypoint: [],
          Cmd: [
            "bun",
            "-e",
            "const fs=require('fs');fs.mkdirSync('/data/target');fs.mkdirSync('/data/other');fs.symlinkSync('target','/data/link')",
          ],
          HostConfig: {
            Mounts: [{ Type: "volume", Source: name, Target: "/data" }],
          },
        });
        cleanup.push(() => setup.remove({ force: true, v: true }));
        await setup.start();
        await setup.wait();
        const proof = await createMountProof([
          {
            Type: "bind",
            Source: `${mountpoint}/target`,
            Destination: "/data",
            RW: true,
          },
        ]);
        cleanup.push(() => proof.cleanup());
        const fixture = (source: string) =>
          docker.createContainer({
            Image: image,
            Entrypoint: [],
            Cmd: ["bun", "-e", "setInterval(() => {}, 3600000)"],
            HostConfig: {
              Mounts: [{ Type: "bind", Source: source, Target: "/data" }],
            },
          });
        let data: Docker.Container | undefined = await fixture(`${mountpoint}/target`);
        cleanup.push(async () => { if (data) await data.remove({ force: true, v: true }); });
        await data.start();
        await assertMountIdentities(data, proof.identities);
        await expect(createMountProof([
            {
              Type: "bind",
              Source: `${mountpoint}/link`,
              Destination: "/data",
              RW: true,
            },
          ])).rejects.toThrow(/could not be verified/);
        await data.remove({ force: true, v: true });
        data = undefined;
        data = await fixture(`${mountpoint}/other`);
        await data.start();
        await expect(assertMountIdentities(data, proof.identities)).rejects.toThrow(/could not be verified/);
        testPassed = true;
      } finally {
        await cleanupFixtures(cleanup, testPassed);
      }
    }, 120_000);
  },
);
