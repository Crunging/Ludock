import { isDeepStrictEqual } from "node:util";
import { releaseNotes } from "../release-notes.mjs";

const releaseBranch = "release-please--branches--main--components--ludock";
const releaseFiles = [".release-please-manifest.json", "CHANGELOG.md", "package.json"];

export function isReleasePullRequest(pull, repository) {
  return Boolean(repository && pull?.base?.ref === "main" && pull?.head?.ref === releaseBranch &&
    pull.base.repo?.full_name === repository && pull.head.repo?.full_name === repository);
}

function stableVersion(value) {
  if (typeof value !== "string" || !/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(value)) {
    throw new Error("Release versions must be stable MAJOR.MINOR.PATCH versions");
  }
  return value.split(".").map(BigInt);
}

export function validateReleaseChanges(before, after) {
  const previousPackage = JSON.parse(before["package.json"]);
  const nextPackage = JSON.parse(after["package.json"]);
  const previous = stableVersion(previousPackage.version);
  const next = stableVersion(nextPackage.version);
  const changedComponent = next.findIndex((value, index) => value !== previous[index]);
  if (changedComponent === -1 || next[changedComponent] < previous[changedComponent]) {
    throw new Error("The release version must increase");
  }
  if (!isDeepStrictEqual({ ...nextPackage, version: previousPackage.version }, previousPackage)) {
    throw new Error("Release PRs may change only the root package version, not scripts or dependencies");
  }
  for (const [files, version] of [[before, previousPackage.version], [after, nextPackage.version]]) {
    if (!isDeepStrictEqual(JSON.parse(files[".release-please-manifest.json"]), { ".": version })) {
      throw new Error("The release manifest must match the root package version");
    }
  }

  const tag = `v${nextPackage.version}`;
  const changelog = after["CHANGELOG.md"];
  const notes = releaseNotes(changelog, tag);
  const previousChangelog = before["CHANGELOG.md"];
  const previousHeading = previousChangelog.search(/^## /m);
  const headings = [...changelog.matchAll(/^## /gm)];
  if (previousHeading < 0 || headings.length < 2 ||
      !changelog.slice(headings[0].index).startsWith(notes.split("\n", 1)[0] + "\n") ||
      changelog.slice(0, headings[0].index) !== previousChangelog.slice(0, previousHeading) ||
      changelog.slice(headings[1].index) !== previousChangelog.slice(previousHeading)) {
    throw new Error("The changelog must prepend the new release and preserve published history");
  }
  return tag;
}

export function validateReleaseRepository(directory, base, head) {
  for (const sha of [base, head]) {
    if (!/^[a-f0-9]{40}$/.test(sha || "")) throw new Error("Expected exact pull request commit SHAs");
  }
  function git(...args) {
    const result = Bun.spawnSync(["git", ...args], { cwd: directory, stdout: "pipe", stderr: "pipe" });
    if (result.exitCode !== 0) throw new Error(`Git ${args[0]} failed: ${result.stderr.toString().trim()}`);
    return result.stdout.toString();
  }
  // The release branch is refreshed from main by Release Please. Comparing its
  // complete tree against that base also catches stale or unexpected changes.
  if (git("merge-base", base, head).trim() !== base) {
    throw new Error("Refresh the release PR from its current base before checking it");
  }
  const changed = git("diff", "--name-only", "--no-renames", "-z", base, head, "--").split("\0").filter(Boolean).sort();
  if (!isDeepStrictEqual(changed, releaseFiles)) {
    throw new Error("Release PRs must change only package.json, .release-please-manifest.json, and CHANGELOG.md");
  }
  const [before, after] = [base, head].map((sha) => Object.fromEntries(releaseFiles.map((filename) => {
    if (!git("ls-tree", sha, "--", filename).startsWith("100644 blob ")) {
      throw new Error(`Release metadata must remain a regular nonexecutable file: ${filename}`);
    }
    return [filename, git("show", `${sha}:${filename}`)];
  })));
  const tag = validateReleaseChanges(before, after);
  if (git("tag", "--list", tag).trim()) throw new Error(`Release tag ${tag} already exists`);
  return tag;
}

if (import.meta.main) {
  const event = await Bun.file(process.env.GITHUB_EVENT_PATH).json();
  const pull = event.pull_request;
  if (!pull) throw new Error("Expected a pull request event");
  if (!isReleasePullRequest(pull, process.env.GITHUB_REPOSITORY)) {
    throw new Error("Focused release checks require the same-repository Release Please branch targeting main");
  }
  const tag = validateReleaseRepository(process.cwd(), pull.base.sha, pull.head.sha);
  console.log(`${tag}: release metadata is consistent; run the production build and startup check.`);
}
