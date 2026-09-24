import { decodeText } from "../src/bytes.js";
import { rejectedBy } from "./fixtures/errors.js";
import { expect, afterAll as after, beforeAll as before, describe, it, spyOn, afterEach, mock } from "bun:test";
import {
  mkdtemp,
  mkdir,
  open,
  readFile,
  rm,
  symlink,
  writeFile,
  type FileHandle,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import {
  approvedPath,
  configuredRoots,
  readApprovedFile,
  readOptionalApprovedFile,
} from "../src/approved-paths.js";
import {
  composeEnvironment,
  isComposeAvailable,
  createComposeSnapshot,
  runCompose,
  validateUpdateService,
  validatedProject,
} from "../src/compose.js";
import { COMPOSE_SOURCE_LABEL, COMPOSE_CONFIG_FILES_LABEL, COMPOSE_WORKING_DIR_LABEL } from "../src/compose-source.js";
import { docker } from "../src/docker-client.js";
import type { ServerContext } from "../src/servers.js";
import { closeDatabase } from "../src/database.js";
import {
  updateRequestSchema,
  serverGrantsSchema,
  scheduleSchema,
} from "@ludock/shared";

let directory: string;
const priorPath = process.env.PATH;
const priorRoots = process.env.LUDOCK_COMPOSE_ROOTS;
process.env.LUDOCK_DB_PATH = ":memory:";
afterEach(() => mock.restore());

before(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "ludock-compose-test-"));
  await mkdir(path.join(directory, "bin"));
  await writeFile(
    path.join(directory, "bin/docker"),
    `#!${process.execPath}
const fs = require("node:fs");
const args = process.argv.slice(2);
if(args.includes("secret-failure")){process.stderr.write("password=fixture-secret");process.exit(1);}
if(args.includes("large-output")){await Bun.write(Bun.stdout, new Uint8Array(9 * 1024 * 1024));}
else if(args.includes("orphaned-plugin")){
  const marker = args.at(-1);
  const script = 'const fs=require("node:fs");const marker=process.argv[1];fs.writeFileSync(marker+".pid",String(process.pid));process.on("SIGTERM",()=>setTimeout(()=>{fs.writeFileSync(marker,"stopped");process.exit(0);},50));setTimeout(()=>{},30000);';
  Bun.spawn([process.execPath,"-e",script,marker],{stdout:"inherit",stderr:"inherit"});
  process.exit(0);
}
else if(args.includes("hang")){setTimeout(()=>{},30000);}
else if(args.includes("config")){
  const files = args.flatMap((arg,i)=>arg==="-f"?[args[i+1]]:[]);
  const models=files.map(file=>JSON.parse(fs.readFileSync(file,"utf8")));
  const model={services:Object.assign({},...models.map(model=>model.services)),fixtureFiles:files,fixtureEnv:process.env,fixtureEnvFiles:args.flatMap((arg,i)=>arg==="--env-file"?[fs.readFileSync(args[i+1],"utf8")]:[])};
  process.stdout.write(JSON.stringify(model));
}else process.stdout.write(JSON.stringify({args,env:process.env}));
`,
    { mode: 0o700 },
  );
  process.env.PATH = `${path.join(directory, "bin")}${path.delimiter}${priorPath}`;
  process.env.LUDOCK_COMPOSE_ROOTS = directory;
});
after(async () => {
  closeDatabase();
  process.env.PATH = priorPath;
  if (priorRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
  else process.env.LUDOCK_COMPOSE_ROOTS = priorRoots;
  await rm(directory, { recursive: true, force: true });
});

describe("Compose execution boundary", () => {
  it("does not probe Compose without supported, approved roots", async () => {
    const currentRoots = process.env.LUDOCK_COMPOSE_ROOTS;
    const spawn = spyOn(Bun, "spawn").mockImplementation(() => {
      throw new Error("An unavailable Compose setup must not launch a process");
    });
    try {
      const roots = [undefined, "", "  ", "/", "relative"];
      if (process.platform !== "linux") roots.push(directory);
      for (const value of roots) {
        if (value === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
        else process.env.LUDOCK_COMPOSE_ROOTS = value;
        expect(await isComposeAvailable()).toBe(false);
      }
      expect(spawn.mock.calls.length).toBe(0);
    } finally {
      if (currentRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
      else process.env.LUDOCK_COMPOSE_ROOTS = currentRoots;
      spawn.mockRestore();
    }
  });

  it.skipIf(process.platform !== "linux")("reports availability after a successful configured probe", async () => {
    expect(await isComposeAvailable()).toBe(true);
  });

  it.skipIf(process.platform !== "linux")("rechecks approved roots after the probe finishes", async () => {
    const currentRoots = process.env.LUDOCK_COMPOSE_ROOTS;
    const probe = isComposeAvailable();
    delete process.env.LUDOCK_COMPOSE_ROOTS;
    try {
      expect(await probe).toBe(false);
    } finally {
      if (currentRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
      else process.env.LUDOCK_COMPOSE_ROOTS = currentRoots;
    }
  });

  it.skipIf(process.platform !== "linux")("updates discovered sources repeatedly and accepts edits without registration", async () => {
    const filename = path.join(directory, "automatic.yaml");
    await writeFile(filename, "services:\n  game:\n    image: alpine:latest\n");
    const context = {
      observation: { compose: { project: "automatic", service: "game", containerNumber: "1" },
        composeSourceLabels: { [COMPOSE_WORKING_DIR_LABEL]: directory, [COMPOSE_CONFIG_FILES_LABEL]: filename } },
      container: { id: "a".repeat(64) },
    } as unknown as ServerContext;
    spyOn(docker, "listContainers").mockResolvedValue([
      { Id: context.container.id, Labels: {} },
    ] as unknown as Awaited<ReturnType<typeof docker["listContainers"]>>);
    const first = await validatedProject(context);
    const model = JSON.parse(await readFile(first.snapshot.configPath, "utf8"));
    const originalSource = model.services.game.labels[COMPOSE_SOURCE_LABEL].replaceAll("$$", "$");
    const firstFingerprint = first.snapshot.fingerprint;
    await first.snapshot.cleanup();
    context.observation.composeSourceLabels = {
      [COMPOSE_SOURCE_LABEL]: originalSource,
      [COMPOSE_CONFIG_FILES_LABEL]: first.snapshot.configPath,
    };
    await writeFile(filename, "services:\n  game:\n    image: alpine:latest\n# edited by owning manager\n");
    const next = await validatedProject(context);
    try {
      expect(next.service).toBe("game");
      expect(next.image).toBe("alpine:latest");
      expect(next.snapshot.fingerprint).not.toBe(firstFingerprint);
    } finally { await next.snapshot.cleanup(); }
    // A discovered source is still subject to supported-service and path checks.
    await writeFile(filename, "services:\n  game:\n    image: alpine:latest\n    scale: 2\n");
    await expect(validatedProject(context)).rejects.toThrow(/one configured replica/);
    context.observation.composeSourceLabels = {
      [COMPOSE_WORKING_DIR_LABEL]: directory, [COMPOSE_CONFIG_FILES_LABEL]: "/outside/compose.yaml",
    };
    await expect(validatedProject(context)).rejects.toThrow(/cannot read this server/);
  });

  it.skipIf(process.platform !== "linux")("snapshots default .env safely and never falls back from a missing explicit env file", async () => {
    const folder = path.join(directory, "default-env");
    await mkdir(folder);
    const filename = path.join(folder, "compose.yaml");
    const env = path.join(folder, ".env");
    await writeFile(filename, "services:\n  game:\n    image: alpine:latest\n");
    const input = { projectName: "env", projectDirectory: folder, composeFiles: [filename], envFiles: [] };
    const missing = await createComposeSnapshot(input, true);
    await missing.cleanup();
    await writeFile(env, "WORLD=fixture\n");
    const loaded = await createComposeSnapshot(input, true);
    try {
      expect(loaded.model.fixtureEnvFiles).toStrictEqual(["WORLD=fixture\n"]);
      expect(loaded.fingerprint).not.toBe(missing.fingerprint);
      expect(JSON.stringify((loaded.model.services as Record<string, { labels: unknown }>).game.labels)).not.toMatch(/WORLD|fixture/);
    } finally { await loaded.cleanup(); }
    await expect(createComposeSnapshot({ ...input, envFiles: ["missing.env"] }, true)).rejects.toThrow(/missing/);
    await rm(env);
    await symlink(filename, env);
    await expect(createComposeSnapshot(input, true)).rejects.toThrow(/symbolic link/);
    await expect(readOptionalApprovedFile(path.join(folder, "missing-parent/.env"), [directory])).rejects.toThrow(/missing/);
    await expect(readOptionalApprovedFile("/outside/.env", [directory])).rejects.toThrow(/outside/);
  });

  it("uses the configured daemon with no inherited secrets or Compose overrides", async () => {
    process.env.LUDOCK_PRIVATE_TEST_SECRET = "never-inherit";
    process.env.COMPOSE_FILE = "/unapproved/compose.yaml";
    try {
      const env = composeEnvironment();
      expect(env.LUDOCK_PRIVATE_TEST_SECRET).toBe(undefined);
      expect(env.COMPOSE_FILE).toBe(undefined);
      expect(env.COMPOSE_DISABLE_ENV_FILE).toBe("true");
      const result = JSON.parse(await runCompose(["version", "--short"])) as {
        args: string[];
        env: Record<string, string>;
      };
      expect(result.args).toStrictEqual(["compose", "version", "--short"]);
      expect(Object.hasOwn(result.env, "LUDOCK_PRIVATE_TEST_SECRET")).toBe(false);
      expect(result.env.DOCKER_HOST).toMatch(/^unix:\//);
    } finally {
      delete process.env.LUDOCK_PRIVATE_TEST_SECRET;
      delete process.env.COMPOSE_FILE;
    }
  });
  it("bounds subprocess execution and never exposes raw diagnostics", async () => {
    await expect(await rejectedBy(runCompose(["secret-failure"]))).toSatisfy((error) =>
        error instanceof Error &&
        !error.message.includes("fixture-secret") &&
        error.message.includes("Docker Compose failed"));
    await expect(runCompose(["hang"], 30)).rejects.toThrow(/execution limit/);
  });
  it("passes shell metacharacters as literal arguments", async () => {
    const literal = "fixture; $(printf unsafe) `printf unsafe` > /unapproved";
    const result = JSON.parse(await runCompose(["version", literal])) as {
      args: string[];
    };
    expect(result.args).toStrictEqual(["compose", "version", literal]);
  });
  it("terminates output that exceeds the configuration size limit", async () => {
    await expect(runCompose(["large-output"])).rejects.toThrow(/execution limit/);
  });
  it("reports an unavailable executable without subprocess details", async () => {
    const currentPath = process.env.PATH;
    process.env.PATH = path.join(directory, "missing-bin");
    try {
      await expect(await rejectedBy(runCompose(["version"]))).toSatisfy((error) => error instanceof Error &&
          error.message === "Docker Compose could not be executed");
      expect(await isComposeAvailable()).toBe(false);
    } finally {
      process.env.PATH = currentPath;
    }
  });
  it.skipIf(process.platform === "win32")(
    "waits for a surviving plugin to terminate before releasing the operation",
    async () => {
      const marker = path.join(directory, "plugin-stopped");
      try {
        await expect(runCompose(["orphaned-plugin", marker], 5000)).rejects.toThrow(/execution limit/);
        expect(await readFile(marker, "utf8")).toBe("stopped");
      } finally {
        const pid = Number(await readFile(`${marker}.pid`, "utf8").catch(() => ""));
        if (pid > 0) {
          try { process.kill(pid, "SIGKILL"); } catch { /* Already exited. */ }
        }
      }
    },
    15_000,
  );
  it("rejects broad roots, prefix escapes and unsupported service replication", () => {
    expect(() => configuredRoots("/")).toThrow(/dedicated/);
    expect(() => approvedPath("/games-other/compose.yaml", ["/games"])).toThrow(/outside/);
    expect(() => approvedPath("../secret", ["/games"])).toThrow(/absolute/);
    expect(() => validateUpdateService({ scale: 2 })).toThrow(/one configured replica/);
    expect(() => validateUpdateService({ deploy: { replicas: 2 } })).toThrow(/one configured replica/);
    expect(() => validateUpdateService({ network_mode: "service:db" })).toThrow(/namespace/);
    expect(() =>
      validateUpdateService({ depends_on: ["db"], profiles: ["games"] })).not.toThrow();
  });
  it.skipIf(Boolean(process.platform !== "linux"))(
    "pins safe file parents and rejects symlink inputs",
    async () => {
      await mkdir(path.join(directory, "safe"));
      await writeFile(
        path.join(directory, "safe/config.yaml"),
        "services: {}\n",
      );
      await symlink(
        path.join(directory, "safe"),
        path.join(directory, "linked"),
      );
      expect(decodeText((
          await readApprovedFile(path.join(directory, "safe/config.yaml"), [
            directory,
          ])
        ))).toMatch(/services/);
      await expect(readApprovedFile(path.join(directory, "linked/config.yaml"), [
          directory,
        ])).rejects.toThrow(/symbolic link/);
      await expect(readApprovedFile(
          path.join(directory, "safe/config.yaml"),
          [directory],
          2,
        )).rejects.toThrow(/size limit/);
    },
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "rejects named-pipe inputs without waiting for a writer",
    async () => {
      const fifo = path.join(directory, "blocked.yaml");
      const fifoProcess = Bun.spawn(["mkfifo", fifo], {
        stdin: "ignore", stdout: "ignore", stderr: "ignore",
      });
      expect(await fifoProcess.exited).toBe(0);
      await expect(readApprovedFile(fifo, [directory])).rejects.toThrow(/regular files/);
    }, 2000,
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "snapshots ordered sources, strips build contexts and hashes source changes",
    async () => {
      const first = path.join(directory, "first.yaml"),
        second = path.join(directory, "second.yaml");
      await writeFile(
        first,
        "services:\n  game:\n    image: alpine:latest\n    build: /must-not-read\n    environment:\n      LITERAL: cash$$value\n",
      );
      await writeFile(
        second,
        "services:\n  dependency:\n    image: alpine:latest\n",
      );
      const input = {
        projectName: "fixture",
        projectDirectory: directory,
        composeFiles: [first, second],
        envFiles: [],
      };
      const snapshot = await createComposeSnapshot(input);
      try {
        expect(snapshot.fingerprint).toMatch(/^hmac-sha256:[a-f0-9]{64}$/);
        const services = snapshot.model.services as Record<
          string,
          Record<string, unknown>
        >;
        expect(services.game.build).toBe(undefined);
        expect(Object.keys(services)).toStrictEqual(["game", "dependency"]);
        const resolved = await readFile(snapshot.configPath, "utf8");
        expect(resolved).toMatch(/cash\$\$value/);
        expect(resolved).not.toMatch(/must-not-read/);
        await writeFile(
          first,
          (await readFile(first, "utf8")) + "\n# owner source changed\n",
        );
        const next = await createComposeSnapshot(input);
        try {
          expect(next.fingerprint).not.toBe(snapshot.fingerprint);
        } finally {
          await next.cleanup();
        }
      } finally {
        await snapshot.cleanup();
      }
    },
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "keeps environment-file fingerprints stable when read timings change",
    async () => {
      const filename = path.join(directory, "env-order.yaml");
      const firstEnv = path.join(directory, "first.env");
      const secondEnv = path.join(directory, "second.env");
      await writeFile(firstEnv, "WORLD=first\n");
      await writeFile(secondEnv, "WORLD=second\n");
      await writeFile(
        filename,
        "services:\n  game:\n    image: alpine:latest\n    env_file:\n      - first.env\n      - second.env\n",
      );
      const firstHandle = await open(firstEnv, "r");
      const secondHandle = await open(secondEnv, "r");
      const firstInode = (await firstHandle.stat()).ino;
      const secondInode = (await secondHandle.stat()).ino;
      const prototype = Object.getPrototypeOf(firstHandle) as FileHandle;
      const originalRead = prototype.read;
      await firstHandle.close();
      await secondHandle.close();
      let delayedInode = firstInode;
      spyOn(prototype, "read").mockImplementation((async function (
        this: FileHandle,
        buffer: Uint8Array,
        offset: number,
        length: number,
        position: number | null,
      ) {
        if ((await this.stat()).ino === delayedInode)
          await new Promise((resolve) => setTimeout(resolve, 40));
        return (originalRead as (...args: unknown[]) => Promise<unknown>)
          .call(this, buffer, offset, length, position);
      }) as unknown as typeof prototype.read);
      const input = {
        projectName: "fixture",
        projectDirectory: directory,
        composeFiles: [filename],
        envFiles: [],
      };
      const first = await createComposeSnapshot(input);
      try {
        delayedInode = secondInode;
        const second = await createComposeSnapshot(input);
        try {
          expect(second.fingerprint).toBe(first.fingerprint);
          for (const snapshot of [first, second]) {
            const services = snapshot.model.services as Record<
              string, { env_file: { path: string }[] }
            >;
            expect(await Promise.all(
                services.game.env_file.map((entry) => readFile(entry.path, "utf8")),
              )).toStrictEqual(["WORLD=first\n", "WORLD=second\n"]);
          }
        } finally {
          await second.cleanup();
        }
      } finally {
        await first.cleanup();
      }
    },
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "keys source fingerprints per installation and preserves them across reopen",
    async () => {
      const filename = path.join(directory, "keyed-source.yaml");
      const environment = path.join(directory, "keyed-source.env");
      await writeFile(
        filename,
        "services:\n  game:\n    image: alpine:latest\n    env_file: keyed-source.env\n",
      );
      await writeFile(environment, "PASSWORD=small-secret\n");
      const input = {
        projectName: "keyed-source",
        projectDirectory: directory,
        composeFiles: [filename],
        envFiles: [],
      };
      const firstDatabase = path.join(directory, "compose-key-first.db");
      closeDatabase();
      process.env.LUDOCK_DB_PATH = firstDatabase;
      const first = await createComposeSnapshot(input);
      try {
        const repeated = await createComposeSnapshot(input);
        try {
          expect(first.fingerprint).toMatch(/^hmac-sha256:[a-f0-9]{64}$/);
          expect(repeated.fingerprint).toBe(first.fingerprint);
        } finally {
          await repeated.cleanup();
        }
        closeDatabase();
        const reopened = await createComposeSnapshot(input);
        try {
          expect(reopened.fingerprint).toBe(first.fingerprint);
        } finally {
          await reopened.cleanup();
        }

        closeDatabase();
        process.env.LUDOCK_DB_PATH = path.join(
          directory,
          "compose-key-second.db",
        );
        const otherInstallation = await createComposeSnapshot(input);
        try {
          expect(otherInstallation.fingerprint).not.toBe(first.fingerprint);
        } finally {
          await otherInstallation.cleanup();
        }
      } finally {
        await first.cleanup();
        closeDatabase();
        process.env.LUDOCK_DB_PATH = ":memory:";
      }
    },
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "rejects duplicate keys and excessive YAML aliases before running Compose",
    async () => {
      const filename = path.join(directory, "unsafe-yaml.yaml");
      for (const yaml of [
        "services:\n  game:\n    image: first\n    image: second\n",
        `x-base: &base [a, b, c]\nx-repeated: [${Array(30).fill("*base").join(", ")}]\nservices:\n  game:\n    image: alpine\n`,
      ]) {
        await writeFile(filename, yaml);
        await expect(createComposeSnapshot({
            projectName: "fixture",
            projectDirectory: directory,
            composeFiles: [filename],
            envFiles: [],
          })).rejects.toThrow(/YAML could not be parsed safely/);
      }
    },
  );
  it.skipIf(Boolean(process.platform !== "linux"))(
    "rejects file-reading Compose features before spawning config",
    async () => {
      for (const yaml of [
        "include: /etc/shadow\nservices: {}",
        "services:\n  game:\n    build: /etc",
        "services:\n  game:\n    image: alpine\n    label_file: /etc/shadow",
        "services:\n  game:\n    image: alpine\n    post_start: []",
      ]) {
        const filename = path.join(directory, "unsupported.yaml");
        await writeFile(filename, yaml);
        await expect(createComposeSnapshot({
            projectName: "fixture",
            projectDirectory: directory,
            composeFiles: [filename],
            envFiles: [],
          })).rejects.toThrow(/outside the supported|Build-only/);
      }
    },
  );
});

describe("shared request contracts", () => {
  it("rejects coercion, unknown mutation fields and unscoped identifiers", () => {
    expect(updateRequestSchema.safeParse({
        createBackup: true,
        forceRecreate: "false",
      }).success).toBe(false);
    expect(updateRequestSchema.safeParse({ createBackup: true, command: "rm" })
        .success).toBe(false);
    expect(updateRequestSchema.parse({ createBackup: true }).forceRecreate).toBe(false);
    expect(serverGrantsSchema.safeParse({
        grants: [
          { serverId: "docker-physical-id", capabilities: ["server.view"] },
        ],
      }).success).toBe(false);
    expect(scheduleSchema.safeParse({
        action: "update",
        time: "12:00",
        days: [1],
        timezone: "UTC",
      }).success).toBe(false);
    expect(scheduleSchema.safeParse({
        action: "stop",
        time: "12:00",
        days: [1],
        timezone: "not-a-zone",
      }).success).toBe(false);
  });
});
