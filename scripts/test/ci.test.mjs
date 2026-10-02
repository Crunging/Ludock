import { describe, expect, it, spyOn } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { scanImage, TRIVY_IMAGE } from "../ci/containers.mjs";

describe("CI helpers", () => {
  it("scans exported images offline without giving the scanner Docker access, and cleans up failures", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-scanner-test-"));
    let failScan = false;
    const spawn = spyOn(Bun, "spawnSync").mockImplementation((args) => ({
      exitCode: failScan && args.includes("--input") ? 1 : 0,
    }));
    try {
      for (const fails of [false, true]) {
        failScan = fails;
        spawn.mockClear();
        const operation = scanImage("ludock:fixture", { RUNNER_TEMP: directory });
        if (failScan) await expect(operation).rejects.toThrow("Docker run failed");
        else await operation;
        const calls = spawn.mock.calls.map(([args]) => args);
        const scannerCalls = calls.filter((args) => args.includes(TRIVY_IMAGE));
        expect(scannerCalls).toHaveLength(2);
        for (const args of scannerCalls) expect(args.join(" ")).not.toContain("docker.sock");
        const [download, scan] = scannerCalls;
        expect(download).toContain("--download-db-only");
        expect(download.join(" ")).not.toContain(":/scan");
        expect(scan[scan.indexOf("--network") + 1]).toBe("none");
        expect(scan).toContain("--offline-scan");
        expect(scan[scan.indexOf("--severity") + 1]).toBe("MEDIUM,HIGH,CRITICAL");
        expect(scan.some((argument) => argument.endsWith(":/scan:ro"))).toBe(true);
        expect(await readdir(directory)).toStrictEqual([]);
      }
    } finally {
      spawn.mockRestore();
      await rm(directory, { recursive: true, force: true });
    }
  });
});
