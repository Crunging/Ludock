import { describe, expect, it, spyOn } from "bun:test";
import { mkdtemp, readdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { imageTags, buildArguments, scanImage, BUILDKIT_IMAGE, TRIVY_IMAGE, SBOM_IMAGE } from "../ci/containers.mjs";
import { exportBuildRuntime, integrationEnvironment, integrations, stageIntegration } from "../ci/integration.mjs";

const root = new URL("../../", import.meta.url);
const read = (name) => Bun.file(new URL(name, root)).text();

describe("Bun-only CI", () => {
  it("runs repository actions with Bun or native containers and keeps all external code pinned", async () => {
    const bunImage = (await read("Dockerfile")).match(/^ARG BUN_IMAGE=(.+)$/m)?.[1];
    for await (const filename of new Bun.Glob(".github/{workflows,actions}/**/*.yaml").scan({ cwd: Bun.fileURLToPath(root), dot: true })) {
      const data = Bun.YAML.parse(await read(filename));
      if (data.runs) {
        expect(["docker", "composite"]).toContain(data.runs.using);
        if (data.runs.using === "docker") {
          expect(data.runs.image).toBe("Dockerfile");
          const dockerfile = await read(filename.replace(/action\.yaml$/, "Dockerfile"));
          expect(dockerfile).toContain(`FROM ${bunImage}`);
          expect(dockerfile).toContain("RUN apk upgrade --no-cache");
          expect(dockerfile).toContain('ENTRYPOINT ["/usr/local/bin/bun"]');
        }
      }
      for (const job of Object.values(data.jobs || {})) {
        if (job.uses) expect(job.uses).toStartWith("./.github/workflows/");
        for (const step of job.steps || []) {
          if (step.uses) expect(step.uses).toMatch(/^(?:\.\/\.github\/|docker:\/\/[^\s@]+@sha256:[a-f0-9]{64}$)/);
          if (step.run) expect(step.run).not.toMatch(/(?:^|[;&|]\s*|\n\s*)(?:node|npm|npx)\s/);
          if (filename.endsWith("checks.yaml") && step.with?.integration)
            expect(["upload-artifact", "build-runtime", "verify-release-please"]).toContain(step.with.integration);
        }
      }
    }
    for (const integration of Object.values(integrations)) expect(integration.revision).toMatch(/^[a-f0-9]{40}$/);
    for (const image of [BUILDKIT_IMAGE, TRIVY_IMAGE, SBOM_IMAGE]) expect(image).toMatch(/@sha256:[a-f0-9]{64}$/);
    const setup = Bun.YAML.parse(await read(".github/actions/setup-bun/action.yaml"));
    expect(setup.runs.steps[0].env.BUN_IMAGE).toMatch(/^oven\/bun:1-distroless@sha256:[a-f0-9]{64}$/);
    expect(Bun.TOML.parse(await read("bunfig.toml")).run.bun).toBe(true);
    const checks = Bun.YAML.parse(await read(".github/workflows/checks.yaml"));
    expect(checks.jobs.source.steps.some((step) => step.with?.integration === "verify-release-please")).toBe(true);
  });

  it("installs workspace dependencies before importing the runtime image selector", async () => {
    for (const filename of ["checks.yaml", "dependency-security.yaml"]) {
      const workflow = Bun.YAML.parse(await read(`.github/workflows/${filename}`));
      for (const job of Object.values(workflow.jobs)) {
        const steps = job.steps || [];
        for (const [index, step] of steps.entries()) {
          if (!step.run?.includes("packages/backend/src/runtime-images.ts")) continue;
          expect(steps.slice(0, index).some((previous) => previous.run === "bun install --frozen-lockfile")).toBe(true);
        }
      }
    }
  });

  it("stages companion assets beside their bundle and waits for downloads before reporting failure", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-ci-assets-"));
    const integration = integrations["release-please"];
    let missing = false;
    const delayed = Promise.withResolvers();
    const requested = [];
    const fetcher = spyOn(globalThis, "fetch").mockImplementation(async (url) => {
      const base = `https://raw.githubusercontent.com/${integration.repository}/${integration.revision}/dist/`;
      expect(url).toStartWith(base);
      const name = url.slice(base.length);
      requested.push(name);
      if (missing && name === "header1.hbs") await delayed.promise;
      return new Response(name, { status: missing && name === "template1.hbs" ? 404 : 200 });
    });
    try {
      const bundle = await stageIntegration("release-please", directory);
      expect(await Bun.file(bundle).text()).toBe("index.js");
      expect(await Bun.file(join(directory, "template1.hbs")).text()).toBe("template1.hbs");
      expect(requested).toContain("header1.hbs");
      expect(requested).toContain("commit1.hbs");
      expect(requested).toContain("footer1.hbs");
      missing = true;
      let settled = false;
      const staging = stageIntegration("release-please", directory).finally(() => { settled = true; });
      void staging.catch(() => {});
      await Bun.sleep(10);
      expect(settled).toBe(false);
      delayed.resolve();
      await expect(staging).rejects.toThrow("template1.hbs (404)");
    } finally {
      delayed.resolve();
      fetcher.mockRestore();
      await rm(directory, { recursive: true, force: true });
    }
  });

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

  it("requires the exact Bun release and enforces the package minimum during setup", async () => {
    const setup = Bun.YAML.parse(await read(".github/actions/setup-bun/action.yaml"));
    expect(setup.runs.steps[0].run).toContain('"$bun_directory/bun" scripts/ci/check-bun.mjs');
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

  it("loads test builds locally and reserves publishing, attestations, and moving tags for release builds", () => {
    const check = buildArguments("check", { LUDOCK_PLATFORM: "linux/arm64", RUNNER_ARCH: "ARM64" });
    expect(check).toContain("--load");
    expect(check).not.toContain("--push");
    const publish = buildArguments("publish", { IMAGE_NAME: "ghcr.io/fixture/ludock", GITHUB_SHA: "a".repeat(40), GITHUB_REPOSITORY: "fixture/ludock", GITHUB_SERVER_URL: "https://github.com" });
    for (const arg of ["--push", "--provenance=mode=max", `--attest=type=sbom,generator=${SBOM_IMAGE}`, "linux/amd64,linux/arm64"]) expect(publish).toContain(arg);
    expect(publish).not.toContain("--load");
    expect(publish).toContain("type=gha,version=2,scope=ci-ARM64");
    expect(() => buildArguments("check", { LUDOCK_PLATFORM: "--push" })).toThrow();
  });

  it("preserves explicit integration inputs and supplies the upstream defaults", () => {
    const environment = integrationEnvironment("release-please", { GITHUB_REPOSITORY: "fixture/ludock", INPUT_TOKEN: "fixture-token", "INPUT_CONFIG-FILE": "custom.json" });
    expect(environment["INPUT_REPO-URL"]).toBe("fixture/ludock");
    expect(environment["INPUT_CONFIG-FILE"]).toBe("custom.json");
    expect(environment.INPUT_TOKEN).toBe("fixture-token");
    const artifact = integrationEnvironment("upload-artifact", { INPUT_PATH: "fixture", "INPUT_RETENTION-DAYS": "3" });
    expect(artifact["INPUT_RETENTION-DAYS"]).toBe("3");
    expect(artifact.INPUT_ARCHIVE).toBe("true");
    expect(() => integrationEnvironment("unknown", {})).toThrow();
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

  it("masks cache credentials and rejects newline injection before exporting any environment", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-ci-env-"));
    const filename = join(directory, "env");
    const messages = spyOn(console, "log").mockImplementation(() => {});
    try {
      await Bun.write(filename, "EXISTING=value\n");
      const environment = { GITHUB_ENV: filename, ACTIONS_RUNTIME_TOKEN: "fixture-secret", ACTIONS_RESULTS_URL: "https://results.example.invalid" };
      await exportBuildRuntime(environment);
      expect(messages.mock.calls[0]).toStrictEqual(["::add-mask::fixture-secret"]);
      const contents = await Bun.file(filename).text();
      expect(contents).toBe("EXISTING=value\nACTIONS_RUNTIME_TOKEN=fixture-secret\nACTIONS_RESULTS_URL=https://results.example.invalid\n");
      await expect(exportBuildRuntime({ ...environment, ACTIONS_CACHE_URL: "bad\nINJECTED=true" })).rejects.toThrow();
      expect(await Bun.file(filename).text()).toBe(contents);
      await expect(exportBuildRuntime({ GITHUB_ENV: filename })).rejects.toThrow();
    } finally {
      messages.mockRestore();
      await rm(directory, { recursive: true, force: true });
    }
  });
});
