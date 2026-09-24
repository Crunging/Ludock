import { describe, expect, it, spyOn } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { imageTags, scanImage, TRIVY_IMAGE } from "../ci/containers.mjs";

const root = new URL("../../", import.meta.url);

describe("CI helpers", () => {
  it("keeps nightly and stable tag ownership, including older hotfix releases", () => {
    const base = { IMAGE_NAME: "ghcr.io/Crunging/Ludock", RELEASE_TAG: "v1.2.3" };
    expect(imageTags(base)).toStrictEqual(["ghcr.io/crunging/ludock:nightly"]);
    expect(imageTags({ ...base, SHOULD_RELEASE: "true", IS_MINOR_LATEST: "true" })).toStrictEqual([
      "ghcr.io/crunging/ludock:nightly", "ghcr.io/crunging/ludock:1.2.3", "ghcr.io/crunging/ludock:1.2",
    ]);
    const latest = imageTags({ ...base, SHOULD_RELEASE: "true", IS_MINOR_LATEST: "true", IS_MAJOR_LATEST: "true", IS_LATEST: "true" });
    expect(latest).toContain("ghcr.io/crunging/ludock:1");
    expect(latest).toContain("ghcr.io/crunging/ludock:latest");
    expect(() => imageTags({ ...base, SHOULD_RELEASE: "true", RELEASE_TAG: "v1.2.3-beta.1" })).toThrow();
    expect(() => imageTags({ ...base, IMAGE_NAME: "invalid\nimage" })).toThrow();
  });

  it("requires the exact Bun release and enforces the package minimum", async () => {
    const script = Bun.fileURLToPath(new URL("scripts/ci/check-bun.mjs", root));
    const check = (cwd) => Bun.spawnSync([process.execPath, script], { cwd, stdout: "pipe", stderr: "pipe" });
    expect(check(Bun.fileURLToPath(root)).exitCode).toBe(0);
    const directory = await mkdtemp(join(tmpdir(), "ludock-ci-bun-version-"));
    try {
      const [major] = Bun.version.split(".");
      await Bun.write(join(directory, ".bun-version"), Bun.version);
      await Bun.write(join(directory, "package.json"), JSON.stringify({ engines: { bun: `>=${Bun.version}` } }));
      expect(check(directory).exitCode).toBe(0);
      for (const selected of [major, `${Number(major) + 1}.0.0`, `${Bun.version} garbage`, ""]) {
        await Bun.write(join(directory, ".bun-version"), selected);
        const rejected = check(directory);
        expect(rejected.exitCode).not.toBe(0);
        expect(new TextDecoder().decode(rejected.stderr)).toContain("does not match .bun-version");
      }
      await Bun.write(join(directory, ".bun-version"), Bun.version);
      await Bun.write(join(directory, "package.json"), JSON.stringify({ engines: { bun: `>=${Number(major) + 1}.0.0` } }));
      const rejected = check(directory);
      expect(rejected.exitCode).not.toBe(0);
      expect(new TextDecoder().decode(rejected.stderr)).toContain("does not satisfy package.json engines.bun");
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  });

  it("scans exported images offline without giving the scanner Docker access, and cleans up failures", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-scanner-test-"));
    let missing = false;
    let failScan = false;
    const spawn = spyOn(Bun, "spawnSync").mockImplementation((args) => ({
      exitCode: (missing && args.includes("inspect")) || (failScan && args.includes("--input")) ? 1 : 0,
    }));
    try {
      for (const scenario of ["local", "remote", "failed"]) {
        missing = scenario === "remote";
        failScan = scenario === "failed";
        spawn.mockClear();
        const operation = scanImage("ludock:fixture", { RUNNER_TEMP: directory });
        if (failScan) await expect(operation).rejects.toThrow("Docker run failed");
        else await operation;
        const calls = spawn.mock.calls.map(([args]) => args);
        expect(calls.some((args) => args[1] === "pull")).toBe(missing);
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
