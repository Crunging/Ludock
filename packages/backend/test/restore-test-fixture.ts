import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";

export type RestoreFixture = {
  directory: string;
  root: string;
  outside: string;
  runScript: (
    script: string,
    request: Record<string, unknown>,
    options?: { input?: Buffer; rejection?: string },
  ) => Promise<string>;
  sentinel: () => Promise<void>;
};

export function withRestoreFixture(body: (fixture: RestoreFixture) => Promise<void>) {
  return async () => {
    // Reserve time to reap a stalled helper and remove its fixture before Bun's
    // default five-second test deadline. Each invocation shares this budget.
    const deadline = performance.now() + 4_000;
    const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-restore-script-"));
    const root = path.join(directory, "root");
    const outside = path.join(directory, "outside");
    const runScript: RestoreFixture["runScript"] = async (script, request, options = {}) => {
      const remaining = deadline - performance.now();
      assert.ok(remaining > 0, "Restore helper exceeded the test's helper deadline");
      const child = Bun.spawn(
        [process.execPath, "-e", script, JSON.stringify(request)],
        {
          stdin: options.input ?? "ignore",
          stdout: "pipe",
          stderr: "pipe",
          timeout: Math.ceil(remaining),
          killSignal: "SIGKILL",
        },
      );
      try {
        const [exitCode, stdout, stderr] = await Promise.all([
          child.exited,
          new Response(child.stdout).text(),
          new Response(child.stderr).text(),
        ]);
        assert.equal(child.signalCode, null, `Restore helper was killed: ${child.signalCode}`);
        if (options.rejection !== undefined) {
          assert.equal(exitCode, 1, "Unsafe operation did not reject normally");
          assert.equal(stderr, options.rejection);
          assert.equal(stdout, "");
        } else {
          assert.equal(exitCode, 0, stderr);
        }
        return stdout;
      } finally {
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
        await child.exited;
      }
    };
    async function sentinel() {
      assert.equal(
        await readFile(path.join(outside, "sentinel"), "utf8"),
        "outside-data",
      );
    }
    try {
      await mkdir(root);
      await mkdir(outside);
      await writeFile(path.join(outside, "sentinel"), "outside-data");
      await body({ directory, root, outside, runScript, sentinel });
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  };
}
