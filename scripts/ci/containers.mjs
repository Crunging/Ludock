import { mkdtemp, rm } from "node:fs/promises";
import { join } from "node:path";

export const TRIVY_IMAGE = "aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969";

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
  if (operation === "scan") await scanImage(value);
  else throw new Error("Unknown CI container operation");
}
