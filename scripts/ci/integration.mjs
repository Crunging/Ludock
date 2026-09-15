import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Keep the maintained GitHub protocols and release policy, with Bun as the
// executable. These are immutable, self-contained upstream distribution files.
export const integrations = {
  "release-please": {
    repository: "googleapis/release-please-action",
    revision: "45996ed1f6d02564a971a2fa1b5860e934307cf7", // v5
    entry: "dist/index.js",
    extension: "cjs",
    defaults: {
      "config-file": "release-please-config.json", "manifest-file": ".release-please-manifest.json",
      "skip-github-release": "false", "skip-github-pull-request": "false", "skip-labeling": "false",
      "include-component-in-tag": "false", fork: "false", "versioning-strategy": "default",
    },
  },
  "upload-artifact": {
    repository: "actions/upload-artifact",
    revision: "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a", // v7
    entry: "dist/upload/index.js",
    extension: "mjs",
    defaults: {
      name: "browser-failures", "if-no-files-found": "warn", "retention-days": "7",
      "compression-level": "6", overwrite: "false", "include-hidden-files": "false", archive: "true",
    },
  },
};

export function integrationEnvironment(name, environment) {
  const integration = integrations[name];
  if (!integration) throw new Error("Unknown GitHub integration");
  const defaults = { ...integration.defaults };
  if (name === "release-please") {
    defaults["repo-url"] = environment.GITHUB_REPOSITORY;
    defaults["github-api-url"] = environment.GITHUB_API_URL || "https://api.github.com";
    defaults["github-graphql-url"] = environment.GITHUB_GRAPHQL_URL || "https://api.github.com/graphql";
    defaults["changelog-host"] = environment.GITHUB_SERVER_URL || "https://github.com";
  }
  return { ...Object.fromEntries(Object.entries(defaults).map(([key, value]) => [`INPUT_${key.toUpperCase()}`, value])), ...environment };
}

export async function exportBuildRuntime(environment) {
  const variables = ["ACTIONS_RUNTIME_TOKEN", "ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL"];
  if (!environment.ACTIONS_RUNTIME_TOKEN || !environment.ACTIONS_RESULTS_URL || !environment.GITHUB_ENV)
    throw new Error("GitHub did not provide the BuildKit cache runtime credentials");
  const values = variables.filter((name) => environment[name]).map((name) => {
    const value = environment[name];
    if (/[\r\n]/.test(value)) throw new Error("Invalid GitHub runtime environment");
    return [name, value];
  });
  console.log(`::add-mask::${environment.ACTIONS_RUNTIME_TOKEN}`);
  const file = Bun.file(environment.GITHUB_ENV);
  const previous = await file.exists() ? await file.text() : "";
  await Bun.write(file, previous + values.map(([name, value]) => `${name}=${value}\n`).join(""));
}

export async function runIntegration(name, environment = process.env) {
  if (["node", "npm", "npx"].some((command) => Bun.which(command)))
    throw new Error("GitHub integrations must run in the Bun-only action image");
  if (name === "build-runtime") return exportBuildRuntime(environment);
  const integration = integrations[name];
  if (!integration) throw new Error("Unknown GitHub integration");
  const directory = await mkdtemp(join(tmpdir(), "ludock-github-"));
  try {
    const url = `https://raw.githubusercontent.com/${integration.repository}/${integration.revision}/${integration.entry}`;
    const response = await fetch(url, { signal: AbortSignal.timeout(60_000), redirect: "error" });
    if (!response.ok) throw new Error(`Cannot download pinned ${name} bundle (${response.status})`);
    const bundle = join(directory, `action.${integration.extension}`);
    if (integration.extension === "mjs") {
      // The upstream ESM bundle contains UMD probes for these CommonJS names.
      // Explicitly absent bindings preserve ESM semantics and stop Bun from
      // classifying the whole bundle as CommonJS because of those probes.
      await Bun.write(bundle, "const exports = undefined, module = undefined;\n" + await response.text());
    } else await Bun.write(bundle, response);
    const child = Bun.spawn([process.execPath, bundle], {
      env: integrationEnvironment(name, environment), stdin: "ignore", stdout: "inherit", stderr: "inherit",
    });
    const code = await child.exited;
    if (code !== 0) throw new Error(`${name} failed with exit code ${code}`);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

if (import.meta.main) await runIntegration(process.argv[2]);
