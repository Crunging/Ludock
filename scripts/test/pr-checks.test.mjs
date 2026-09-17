import { describe, expect, it } from "bun:test";
import { chmod, mkdtemp, rm, symlink } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { isReleasePullRequest, validateReleaseChanges, validateReleaseRepository } from "../ci/pr-checks.mjs";

const repository = "fixture/ludock";
const branch = "release-please--branches--main--components--ludock";
const oldEntry = "## [0.3.0](https://example.invalid/v0.3.0) (2026-09-01)\n\n* Earlier release.\n";
const newEntry = "## [0.4.0](https://example.invalid/v0.4.0) (2026-09-17)\n\n### Features\n\n* New feature.\n\n";
const packageFor = (version) => JSON.stringify({ name: "ludock", version, scripts: { build: "bun build" } });
const before = {
  "package.json": packageFor("0.3.0"),
  ".release-please-manifest.json": JSON.stringify({ ".": "0.3.0" }),
  "CHANGELOG.md": "# Changelog\n\n" + oldEntry,
};
const after = {
  "package.json": packageFor("0.4.0"),
  ".release-please-manifest.json": JSON.stringify({ ".": "0.4.0" }),
  "CHANGELOG.md": "# Changelog\n\n" + newEntry + oldEntry,
};
function pullRequest() {
  return {
    base: { ref: "main", repo: { full_name: repository } },
    head: { ref: branch, repo: { full_name: repository } },
  };
}

async function withRepository(check) {
  const directory = await mkdtemp(join(tmpdir(), "ludock-pr-check-"));
  const environment = {
    PATH: process.env.PATH,
    GIT_CONFIG_GLOBAL: "/dev/null",
    GIT_CONFIG_NOSYSTEM: "1",
  };
  function git(...args) {
    const result = Bun.spawnSync(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
      "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", ...args], {
      cwd: directory, env: environment, stdout: "pipe", stderr: "pipe",
    });
    expect(result.exitCode, result.stderr.toString()).toBe(0);
    return result.stdout.toString().trim();
  }
  const write = (filename, content) => Bun.write(join(directory, filename), content);
  async function commit(files) {
    for (const [filename, contents] of Object.entries(files)) await write(filename, contents);
    git("add", ".");
    git("commit", "-qm", "chore: fixture");
    return git("rev-parse", "HEAD");
  }
  try {
    git("init", "-q", ".");
    const base = await commit({ ...before, "app.js": "// Application fixture\n" });
    const head = await commit(after);
    await check({ directory, base, head, git, write, commit });
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

describe("release PR checks", () => {
  it("limits the focused checks to the same-repository release branch targeting main", () => {
    expect(isReleasePullRequest(pullRequest(), repository)).toBe(true);
    for (const field of ["base", "head"]) {
      const otherBranch = pullRequest();
      otherBranch[field].ref = "feature";
      expect(isReleasePullRequest(otherBranch, repository)).toBe(false);
      const fork = pullRequest();
      fork[field].repo.full_name = "someone/ludock";
      expect(isReleasePullRequest(fork, repository)).toBe(false);
      fork[field].repo = null;
      expect(isReleasePullRequest(fork, repository)).toBe(false);
    }
    expect(isReleasePullRequest(undefined, repository)).toBe(false);
    expect(isReleasePullRequest(pullRequest(), "")).toBe(false);
  });

  it("accepts the generated version, manifest, and changelog together", () => {
    expect(validateReleaseChanges(before, after)).toBe("v0.4.0");
  });

  for (const version of ["0.3.0", "0.2.9", "0.4.0-beta.1", "0.04.0", "invalid", 4, null]) {
    it(`rejects nonincreasing or invalid release version ${version}`, () => {
      expect(() => validateReleaseChanges(before, { ...after, "package.json": packageFor(version) })).toThrow();
    });
  }

  it("rejects dependency and script edits within package.json", () => {
    for (const extra of [{ dependencies: { newPackage: "*" } }, { scripts: { build: "unexpected command" } }]) {
      const changed = JSON.stringify({ ...JSON.parse(after["package.json"]), ...extra });
      expect(() => validateReleaseChanges(before, { ...after, "package.json": changed })).toThrow("only the root package version");
    }
  });

  it("rejects mismatched manifests in either revision and unexpected manifest components", () => {
    for (const manifest of [{ ".": "9.0.0" }, { ".": "0.4.0", extra: "0.4.0" }]) {
      expect(() => validateReleaseChanges(before, { ...after, ".release-please-manifest.json": JSON.stringify(manifest) })).toThrow("manifest");
    }
    expect(() => validateReleaseChanges({ ...before, ".release-please-manifest.json": "{}" }, after)).toThrow("manifest");
  });

  for (const [name, changelog] of Object.entries({
    missing: before["CHANGELOG.md"],
    duplicate: "# Changelog\n\n" + newEntry + newEntry + oldEntry,
    empty: "# Changelog\n\n## 0.4.0\n\n" + oldEntry,
    olderFirst: "# Changelog\n\n" + oldEntry + newEntry,
    editedHistory: after["CHANGELOG.md"].replace("Earlier release", "Rewritten history"),
    extraRelease: "# Changelog\n\n" + newEntry + "## 0.3.1\n\n* Surprise.\n\n" + oldEntry,
    editedPreamble: after["CHANGELOG.md"].replace("# Changelog", "# Rewritten"),
  })) {
    it(`rejects ${name} changelog content`, () => {
      expect(() => validateReleaseChanges(before, { ...after, "CHANGELOG.md": changelog })).toThrow();
    });
  }

  it("validates real Git trees and rejects an existing release tag", async () => {
    await withRepository(async ({ directory, base, head, git }) => {
      expect(validateReleaseRepository(directory, base, head)).toBe("v0.4.0");
      git("tag", "v0.4.0", base);
      expect(() => validateReleaseRepository(directory, base, head)).toThrow("already exists");
    });
  });

  for (const filename of ["app.js", "bun.lock", ".github/workflows/extra.yaml"]) {
    it(`rejects an unexpected ${filename} change`, async () => {
      await withRepository(async ({ directory, base, commit }) => {
        const head = await commit({ [filename]: "unexpected change" });
        expect(() => validateReleaseRepository(directory, base, head)).toThrow("must change only");
      });
    });
  }

  for (const mode of ["executable", "symlink", "deleted"]) {
    it(`rejects ${mode} release metadata`, async () => {
      await withRepository(async ({ directory, base, commit }) => {
        const file = join(directory, "CHANGELOG.md");
        if (mode === "executable") await chmod(file, 0o755);
        else {
          await rm(file);
          if (mode === "symlink") await symlink("app.js", file);
        }
        const head = await commit({});
        expect(() => validateReleaseRepository(directory, base, head)).toThrow("regular nonexecutable file");
      });
    });
  }

  it("rejects stale release branches and revision arguments that are not exact SHAs", async () => {
    await withRepository(async ({ directory, base, head, git, commit }) => {
      for (const ref of ["HEAD", "--help", "a".repeat(39), undefined]) {
        expect(() => validateReleaseRepository(directory, ref, head)).toThrow("exact pull request commit SHAs");
      }
      git("checkout", "--detach", base);
      const updatedBase = await commit({ "app.js": "// A newer main commit\n" });
      expect(() => validateReleaseRepository(directory, updatedBase, head)).toThrow("Refresh the release PR");
    });
  });

  it("validates the event and actual PR commits through the workflow entry point", async () => {
    await withRepository(async ({ directory, base, head, write, git }) => {
      const event = pullRequest();
      event.base.sha = base;
      event.head.sha = head;
      const run = async () => {
        await write("event.json", JSON.stringify({ pull_request: event }));
        return Bun.spawnSync([process.execPath, Bun.fileURLToPath(new URL("../ci/pr-checks.mjs", import.meta.url))], {
          cwd: directory, env: { PATH: process.env.PATH, GITHUB_EVENT_PATH: join(directory, "event.json"),
            GITHUB_REPOSITORY: repository }, stdout: "pipe", stderr: "pipe",
        });
      };
      expect((await run()).exitCode).toBe(0);
      git("tag", "v0.4.0", base);
      expect((await run()).exitCode).toBe(1);
      event.head.ref = "code-change";
      expect((await run()).exitCode).toBe(1);
    });
  });

  it("keeps full checks on code PRs and publication, and release checks read-only", async () => {
    const readWorkflow = async (name) => Bun.YAML.parse(await Bun.file(new URL(`../../.github/workflows/${name}.yaml`, import.meta.url)).text());
    const ci = await readWorkflow("ci");
    expect(ci.on.pull_request["paths-ignore"]).not.toContain("package.json");
    expect(ci.permissions).toEqual({ contents: "read" });
    // Ordinary PR checks start immediately; no classification runner is needed.
    expect(ci.jobs.check.needs).toBeUndefined();
    const releaseCondition = ci.jobs.release.if.slice(4, -3);
    expect(releaseCondition).toBe("github.base_ref == 'main' && " +
      "github.head_ref == 'release-please--branches--main--components--ludock' && " +
      "github.event.pull_request.head.repo.full_name == github.repository");
    expect(ci.jobs.check.if).toBe("${{ !(" + releaseCondition + ") }}");
    expect(ci.jobs.check.uses).toBe("./.github/workflows/checks.yaml");
    const steps = ci.jobs.release.steps;
    expect(steps.findIndex((step) => step.run === "bun scripts/ci/pr-checks.mjs")).toBeLessThan(
      steps.findIndex((step) => step.run === "bun scripts/ci/containers.mjs build check"));
    expect(ci.jobs.release.steps.some((step) => step.run === "bun scripts/test-linux.mjs --smoke-only")).toBe(true);
    expect((await readWorkflow("release")).jobs.check.uses).toBe("./.github/workflows/checks.yaml");
  });
});
