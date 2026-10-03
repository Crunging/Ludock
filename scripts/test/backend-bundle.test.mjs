import { describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildBackend } from "../build-backend.mjs";

describe("backend deployment bundle", () => {
  it("loads both entry points without installed packages", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-bundle-test-"));
    const output = join(directory, "dist");
    try {
      await buildBackend(output);
      const environment = { ...process.env, LUDOCK_DB_PATH: ":memory:", LUDOCK_RECOVERY_PASSWORD: "" };
      const run = (args) => Bun.spawnSync([process.execPath, "--no-install", ...args], {
        cwd: directory, env: environment, stdout: "pipe", stderr: "pipe",
      });
      const imported = run(["-e", `await import(${JSON.stringify(Bun.pathToFileURL(join(output, "index.js")).href)})`]);
      expect(imported.exitCode, new TextDecoder().decode(imported.stderr)).toBe(0);
      const recovery = run([join(output, "recovery.js")]);
      expect(recovery.exitCode).toBe(2);
      expect(new TextDecoder().decode(recovery.stderr)).toContain("Usage:");
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  });
});
