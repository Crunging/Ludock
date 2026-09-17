import { fixtureBytes } from "./fixtures/bytes.js";
import {
  mkdtemp,
  realpath,
  rm,
  readFile,
  writeFile,
  readdir,
} from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { expect, describe, it } from "bun:test";
import type * as Docker from "../src/docker-client.js";
import { getDockerInstance } from "../src/docker.js";
import { closeDatabase, createUser } from "../src/database.js";
import { setSetting } from "../src/settings.js";
import { listServers, resolveAuthorizedServer } from "../src/servers.js";
import {
  listBackups,
  getBackup,
  runBackup,
  runRestore,
  recoverRestore,
  stopForDataOperation,
} from "../src/backups.js";
import {
  createDataHelper,
  helperExec,
  helperRoot,
} from "../src/backup-storage.js";
import type { JobContext } from "../src/operations.js";
import { DEFAULT_HELPER_IMAGE } from "../src/runtime-images.js";

const enabled = process.env.LUDOCK_DOCKER_TESTS === "1";
function context(
  serverId: string,
  bindingRevision: number,
  kind: string,
  input: Record<string, unknown> = {},
): JobContext {
  const job: JobContext["job"] = {
    id: crypto.randomUUID(),
    serverId,
    actorId: "backup-test-admin",
    kind,
    status: "running",
    phase: "validating",
    createdAt: Date.now(),
    updatedAt: Date.now(),
    error: null,
    result: null,
    bindingRevision,
    input,
    recovery: {},
  };
  return {
    job,
    progress: (phase, patch) => {
      job.phase = phase;
      Object.assign(job.recovery, patch);
    },
  };
}

describe.skipIf(Boolean(!enabled || process.platform !== "linux"))(
  "Docker backup and restore",
    () => {
    it("copies stopped data, preserves state, rejects shared writers/links, and restores through a safety backup", async () => {
      process.env.LUDOCK_DB_PATH = ":memory:";
      const directory = await realpath(
        await mkdtemp(
          path.join(
            process.env.LUDOCK_TEST_BACKUP_DIRECTORY || tmpdir(),
            "ludock-backup-docker-",
          ),
        ),
      );
      const docker = getDockerInstance(),
        containers: Docker.Container[] = [],
        volumes: Docker.Volume[] = [];
      const oldRoots = process.env.LUDOCK_BACKUP_ROOTS,
        oldSelf = process.env.LUDOCK_SELF_CONTAINER;
      try {
        const volume = await docker.createVolume({
          Name: `ludock-test-${crypto.randomUUID()}`,
        });
        volumes.push(volume);
        const source = await docker.createContainer({
          Image: DEFAULT_HELPER_IMAGE,
          Entrypoint: [],
          name: `ludock-backup-test-${crypto.randomUUID()}`,
          Labels: { "ludock.enable": "true", "ludock.name": "Backup fixture" },
          Cmd: ["bun", "-e", "process.on('SIGTERM', () => process.exit(0)); setInterval(() => {}, 3600000)"],
          HostConfig: {
            // This idle fixture has no shutdown work. Forward signals even if
            // a second stop races Bun startup after restart under emulation.
            Init: true,
            Mounts: [{ Type: "volume", Source: volume.name, Target: "/data" }],
          },
        });
        containers.push(source);
        await source.start();
        await helperExec(source, [
          "bun",
          "-e",
          "const fs = require('node:fs'); fs.writeFileSync('/data/world.txt', 'original-world'); fs.mkdirSync('/data/settings'); fs.writeFileSync('/data/settings/server.cfg', 'original-config');",
        ]);
        const self = !oldSelf
          ? await docker.createContainer({
              Image: DEFAULT_HELPER_IMAGE,
              Entrypoint: [],
              Cmd: ["bun", "-e", ""],
              HostConfig: {
                Mounts: [
                  { Type: "bind", Source: directory, Target: directory },
                ],
              },
              Labels: { "ludock.enable": "false" },
            })
          : undefined;
        if (self) containers.push(self);
        process.env.LUDOCK_BACKUP_ROOTS = directory;
        process.env.LUDOCK_SELF_CONTAINER = oldSelf || self!.id;
        closeDatabase();
        const admin = {
          id: "backup-test-admin",
          username: "admin",
          role: "admin" as const,
        };
        createUser({
          ...admin,
          passwordHash: "not-a-password",
          disabled: false,
          createdAt: Date.now(),
        });
        setSetting("backups", {
          destination: directory,
          retentionCount: 1,
          maxBytes: 20_000_000,
          reserveBytes: 1_000_000,
        });
        const server = (await listServers(admin)).find(
          (entry) => entry.displayName === "Backup fixture",
        )!;
        expect(server).toBeTruthy();
        let state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.create",
        );

        const aliased = await docker.createContainer({
          Image: DEFAULT_HELPER_IMAGE,
          Entrypoint: [],
          Cmd: ["bun", "-e", ""],
          Labels: {
            "ludock.enable": "true",
            "ludock.name": "Aliased backup fixture",
          },
          HostConfig: {
            Mounts: [
              { Type: "volume", Source: volume.name, Target: "/first-data" },
              { Type: "volume", Source: volume.name, Target: "/second-data" },
            ],
          },
        });
        containers.push(aliased);
        const aliasServer = (await listServers(admin)).find(
          (entry) => entry.displayName === "Aliased backup fixture",
        )!;
        const aliasState = await resolveAuthorizedServer(
          admin,
          aliasServer.id,
          "backups.create",
        );
        await expect(createDataHelper(aliasState, true, crypto.randomUUID())).rejects.toThrow(/overlap on the Docker host/);
        await aliased.remove();
        containers.splice(containers.indexOf(aliased), 1);
        await helperExec(source, [
          "bun",
          "-e",
          'const fs=require("node:fs");fs.writeFileSync("/data/high-owner","metadata");fs.chownSync("/data/high-owner",1000000,2000000);fs.utimesSync("/data/high-owner",2208988800,2208988800);',
        ]);
        const backupJob = context(
          server.id,
          state.logical.bindingRevision,
          "backup",
        );
        const result = await runBackup(backupJob);
        const backup = getBackup(server.id, String(result.backupId));
        expect("roots" in result).toBe(false);
        expect("backup" in result).toBe(false);
        expect(backup.state).toBe("complete");
        expect((await source.inspect()).State.Running).toBe(true);
        expect(backupJob.job.recovery.stateRestored).toBe(true);
        // Capacity failure while streaming cannot publish an incomplete archive
        // or leave a previously running server stopped.
        setSetting("backups", {
          destination: directory,
          retentionCount: 1,
          maxBytes: backup.size + 1024,
          reserveBytes: 1_000_000,
        });
        await expect(runBackup(
            context(server.id, state.logical.bindingRevision, "backup"),
          )).rejects.toThrow(/byte limit/);
        expect((await source.inspect()).State.Running).toBe(true);
        expect(listBackups(server.id).length).toBe(1);
        expect((await readdir(directory)).some((name) => name.endsWith(".partial"))).toBe(false);
        setSetting("backups", {
          destination: directory,
          retentionCount: 1,
          maxBytes: 20_000_000,
          reserveBytes: Number.MAX_SAFE_INTEGER,
        });
        await expect(runBackup(
            context(server.id, state.logical.bindingRevision, "backup"),
          )).rejects.toThrow(/free space/);
        expect((await source.inspect()).State.Running).toBe(true);
        setSetting("backups", {
          destination: directory,
          retentionCount: 1,
          maxBytes: 20_000_000,
          reserveBytes: 1_000_000,
        });
        const archivePath = path.join(directory, `${backup.id}.tar`),
          originalArchive = await readFile(archivePath);
        const changedArchive = fixtureBytes(originalArchive);
        changedArchive[changedArchive.length - 1] = 1;
        await writeFile(archivePath, changedArchive);
        await expect(runRestore(
            context(server.id, state.logical.bindingRevision, "restore", {
              backupId: backup.id,
              confirmation: "Backup fixture",
            }),
          )).rejects.toThrow();
        expect((await source.inspect()).State.Running, "invalid restore is rejected before stopping").toBe(true);
        await writeFile(archivePath, originalArchive);
        await helperExec(source, [
          "bun",
          "-e",
          "const fs = require('node:fs'); fs.writeFileSync('/data/world.txt', 'changed-world'); fs.writeFileSync('/data/new.txt', 'new-file');",
        ]);
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.restore",
        );
        const restored = await runRestore(
          context(server.id, state.logical.bindingRevision, "restore", {
            backupId: backup.id,
            confirmation: "Backup fixture",
          }),
        );
        expect(restored.restoredBackupId).toBe(backup.id);
        expect((await source.inspect()).State.Running).toBe(true);
        expect(await helperExec(source, ["bun", "-e", "process.stdout.write(require('node:fs').readFileSync('/data/world.txt'))"])).toBe("original-world");
        expect(await helperExec(source, [
            "bun",
            "-e",
            'const s=require("node:fs").statSync("/data/high-owner");process.stdout.write(JSON.stringify([s.uid,s.gid,s.mtimeMs/1000]));',
          ])).toBe("[1000000,2000000,2208988800]");
        expect(await helperExec(source, [
            "bun",
            "-e",
            "if (require('node:fs').existsSync('/data/new.txt')) process.exit(1); process.stdout.write('ok');",
          ])).toBe("ok");
        expect(listBackups(server.id).some(
            (entry) => entry.id === restored.safetyBackupId,
          )).toBeTruthy();
        expect(listBackups(server.id).some((entry) => entry.id === backup.id), "retention must not delete the restore target during its safety backup").toBeTruthy();

        await helperExec(source, [
          "bun",
          "-e",
          "require('node:fs').symlinkSync('/etc/passwd', '/data/unsafe-link')",
        ]);
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.create",
        );
        const beforeFailure = listBackups(server.id).length;
        await expect(runBackup(
            context(server.id, state.logical.bindingRevision, "backup"),
          )).rejects.toThrow(/symbolic links/);
        expect((await source.inspect()).State.Running, "copy failure restores initial running state").toBe(true);
        expect(listBackups(server.id).length).toBe(beforeFailure);
        await helperExec(source, ["bun", "-e", "require('node:fs').unlinkSync('/data/unsafe-link')"]);

        const writer = await docker.createContainer({
          Image: DEFAULT_HELPER_IMAGE,
          Entrypoint: [],
          Cmd: ["bun", "-e", "setInterval(() => {}, 3600000)"],
          HostConfig: {
            Mounts: [
              { Type: "volume", Source: volume.name, Target: "/shared" },
            ],
          },
          Labels: { "ludock.enable": "false" },
        });
        containers.push(writer);
        await writer.start();
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.create",
        );
        await expect(runBackup(
            context(server.id, state.logical.bindingRevision, "backup"),
          )).rejects.toThrow(/Another running container/);
        expect((await source.inspect()).State.Running, "shared writer check happens before stopping").toBe(true);
        await writer.remove({ force: true });
        containers.splice(containers.indexOf(writer), 1);
        await source.stop({ t: 5 });
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.create",
        );
        const stoppedJob = context(
          server.id,
          state.logical.bindingRevision,
          "backup",
        );
        await runBackup(stoppedJob);
        expect((await source.inspect()).State.Running, "initially stopped server stays stopped").toBe(false);
        expect(listBackups(server.id).length, "ordinary completed backups enforce retention after publishing").toBe(1);

        // Model an interrupted rollback: world.txt has already returned from old/
        // while settings/ is still staged. Recovery must not delete world.txt by
        // replaying the earlier remove-new-data phase.
        await source.start();
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.restore",
        );
        const interrupted = context(
          server.id,
          state.logical.bindingRevision,
          "restore",
        );
        await stopForDataOperation(state, interrupted);
        const helper = await createDataHelper(state, false, interrupted.job.id);
        const root = helper.roots[0],
          rootPath = helperRoot(state, root),
          stage = `.ludock-restore-${interrupted.job.id}`;
        await helperExec(
          helper.container,
          [
            "bun",
            "-e",
            "const fs = require('node:fs'); const root = process.env.ROOT; const stage = root + '/' + process.env.STAGE; fs.mkdirSync(stage); fs.mkdirSync(stage + '/new'); fs.mkdirSync(stage + '/old'); fs.renameSync(root + '/settings', stage + '/old/settings');",
          ],
          { ROOT: rootPath, STAGE: stage },
        );
        interrupted.progress("restore_rolling_back", {
          dataSafe: false,
          dataHelperId: helper.container.id,
          restoreRoots: [{ root, phase: "rolling_back" }],
        });
        // Leave the helper alive as a crashed Ludock process would; recovery must
        // remove this known operation helper before shared-writer validation.
        await recoverRestore(interrupted);
        expect((await source.inspect()).State.Running).toBe(true);
        expect(await helperExec(source, ["bun", "-e", "process.stdout.write(require('node:fs').readFileSync('/data/world.txt'))"])).toBe("original-world");
        expect(await helperExec(source, ["bun", "-e", "process.stdout.write(require('node:fs').readFileSync('/data/settings/server.cfg'))"])).toBe("original-config");
        expect(interrupted.job.recovery.dataSafe).toBe(true);
        expect(await helperExec(
            source,
            ["bun", "-e", "if (require('node:fs').existsSync(process.env.STAGE)) process.exit(1); process.stdout.write('clean');"],
            { STAGE: `/data/${stage}` },
          )).toBe("clean");

        // A crash after the old files have returned and cleanup has removed the
        // stage must safely resume from the durable rolled_back journal phase.
        state = await resolveAuthorizedServer(
          admin,
          server.id,
          "backups.restore",
        );
        const cleaned = context(
          server.id,
          state.logical.bindingRevision,
          "restore",
        );
        await stopForDataOperation(state, cleaned);
        cleaned.progress("restore_rolled_back", {
          dataSafe: false,
          restoreRoots: [{ root, phase: "rolled_back" }],
        });
        await recoverRestore(cleaned);
        expect((await source.inspect()).State.Running).toBe(true);
        expect(await helperExec(source, ["bun", "-e", "process.stdout.write(require('node:fs').readFileSync('/data/world.txt'))"])).toBe("original-world");
        expect(cleaned.job.recovery.restoreRoots).toStrictEqual([]);
        // Recovery is also idempotent after the restart was already completed.
        await recoverRestore(cleaned);
        expect((await source.inspect()).State.Running).toBe(true);
        const unpublishedId = crypto.randomUUID();
        await writeFile(
          path.join(directory, `${unpublishedId}.tar.partial`),
          "interrupted-safety-backup",
        );
        const early = context(
          server.id,
          state.logical.bindingRevision,
          "restore",
        );
        await stopForDataOperation(state, early);
        early.progress("backing_up", {
          backupId: unpublishedId,
          backupDestination: directory,
        });
        await recoverRestore(early);
        expect((await source.inspect()).State.Running).toBe(true);
        expect((await readdir(directory)).some((name) =>
            name.includes(unpublishedId),
          )).toBe(false);
      } finally {
        for (const container of containers.reverse())
          await container.remove({ force: true, v: true }).catch(() => {});
        for (const volume of volumes) await volume.remove().catch(() => {});
        closeDatabase();
        if (oldRoots === undefined) delete process.env.LUDOCK_BACKUP_ROOTS;
        else process.env.LUDOCK_BACKUP_ROOTS = oldRoots;
        if (oldSelf === undefined) delete process.env.LUDOCK_SELF_CONTAINER;
        else process.env.LUDOCK_SELF_CONTAINER = oldSelf;
        await rm(directory, { recursive: true, force: true });
      }
    }, 120_000);
  },
);
