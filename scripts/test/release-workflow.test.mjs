import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { expect, describe, it } from "bun:test";

import { releaseNotes } from "../release-notes.mjs";

const workflow = Bun.YAML.parse(await readFile(new URL("../../.github/workflows/release.yaml", import.meta.url), "utf8"));
const versionScript = workflow.jobs.validate.steps.find((step) => step.id === "version").run;
const rankScript = workflow.jobs.release.steps.find((step) => step.id === "rank").run;
const tagScript = workflow.jobs.release.steps.find((step) => step.name === "Create release tag").run;
const publicationStep = workflow.jobs.release.steps.find((step) => step.name === "Mark release PR as published");

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
    expect(result.exitCode, result.stderr.toString()).toBe(0);
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
        expect(result.exitCode, result.diagnostic).toBe(0);
        expect(result.outputs).toStrictEqual({ should_release: "false", tag: `v${version}` });
      });
    });
  }

  it("selects a stable release when the version changes", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ run }) => {
      const result = await run(versionScript);
      expect(result.exitCode, result.diagnostic).toBe(0);
      expect(result.outputs).toStrictEqual({ should_release: "true", tag: "v1.0.1" });
    });
  });

  for (const version of ["1.0.1-beta.1", "malformed"]) {
    it(`rejects changed nonstable version ${version}`, async () => {
      await withRepository("1.0.0", version, async ({ run }) => {
        const result = await run(versionScript);
        expect(result.exitCode, result.diagnostic).toBe(1);
        expect(result.outputs.should_release).toBe(undefined);
      });
    });
  }

  it("allows reruns when the existing tag belongs to the same commit", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ git, run }) => {
      git("tag", "--annotate", "v1.0.1", "--message", "Fixture release");
      const originalTag = git("rev-parse", "refs/tags/v1.0.1");
      const version = await run(versionScript);
      expect(version.exitCode, version.diagnostic).toBe(0);
      expect(version.outputs.should_release).toBe("true");
      const tag = await run(tagScript, { RELEASE_TAG: "v1.0.1" });
      expect(tag.exitCode, tag.diagnostic).toBe(0);
      expect(git("rev-parse", "refs/tags/v1.0.1")).toBe(originalTag);
    });
  });

  it("rejects a tag belonging to another commit during validation and publication", async () => {
    await withRepository("1.0.0", "1.0.1", async ({ git, run, previousCommit }) => {
      git("tag", "v1.0.1", previousCommit);
      for (const script of [versionScript, tagScript]) {
        const result = await run(script, { RELEASE_TAG: "v1.0.1" });
        expect(result.exitCode, result.diagnostic).toBe(1);
        expect(git("rev-list", "-n", "1", "v1.0.1")).toBe(previousCommit);
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
        expect(result.exitCode, result.diagnostic).toBe(0);
        expect(["is_latest", "is_major_latest", "is_minor_latest"].map((key) => result.outputs[key])).toStrictEqual(expected.map(String));
      });
    });
  }
});

const publicationRepository = "fixture/ludock";
const publicationSha = "a".repeat(40);

function pendingPullRequest(overrides = {}) {
  return {
    number: 6,
    merged_at: "2026-01-01T00:00:00Z",
    merge_commit_sha: publicationSha,
    base: { ref: "main", repo: { full_name: publicationRepository } },
    labels: [{ name: "autorelease: pending" }, { name: "keep-this-label" }],
    ...overrides,
  };
}

async function runPublicationLabels(pages, failMethod = "") {
  const directory = await mkdtemp(path.join(os.tmpdir(), "ludock-release-label-test-"));
  const callsPath = path.join(directory, "calls.jsonl");
  const responsePath = path.join(directory, "response.jsonl");
  const ghPath = path.join(directory, "gh");
  try {
    await writeFile(callsPath, "");
    await writeFile(responsePath, pages.map((page) => JSON.stringify(page)).join("\n"));
    await writeFile(ghPath, `#!/usr/bin/env bun
import { appendFileSync, readFileSync } from "node:fs";
const args = process.argv.slice(2);
appendFileSync(process.env.GH_CALLS, JSON.stringify(args) + "\\n");
const method = args.includes("--method") ? args[args.indexOf("--method") + 1] : "GET";
if (method === process.env.GH_FAIL_METHOD) process.exit(1);
if (method === "GET") process.stdout.write(readFileSync(process.env.GH_RESPONSE));
`);
    await chmod(ghPath, 0o755);
    const subprocess = Bun.spawn(["bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", publicationStep.run], {
      cwd: directory,
      env: {
        PATH: [directory, path.dirname(process.execPath), process.env.PATH].join(path.delimiter),
        LC_ALL: "C",
        GITHUB_REPOSITORY: publicationRepository,
        GITHUB_SHA: publicationSha,
        GH_CALLS: callsPath,
        GH_RESPONSE: responsePath,
        GH_FAIL_METHOD: failMethod,
      },
      stdout: "pipe",
      stderr: "pipe",
    });
    const [exitCode, stdout, stderr] = await Promise.all([
      subprocess.exited,
      new Response(subprocess.stdout).text(),
      new Response(subprocess.stderr).text(),
    ]);
    const calls = (await readFile(callsPath, "utf8")).trim().split("\n").filter(Boolean).map((line) => JSON.parse(line));
    return { exitCode, calls, diagnostic: stdout + stderr };
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

describe("release PR publication state", () => {
  it("marks only the merged pending PR for the published commit, including later API pages", async () => {
    const result = await runPublicationLabels([
      [
        pendingPullRequest({ number: 1, merged_at: null }),
        pendingPullRequest({ number: 2, merge_commit_sha: "b".repeat(40) }),
        pendingPullRequest({ number: 3, base: { ref: "maintenance", repo: { full_name: publicationRepository } } }),
        pendingPullRequest({ number: 4, base: { ref: "main", repo: { full_name: "unrelated/ludock" } } }),
        pendingPullRequest({ number: 5, labels: [{ name: "autorelease: tagged" }] }),
      ],
      [pendingPullRequest()],
    ]);
    expect(result.exitCode, result.diagnostic).toBe(0);
    expect(result.calls).toStrictEqual([
      ["api", "--paginate", `repos/${publicationRepository}/commits/${publicationSha}/pulls`],
      ["api", "--method", "POST", `repos/${publicationRepository}/issues/6/labels`, "-f", "labels[]=autorelease: tagged", "--silent"],
      ["api", "--method", "DELETE", `repos/${publicationRepository}/issues/6/labels/autorelease%3A%20pending`, "--silent"],
    ]);
  });

  for (const [description, pulls] of [
    ["an already tagged PR", [pendingPullRequest({ labels: [{ name: "autorelease: tagged" }] })]],
    ["a release with no associated PR", []],
  ]) {
    it(`does not mutate labels for ${description}`, async () => {
      const result = await runPublicationLabels([pulls]);
      expect(result.exitCode, result.diagnostic).toBe(0);
      expect(result.calls.length).toBe(1);
    });
  }

  it("retries a transition interrupted after adding the tagged label", async () => {
    const result = await runPublicationLabels([[pendingPullRequest({ labels: [{ name: "autorelease: pending" }, { name: "autorelease: tagged" }] })]]);
    expect(result.exitCode, result.diagnostic).toBe(0);
    expect(result.calls.slice(1).map((args) => args[2])).toStrictEqual(["POST", "DELETE"]);
  });

  it("rejects ambiguous matches without changing labels", async () => {
    const result = await runPublicationLabels([[pendingPullRequest()], [pendingPullRequest({ number: 7 })]]);
    expect(result.exitCode, result.diagnostic).toBe(1);
    expect(result.calls.length).toBe(1);
  });

  for (const [method, expectedCalls] of [["GET", 1], ["POST", 2], ["DELETE", 3]]) {
    it(`preserves a retryable pending state and fails when ${method} fails`, async () => {
      const result = await runPublicationLabels([[pendingPullRequest()]], method);
      expect(result.exitCode, result.diagnostic).toBe(1);
      expect(result.calls.length).toBe(expectedCalls);
    });
  }
});

describe("release notes", () => {
  const current = "## [1.2.3](https://example.invalid/compare/v1.2.2...v1.2.3) (2026-09-14)\n\n### Bug Fixes\n\n* Preserve the reviewed entry.\n";
  const older = "## 1.2.2 (2026-09-01)\n\n* Earlier change.\n";
  it("uses only the selected release-please section, including the first release", () => {
    expect(releaseNotes(`# Changelog\n\n${current}\n${older}`, "v1.2.3")).toBe(current);
    expect(releaseNotes(`# Changelog\n\n${current}`, "v1.2.3")).toBe(current);
    expect(releaseNotes(current + older, "v1.2.2")).toBe(older);
  });
  it("fails before publication for missing, duplicate, or empty entries", () => {
    for (const changelog of [older, current + current, "## 1.2.3\n\n" + older])
      expect(() => releaseNotes(changelog, "v1.2.3")).toThrow();
    expect(() => releaseNotes(current, "v1.2.3-beta.1")).toThrow();
  });
});
