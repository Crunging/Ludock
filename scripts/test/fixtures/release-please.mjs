import { dirname, join } from "node:path";
import { integrationEnvironment } from "../../ci/integration.mjs";

// Exercise the unmodified upstream action through changelog, version, and PR
// creation. Every GitHub request receives a fixture response; no token is used.
const bundle = process.argv[2];
Object.assign(process.env, integrationEnvironment("release-please", {
  GITHUB_REPOSITORY: "fixture/ludock", INPUT_TOKEN: "fixture-token",
  GITHUB_OUTPUT: join(dirname(bundle), "fixture-output"),
  "INPUT_TARGET-BRANCH": "main", "INPUT_SKIP-GITHUB-RELEASE": "true",
  "INPUT_SKIP-GITHUB-PULL-REQUEST": "false", "INPUT_SKIP-LABELING": "true",
}));
const previous = "a".repeat(40), current = "b".repeat(40);
const pageInfo = { hasNextPage: false, endCursor: null };
const files = {
  "release-please-config.json": JSON.stringify({
    "release-type": "node", "include-component-in-tag": false,
    "bump-minor-pre-major": true, "skip-github-release": true, packages: { ".": {} },
  }),
  ".release-please-manifest.json": '{".":"0.3.0"}',
  "package.json": '{"name":"fixture","version":"0.3.0"}',
  "CHANGELOG.md": "# Changelog\n",
};
const tree = Object.keys(files).map((path, index) => ({ path, mode: "100644", type: "blob", sha: String(index + 1).repeat(40) }));
const prefix = "/repos/fixture/ludock";
let changes, pullRequest;
await Bun.write(process.env.GITHUB_OUTPUT, "");
const { main } = await import(bundle);
await main(async (input, options = {}) => {
  const url = new URL(typeof input === "string" ? input : input.url);
  const method = options.method || "GET";
  const body = options.body ? JSON.parse(options.body) : {};
  if (url.origin !== "https://api.github.com") throw new Error("Unexpected fixture host");
  if (url.pathname === "/graphql" && method === "POST") {
    let repository;
    if (body.query.startsWith("query releases(")) repository = { releases: {
      nodes: [{ name: "v0.3.0", tag: { name: "v0.3.0" }, tagCommit: { oid: previous }, description: "", isDraft: false }], pageInfo,
    } };
    if (body.query.startsWith("query pullRequestsSince(")) repository = { ref: { target: { history: { nodes: [
      { sha: current, message: "feat: improve native transfers", associatedPullRequests: { nodes: [{
        number: 1, title: "feat: improve native transfers", baseRefName: "main", headRefName: "feature",
        mergeCommit: { oid: current }, body: "", labels: { nodes: [] }, files: { nodes: [{ path: "package.json" }], pageInfo },
      }] } },
      { sha: previous, message: "chore: release 0.3.0", associatedPullRequests: { nodes: [] } },
    ], pageInfo } } } };
    if (repository) return Response.json({ data: { repository } });
  }
  if (method === "GET") {
    if (url.pathname === `${prefix}/git/trees/main`) return Response.json({ sha: current, tree, truncated: false });
    const blob = tree.find((entry) => url.pathname === `${prefix}/git/blobs/${entry.sha}`);
    if (blob) return Response.json({ encoding: "base64", content: btoa(files[blob.path]) });
    if (url.pathname === `${prefix}/commits/${previous}`) return Response.json({ files: [] });
    if (url.pathname === `${prefix}/pulls`) return Response.json([]);
    if (url.pathname === `${prefix}/branches/main`) return Response.json({ commit: { sha: current } });
    if (url.pathname.startsWith(`${prefix}/git/ref/heads%2Frelease-please--`)) return Response.json({ message: "Not Found" }, { status: 404 });
    if (url.pathname === `${prefix}/git/commits/${current}`) return Response.json({ tree: { sha: current } });
    if (url.pathname === `${prefix}/pulls/2` && pullRequest) return Response.json(pullRequest);
  }
  if (method === "POST") {
    if (url.pathname === `${prefix}/git/refs`) return Response.json({ object: { sha: current } });
    if (url.pathname === `${prefix}/git/trees`) { changes = body.tree; return Response.json({ sha: current }); }
    if (url.pathname === `${prefix}/git/commits`) return Response.json({ sha: current });
    if (url.pathname === `${prefix}/pulls`) {
      pullRequest = { number: 2, title: body.title, body: body.body, head: { ref: body.head }, base: { ref: body.base }, labels: [] };
      return Response.json(pullRequest, { status: 201 });
    }
  }
  if (method === "PATCH" && url.pathname.startsWith(`${prefix}/git/refs/heads%2Frelease-please--`))
    return Response.json({ object: { sha: current } });
  throw new Error(`Unexpected fixture request: ${method} ${url.pathname}`);
});
const updated = Object.fromEntries((changes || []).map((entry) => [entry.path, entry.content]));
if (JSON.parse(updated["package.json"] || "{}").version !== "0.4.0" ||
    JSON.parse(updated[".release-please-manifest.json"] || "{}")["."] !== "0.4.0" ||
    !updated["CHANGELOG.md"]?.includes("### Features\n\n* improve native transfers") ||
    !pullRequest?.title.endsWith("release 0.4.0") ||
    !pullRequest.body.includes("improve native transfers"))
  throw new Error("Release Please did not generate the expected version, changelog, and pull request");
console.log("Verified Release Please changelog and PR generation against the fixture API.");
