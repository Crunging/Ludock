import { describe, expect, it, spyOn } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { imageTags, buildArguments, BUILDKIT_IMAGE, TRIVY_IMAGE } from "../ci/containers.mjs";
import { exportBuildRuntime, integrationEnvironment, integrations } from "../ci/integration.mjs";
import { DEFAULT_HELPER_IMAGE } from "../../packages/backend/src/runtime-images.ts";

const root = new URL("../../", import.meta.url);
const read = (name) => Bun.file(new URL(name, root)).text();

describe("Bun-only CI", () => {
  it("runs repository actions with Bun or native containers and keeps all external code pinned", async () => {
    for await (const filename of new Bun.Glob(".github/{workflows,actions}/**/*.yaml").scan({ cwd: Bun.fileURLToPath(root), dot: true })) {
      const data = Bun.YAML.parse(await read(filename));
      if (data.runs) {
        expect(["docker", "composite"]).toContain(data.runs.using);
        if (data.runs.using === "docker") {
          expect(data.runs.image).toBe("Dockerfile");
          const dockerfile = await read(filename.replace(/action\.yaml$/, "Dockerfile"));
          expect(dockerfile).toContain(`FROM ${DEFAULT_HELPER_IMAGE}`);
          expect(dockerfile).toContain('ENTRYPOINT ["/usr/local/bin/bun"]');
        }
      }
      for (const job of Object.values(data.jobs || {})) {
        if (job.uses) expect(job.uses).toStartWith("./.github/workflows/");
        for (const step of job.steps || []) {
          if (step.uses) expect(step.uses).toMatch(/^(?:\.\/\.github\/|docker:\/\/[^\s@]+@sha256:[a-f0-9]{64}$)/);
          if (step.run) expect(step.run).not.toMatch(/(?:^|[;&|]\s*|\n\s*)(?:node|npm|npx)\s/);
        }
      }
    }
    for (const integration of Object.values(integrations)) expect(integration.revision).toMatch(/^[a-f0-9]{40}$/);
    for (const image of [BUILDKIT_IMAGE, TRIVY_IMAGE]) expect(image).toMatch(/@sha256:[a-f0-9]{64}$/);
    expect(await read(".github/actions/setup-bun/action.yaml")).toContain(DEFAULT_HELPER_IMAGE);
    expect(Bun.TOML.parse(await read("bunfig.toml")).run.bun).toBe(true);
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

  it("loads test builds locally and reserves publishing, attestations, and moving tags for release builds", () => {
    const check = buildArguments("check", { LUDOCK_PLATFORM: "linux/arm64", RUNNER_ARCH: "ARM64" });
    expect(check).toContain("--load");
    expect(check).not.toContain("--push");
    const publish = buildArguments("publish", { IMAGE_NAME: "ghcr.io/fixture/ludock", GITHUB_SHA: "a".repeat(40), GITHUB_REPOSITORY: "fixture/ludock", GITHUB_SERVER_URL: "https://github.com" });
    for (const arg of ["--push", "--provenance=mode=max", "--sbom=true", "linux/amd64,linux/arm64"]) expect(publish).toContain(arg);
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
