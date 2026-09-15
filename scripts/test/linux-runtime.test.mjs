import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { expect, describe, it } from "bun:test";

const harness = Bun.fileURLToPath(new URL("../test-linux.mjs", import.meta.url));

async function runHarness(scenario) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-linux-harness-test-"));
  const log = path.join(directory, "commands.jsonl");
  try {
    await writeFile(path.join(directory, "docker"), `#!/usr/bin/env bun
import { appendFileSync } from "node:fs";
const args = process.argv.slice(2);
appendFileSync(process.env.LUDOCK_COMMAND_LOG, JSON.stringify(args) + "\\n");
if (args[0] === "inspect") console.log(process.env.LUDOCK_SCENARIO === "unclean-stop" ? 137 : 0);
if (args[0] === "exec" && process.env.LUDOCK_SCENARIO === "failed-smoke") process.exit(7);
if (args[0] === "run" && args.includes("test") && process.env.LUDOCK_SCENARIO === "failed-suite") process.exit(9);
`, { mode: 0o700 });
    const result = Bun.spawnSync([process.execPath, harness], {
      env: {
        ...process.env,
        PATH: [directory, path.dirname(process.execPath), process.env.PATH].join(path.delimiter),
        LUDOCK_TEST_IMAGE: "ludock:fixture",
        LUDOCK_COMMAND_LOG: log,
        LUDOCK_SCENARIO: scenario,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const commands = (await readFile(log, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
    return { result, commands };
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

describe("Linux production harness", () => {
  it("checks the default image command before source suites and cleans up fixture volumes", async () => {
    const { result, commands } = await runHarness("success");
    expect(result.exitCode, result.stderr.toString()).toBe(0);
    const smoke = commands[0];
    expect(smoke.at(-1)).toBe("ludock:fixture");
    expect(!smoke.includes("--entrypoint") && !smoke.includes("-v") && !smoke.includes("-p")).toBeTruthy();
    expect(smoke[smoke.indexOf("--network") + 1]).toBe("none");
    expect(commands.map((command) => command[0])).toStrictEqual(["run", "exec", "stop", "inspect", "run", "rm"]);
    expect(commands[4].includes("test")).toBeTruthy();
    const mounts = commands[4].filter((argument, index, args) => args[index - 1] === "-v");
    for (const directory of ["node_modules", "packages/backend/node_modules", "packages/shared/node_modules", "packages/shared/src"]) {
      expect(mounts.some((mount) => mount.endsWith(`:/app/${directory}:ro`))).toBe(true);
    }
    expect(commands.at(-1).slice(0, 2)).toStrictEqual(["rm", "-fv"]);
    expect(commands.at(-1)[2]).toBe(smoke[smoke.indexOf("--name") + 1]);
  });

  for (const scenario of ["failed-smoke", "unclean-stop"]) {
    it(`rejects ${scenario} before source tests and retains diagnostic output`, async () => {
      const { result, commands } = await runHarness(scenario);
      expect(result.exitCode).toBe(1);
      expect(!commands.some((command) => command[0] === "run" && command.includes("test"))).toBeTruthy();
      expect(commands.at(-2)[0]).toBe("logs");
      expect(commands.at(-1)[0]).toBe("rm");
    });
  }

  it("propagates source-suite failures and still removes its fixtures", async () => {
    const { result, commands } = await runHarness("failed-suite");
    expect(result.exitCode).toBe(9);
    expect(commands.at(-1)[0]).toBe("rm");
  });
});
