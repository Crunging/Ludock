import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { describe, it } from "bun:test";

const workflow = Bun.YAML.parse(await readFile(new URL("../../.github/workflows/release.yaml", import.meta.url), "utf8"));
const versionScript = workflow.jobs.validate.steps.find((step) => step.id === "version").run;
const rankScript = workflow.jobs.release.steps.find((step) => step.id === "rank").run;
const tagScript = workflow.jobs.release.steps.find((step) => step.name === "Create release tag").run;

async function withRepository(previousVersion, currentVersion, check) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-release-test-"));
  // Every Git invocation, including workflow shell commands, ignores user config,
  // hooks, signing settings, and exported shell functions.
  const config = Object.entries({
    "user.name": "Ludock fixture",
    "user.email": "fixture@example.invalid",
    "commit.gpgsign": "false",
    "tag.gpgsign": "false",
    "core.hooksPath": "/dev/null",
  });
  const environment = {
    PATH: [path.dirname(process.execPath), process.env.PATH].join(path.delimiter),
    LC_ALL: "C",
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
    GIT_CONFIG_COUNT: String(config.length),
    ...Object.fromEntries(config.flatMap(([key, value], index) => [
      [`GIT_CONFIG_KEY_${index}`, key], [`GIT_CONFIG_VALUE_${index}`, value],
    ])),
  };
  function git(...args) {
    const result = Bun.spawnSync(["git", ...args], { cwd: directory, env: environment, stdout: "pipe", stderr: "pipe" });
    assert.equal(result.exitCode, 0, result.stderr.toString());
    return result.stdout.toString().trim();
  }
  async function run(script, extraEnvironment = {}) {
    const output = path.join(directory, "outputs");
    await writeFile(output, "");
    const result = Bun.spawnSync(["bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script], {
      cwd: directory,
      env: { ...environment, GITHUB_SHA: git("rev-parse", "HEAD"), GITHUB_OUTPUT: output, ...extraEnvironment },
      stdout: "pipe",
      stderr: "pipe",
    });
    const outputs = Object.fromEntries((await readFile(output, "utf8")).trim().split("\n").filter(Boolean).map((line) => line.split("=")));
    return { exitCode: result.exitCode, outputs, diagnostic: result.stdout.toString() + result.stderr.toString() };
  }
  try {
    git("init", "-q", ".");
    await writeFile(path.join(directory, "package.json"), JSON.stringify({ version: previousVersion }));
    git("add", "package.json");
    git("commit", "-qm", "chore: initial fixture");
    const previousCommit = git("rev-parse", "HEAD");
    await writeFile(path.join(directory, "package.json"), JSON.stringify({ version: currentVersion, description: "Fixture change" }));
    git("commit", "-qam", "chore: fixture change");
    await check({ git, run, previousCommit });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

describe("release workflow decisions", () => {
  for (const version of ["1.0.0", "1.0.0-beta.1"]) {
    it(`publishes only nightly when ${version} is unchanged`, async () => {
      await withRepository(version, version, async ({ run }) => {
        const result = await run(versionScript);
        assert.equal(result.exitCode, 0, result.diagnostic);
        assert.deepEqual(result.outputs, { should_release: "false", tag: `v${version}` });
      });
    });
  }

  it("selects a stable release when the version changes", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ run }) => {
      const result = await run(versionScript);
      assert.equal(result.exitCode, 0, result.diagnostic);
      assert.deepEqual(result.outputs, { should_release: "true", tag: "v1.0.1" });
    });
  });

  for (const version of ["1.0.1-beta.1", "malformed"]) {
    it(`rejects changed nonstable version ${version}`, async () => {
      await withRepository("1.0.0", version, async ({ run }) => {
        const result = await run(versionScript);
        assert.equal(result.exitCode, 1, result.diagnostic);
        assert.equal(result.outputs.should_release, undefined);
      });
    });
  }

  it("allows reruns when the existing tag belongs to the same commit", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ git, run }) => {
      git("tag", "--annotate", "v1.0.1", "--message", "Fixture release");
      const originalTag = git("rev-parse", "refs/tags/v1.0.1");
      const version = await run(versionScript);
      assert.equal(version.exitCode, 0, version.diagnostic);
      assert.equal(version.outputs.should_release, "true");
      const tag = await run(tagScript, { RELEASE_TAG: "v1.0.1" });
      assert.equal(tag.exitCode, 0, tag.diagnostic);
      assert.equal(git("rev-parse", "refs/tags/v1.0.1"), originalTag);
    });
  });

  it("rejects a tag belonging to another commit during validation and publication", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ git, run, previousCommit }) => {
      git("tag", "v1.0.1", previousCommit);
      for (const script of [versionScript, tagScript]) {
        const result = await run(script, { RELEASE_TAG: "v1.0.1" });
        assert.equal(result.exitCode, 1, result.diagnostic);
        assert.equal(git("rev-list", "-n", "1", "v1.0.1"), previousCommit);
      }
    });
  });

  for (const [current, existing, expected] of [
    ["v3.0.0", ["v2.9.0"], [true, true, true]],
    ["v1.8.0", ["v2.1.0", "v1.7.8"], [false, true, true]],
    ["v1.7.9", ["v2.1.0", "v1.7.8"], [false, true, true]],
    ["v1.6.9", ["v1.7.8", "v1.6.8"], [false, false, true]],
    ["v1.7.5", ["v1.7.8"], [false, false, false]],
  ]) {
    it(`limits moving tags for ${current} with existing ${existing.join(", ")}`, async () => {
      await withRepository("0.0.0", current.slice(1), async ({ git, run }) => {
        for (const tag of [...existing, current, "v9.0.0-beta.1", "nonrelease"]) git("tag", tag);
        const result = await run(rankScript, { RELEASE_TAG: current });
        assert.equal(result.exitCode, 0, result.diagnostic);
        assert.deepEqual(
          ["is_latest", "is_major_latest", "is_minor_latest"].map((key) => result.outputs[key]),
          expected.map(String),
        );
      });
    });
  }
});
