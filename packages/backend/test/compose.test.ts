import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";
import {
  mkdtemp,
  mkdir,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import {
  approvedPath,
  configuredRoots,
  readApprovedFile,
} from "../src/approved-paths.js";
import {
  composeEnvironment,
  createComposeSnapshot,
  runCompose,
  validateUpdateService,
} from "../src/compose.js";
import {
  updateRequestSchema,
  serverGrantsSchema,
  scheduleSchema,
} from "@ludock/shared";

let directory: string;
const priorPath = process.env.PATH;
const priorRoots = process.env.LUDOCK_COMPOSE_ROOTS;
before(async () => {
  directory = await mkdtemp(path.join(os.tmpdir(), "ludock-compose-test-"));
  await mkdir(path.join(directory, "bin"));
  await writeFile(
    path.join(directory, "bin/docker"),
    `#!${process.execPath}
const fs = require("node:fs");
const args = process.argv.slice(2);
if(args.includes("secret-failure")){process.stderr.write("password=fixture-secret");process.exit(1);}
if(args.includes("hang")){setTimeout(()=>{},30000);}
else if(args.includes("config")){
  const files = args.flatMap((arg,i)=>arg==="-f"?[args[i+1]]:[]);
  const models=files.map(file=>JSON.parse(fs.readFileSync(file,"utf8")));
  const model={services:Object.assign({},...models.map(model=>model.services)),fixtureFiles:files,fixtureEnv:process.env};
  process.stdout.write(JSON.stringify(model));
}else process.stdout.write(JSON.stringify({args,env:process.env}));
`,
    { mode: 0o700 },
  );
  process.env.PATH = `${path.join(directory, "bin")}${path.delimiter}${priorPath}`;
  process.env.LUDOCK_COMPOSE_ROOTS = directory;
});
after(async () => {
  process.env.PATH = priorPath;
  if (priorRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
  else process.env.LUDOCK_COMPOSE_ROOTS = priorRoots;
  await rm(directory, { recursive: true, force: true });
});

describe("Compose execution boundary", () => {
  it("uses the configured daemon with no inherited secrets or Compose overrides", async () => {
    process.env.LUDOCK_PRIVATE_TEST_SECRET = "never-inherit";
    process.env.COMPOSE_FILE = "/unapproved/compose.yaml";
    try {
      const env = composeEnvironment();
      assert.equal(env.LUDOCK_PRIVATE_TEST_SECRET, undefined);
      assert.equal(env.COMPOSE_FILE, undefined);
      assert.equal(env.COMPOSE_DISABLE_ENV_FILE, "true");
      const result = JSON.parse(await runCompose(["version", "--short"])) as {
        args: string[];
        env: Record<string, string>;
      };
      assert.deepEqual(result.args, ["compose", "version", "--short"]);
      assert.equal(result.env.LUDOCK_PRIVATE_TEST_SECRET, undefined);
      assert.match(result.env.DOCKER_HOST, /^unix:\//);
    } finally {
      delete process.env.LUDOCK_PRIVATE_TEST_SECRET;
      delete process.env.COMPOSE_FILE;
    }
  });
  it("bounds subprocess execution and never exposes raw diagnostics", async () => {
    await assert.rejects(
      runCompose(["secret-failure"]),
      (error) =>
        error instanceof Error &&
        !error.message.includes("fixture-secret") &&
        error.message.includes("Docker Compose failed"),
    );
    await assert.rejects(runCompose(["hang"], 30), /execution limit/);
  });
  it("rejects broad roots, prefix escapes and unsupported service replication", () => {
    assert.throws(() => configuredRoots("/"), /dedicated/);
    assert.throws(
      () => approvedPath("/games-other/compose.yaml", ["/games"]),
      /outside/,
    );
    assert.throws(() => approvedPath("../secret", ["/games"]), /absolute/);
    assert.throws(
      () => validateUpdateService({ scale: 2 }),
      /one configured replica/,
    );
    assert.throws(
      () => validateUpdateService({ deploy: { replicas: 2 } }),
      /one configured replica/,
    );
    assert.throws(
      () => validateUpdateService({ network_mode: "service:db" }),
      /namespace/,
    );
    assert.doesNotThrow(() =>
      validateUpdateService({ depends_on: ["db"], profiles: ["games"] }),
    );
  });
  it(
    "pins safe file parents and rejects symlink inputs",
    { skip: process.platform !== "linux" },
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
      assert.match(
        (
          await readApprovedFile(path.join(directory, "safe/config.yaml"), [
            directory,
          ])
        ).toString(),
        /services/,
      );
      await assert.rejects(
        readApprovedFile(path.join(directory, "linked/config.yaml"), [
          directory,
        ]),
        /symbolic link/,
      );
      await assert.rejects(
        readApprovedFile(
          path.join(directory, "safe/config.yaml"),
          [directory],
          2,
        ),
        /size limit/,
      );
    },
  );
  it(
    "rejects named-pipe inputs without waiting for a writer",
    { skip: process.platform !== "linux", timeout: 2000 },
    async () => {
      const fifo = path.join(directory, "blocked.yaml");
      await promisify(execFile)("mkfifo", [fifo]);
      await assert.rejects(
        readApprovedFile(fifo, [directory]),
        /regular files/,
      );
    },
  );
  it(
    "snapshots ordered sources, strips build contexts and hashes source changes",
    { skip: process.platform !== "linux" },
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
        const services = snapshot.model.services as Record<
          string,
          Record<string, unknown>
        >;
        assert.equal(services.game.build, undefined);
        assert.deepEqual(Object.keys(services), ["game", "dependency"]);
        const resolved = await readFile(snapshot.configPath, "utf8");
        assert.match(resolved, /cash\$\$value/);
        assert.doesNotMatch(resolved, /must-not-read/);
        await writeFile(
          first,
          (await readFile(first, "utf8")) + "\n# owner source changed\n",
        );
        const next = await createComposeSnapshot(input);
        try {
          assert.notEqual(next.fingerprint, snapshot.fingerprint);
        } finally {
          await next.cleanup();
        }
      } finally {
        await snapshot.cleanup();
      }
    },
  );
  it(
    "rejects file-reading Compose features before spawning config",
    { skip: process.platform !== "linux" },
    async () => {
      for (const yaml of [
        "include: /etc/shadow\nservices: {}",
        "services:\n  game:\n    build: /etc",
        "services:\n  game:\n    image: alpine\n    label_file: /etc/shadow",
        "services:\n  game:\n    image: alpine\n    post_start: []",
      ]) {
        const filename = path.join(directory, "unsupported.yaml");
        await writeFile(filename, yaml);
        await assert.rejects(
          createComposeSnapshot({
            projectName: "fixture",
            projectDirectory: directory,
            composeFiles: [filename],
            envFiles: [],
          }),
          /outside the supported|Build-only/,
        );
      }
    },
  );
});

describe("shared request contracts", () => {
  it("rejects coercion, unknown mutation fields and unscoped identifiers", () => {
    assert.equal(
      updateRequestSchema.safeParse({
        createBackup: true,
        forceRecreate: "false",
      }).success,
      false,
    );
    assert.equal(
      updateRequestSchema.safeParse({ createBackup: true, command: "rm" })
        .success,
      false,
    );
    assert.equal(
      updateRequestSchema.parse({ createBackup: true }).forceRecreate,
      false,
    );
    assert.equal(
      serverGrantsSchema.safeParse({
        grants: [
          { serverId: "docker-physical-id", capabilities: ["server.view"] },
        ],
      }).success,
      false,
    );
    assert.equal(
      scheduleSchema.safeParse({
        action: "update",
        time: "12:00",
        days: [1],
        timezone: "UTC",
      }).success,
      false,
    );
    assert.equal(
      scheduleSchema.safeParse({
        action: "stop",
        time: "12:00",
        days: [1],
        timezone: "not-a-zone",
      }).success,
      false,
    );
  });
});
