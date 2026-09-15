import { expect } from "bun:test";
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
    options?: {
      input?: Uint8Array;
      rejection?: string;
      preludeData?: Record<string, string>;
    },
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
      expect(remaining > 0, "Restore helper exceeded the test's helper deadline").toBeTruthy();
      const child = Bun.spawn(
        [process.execPath, "-e", script, JSON.stringify(request)],
        {
          // Race hooks are static programs; fixture paths travel as data.
          env: {
            ...process.env,
            LUDOCK_RESTORE_TEST_DATA: JSON.stringify(options.preludeData ?? {}),
          },
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
        expect(child.signalCode, `Restore helper was killed: ${child.signalCode}`).toBe(null);
        if (options.rejection !== undefined) {
          expect(exitCode, "Unsafe operation did not reject normally").toBe(1);
          expect(stderr).toBe(options.rejection);
          expect(stdout).toBe("");
        } else {
          expect(exitCode, stderr).toBe(0);
        }
        return stdout;
      } finally {
        if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
        await child.exited;
      }
    };
    async function sentinel() {
      expect(await readFile(path.join(outside, "sentinel"), "utf8")).toBe("outside-data");
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
