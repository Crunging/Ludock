import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";

export const TRIVY_IMAGE = "aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969";

export function imageTags(environment) {
  const image = environment.IMAGE_NAME?.toLowerCase();
  if (!/^ghcr\.io\/[a-z0-9_.-]+\/[a-z0-9_.-]+$/.test(image || "")) throw new Error("Invalid release image name");
  const tags = [`${image}:nightly`];
  if (environment.SHOULD_RELEASE === "true") {
    const tag = environment.RELEASE_TAG;
    if (!/^v\d+\.\d+\.\d+$/.test(tag || "")) throw new Error("Invalid stable release tag");
    const version = tag.slice(1), [major, minor] = version.split(".");
    tags.push(`${image}:${version}`);
    if (environment.IS_MINOR_LATEST === "true") tags.push(`${image}:${major}.${minor}`);
    if (environment.IS_MAJOR_LATEST === "true") tags.push(`${image}:${major}`);
    if (environment.IS_LATEST === "true") tags.push(`${image}:latest`);
  }
  return tags;
}

function run(args) {
  const result = Bun.spawnSync(["docker", ...args], { stdin: "inherit", stdout: "inherit", stderr: "inherit" });
  if (result.exitCode !== 0) throw new Error(`Docker ${args[0]} failed (${result.exitCode})`);
}

export async function scanImage(value, environment = process.env) {
  if (!value || value.startsWith("-")) throw new Error("An image reference is required");
  if (!environment.RUNNER_TEMP?.startsWith("/")) throw new Error("The CI scanner requires RUNNER_TEMP");
  const directory = await mkdtemp(join(environment.RUNNER_TEMP, "ludock-scan-"));
  const cache = `${environment.RUNNER_TEMP}/ludock-trivy-cache:/root/.cache/trivy`;
  try {
    const local = Bun.spawnSync(["docker", "image", "inspect", value], { stdout: "ignore", stderr: "ignore" });
    if (local.exitCode !== 0) run(["pull", value]);
    run(["image", "save", "--output", join(directory, "image.tar"), value]);
    // Download advisory data without access to the image or Docker daemon.
    run(["run", "--rm", "--volume", cache, TRIVY_IMAGE,
      "image", "--download-db-only", "--no-progress"]);
    // Analyze an immutable export offline; the scanner never gets the socket.
    run(["run", "--rm", "--network", "none", "--volume", cache,
      "--volume", `${directory}:/scan:ro`, TRIVY_IMAGE,
      "image", "--input", "/scan/image.tar", "--skip-db-update", "--offline-scan",
      "--scanners", "vuln", "--severity", "MEDIUM,HIGH,CRITICAL", "--ignore-unfixed",
      "--exit-code", "1", "--timeout", "8m"]);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  const [operation, value] = process.argv.slice(2);
  // Release workflows read the image tags this build owns, one per line.
  if (operation === "tags") console.log(imageTags(process.env).join("\n"));
  else if (operation === "scan") await scanImage(value);
  else throw new Error("Unknown CI container operation");
}
