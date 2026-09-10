import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { Readable } from "node:stream";
import { describe, it } from "bun:test";
import type Docker from "dockerode";
import { getDockerInstance, getManagedContainer } from "../src/docker.js";
import {
  createDirectory,
  uploadFile,
  openDownload,
  listFiles,
  renameFileEntry,
  deleteFileEntry,
} from "../src/file-storage.js";
import { createMountProof, assertMountIdentities } from "../src/mount-proof.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";

/** Explicit Docker acceptance harness; never included by the ordinary test glob.
 * Run LUDOCK_DOCKER_TESTS=1 bun test ./test/docker-storage.integration.ts
 * from packages/backend. Every fixture uses a disposable named volume. */
describe.skipIf(process.env.LUDOCK_DOCKER_TESTS !== "1")(
  "Docker storage acceptance",
  () => {
    const docker = getDockerInstance();
    const image = process.env.FILE_HELPER_IMAGE || DEFAULT_HELPER_IMAGE;

    it("uses scoped helpers for complete file operations on stopped and running servers", async () => {
      const volumeName = `ludock-test-${randomUUID()}`;
      await docker.createVolume({ Name: volumeName });
      const game = await docker.createContainer({
        Image: image,
        Cmd: ["bun", "-e", "setInterval(() => {}, 3600000)"],
        Labels: { "ludock.enable": "true" },
        HostConfig: {
          Mounts: [{ Type: "volume", Source: volumeName, Target: "/data" }],
        },
      });
      try {
        for (const state of ["stopped", "running"]) {
          if (state === "running") await game.start();
          const server = await getManagedContainer(game.id);
          const root = server.fileRoots[0];
          assert.ok(root);
          await createDirectory(server, root.id, "", state);
          await uploadFile(
            server,
            root.id,
            state,
            "test.txt",
            13,
            Readable.from("hello helper!"),
          );
          assert.equal(
            (await listFiles(server, root.id, state)).entries[0].name,
            "test.txt",
          );
          const file = await openDownload(server, root.id, `${state}/test.txt`);
          let content = "";
          for await (const chunk of file.stream as Readable)
            content += (chunk as Buffer).toString();
          await file.completed;
          assert.equal(content, "hello helper!");
          await renameFileEntry(
            server,
            root.id,
            `${state}/test.txt`,
            "renamed.txt",
          );
          const archive = await openDownload(server, root.id, state);
          let bytes = 0;
          for await (const chunk of archive.stream as Readable)
            bytes += (chunk as Buffer).length;
          await archive.completed;
          assert.ok(bytes >= 2048);
          await deleteFileEntry(server, root.id, state);
        }
      } finally {
        await game.remove({ force: true });
        await docker.getVolume(volumeName).remove();
      }
    }, 120_000);

    it("proves bind source identity and rejects symlink or swapped source directories", async () => {
      const name = `ludock-proof-${randomUUID()}`;
      await docker.createVolume({ Name: name });
      const volume = docker.getVolume(name);
      const mountpoint = (await volume.inspect()).Mountpoint;
      const setup = await docker.createContainer({
        Image: image,
        Cmd: [
          "bun",
          "-e",
          "const fs=require('fs');fs.mkdirSync('/data/target');fs.mkdirSync('/data/other');fs.symlinkSync('target','/data/link')",
        ],
        HostConfig: {
          Mounts: [{ Type: "volume", Source: name, Target: "/data" }],
        },
      });
      let proof: Awaited<ReturnType<typeof createMountProof>> | undefined;
      let data: Docker.Container | undefined;
      try {
        await setup.start();
        await setup.wait();
        proof = await createMountProof([
          {
            Type: "bind",
            Source: `${mountpoint}/target`,
            Destination: "/data",
            RW: true,
          },
        ]);
        const fixture = (source: string) =>
          docker.createContainer({
            Image: image,
            Cmd: ["bun", "-e", "setInterval(() => {}, 3600000)"],
            HostConfig: {
              Mounts: [{ Type: "bind", Source: source, Target: "/data" }],
            },
          });
        data = await fixture(`${mountpoint}/target`);
        await data.start();
        await assertMountIdentities(data, proof.identities);
        await assert.rejects(
          createMountProof([
            {
              Type: "bind",
              Source: `${mountpoint}/link`,
              Destination: "/data",
              RW: true,
            },
          ]),
          /could not be verified/,
        );
        await data.remove({ force: true });
        data = await fixture(`${mountpoint}/other`);
        await data.start();
        await assert.rejects(
          assertMountIdentities(data, proof.identities),
          /could not be verified/,
        );
      } finally {
        if (data) await data.remove({ force: true });
        if (proof) await proof.cleanup();
        await setup.remove({ force: true });
        await volume.remove();
      }
    }, 120_000);
  },
);
