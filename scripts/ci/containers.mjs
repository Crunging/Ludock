export const BUILDKIT_IMAGE = "moby/buildkit:buildx-stable-1@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8";
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

export function buildArguments(mode, environment) {
  const args = ["buildx", "build", "--pull"];
  if (mode === "check") {
    if (!["linux/amd64", "linux/arm64"].includes(environment.LUDOCK_PLATFORM)) throw new Error("Invalid test platform");
    args.push("--platform", environment.LUDOCK_PLATFORM, "--load", "--tag", "ludock:ci",
      "--cache-from", `type=gha,version=2,scope=ci-${environment.RUNNER_ARCH}`,
      "--cache-to", `type=gha,version=2,mode=max,scope=ci-${environment.RUNNER_ARCH}`);
  } else if (mode === "publish") {
    args.push("--platform", "linux/amd64,linux/arm64", "--push", "--provenance=mode=max", "--sbom=true");
    for (const tag of imageTags(environment)) args.push("--tag", tag);
    const labels = {
      "org.opencontainers.image.source": `${environment.GITHUB_SERVER_URL}/${environment.GITHUB_REPOSITORY}`,
      "org.opencontainers.image.revision": environment.GITHUB_SHA,
      "org.opencontainers.image.version": environment.SHOULD_RELEASE === "true" ? environment.RELEASE_TAG.slice(1) : "nightly",
      "org.opencontainers.image.title": "Ludock",
      "org.opencontainers.image.description": "A self-hosted control panel for existing Docker game servers",
      "org.opencontainers.image.licenses": "MIT",
    };
    for (const [key, value] of Object.entries(labels)) args.push("--label", `${key}=${value}`);
    for (const scope of ["publish", "ci-X64", "ci-ARM64"]) args.push("--cache-from", `type=gha,version=2,scope=${scope}`);
    args.push("--cache-to", "type=gha,version=2,mode=max,scope=publish");
  } else throw new Error("Unknown build mode");
  return [...args, "."];
}

function run(args) {
  const result = Bun.spawnSync(["docker", ...args], { stdin: "inherit", stdout: "inherit", stderr: "inherit" });
  if (result.exitCode !== 0) throw new Error(`Docker ${args[0]} failed (${result.exitCode})`);
}

if (import.meta.main) {
  const [operation, value] = process.argv.slice(2);
  if (operation === "setup") {
    run(["buildx", "create", "--name", "ludock", "--driver", "docker-container", "--driver-opt", `image=${BUILDKIT_IMAGE}`, "--use"]);
    run(["buildx", "inspect", "--bootstrap"]);
  } else if (operation === "build") run(buildArguments(value, process.env));
  else if (operation === "scan") {
    if (!value || value.startsWith("-")) throw new Error("An image reference is required");
    if (!process.env.RUNNER_TEMP?.startsWith("/")) throw new Error("The CI scanner requires RUNNER_TEMP");
    run(["run", "--rm", "--volume", "/var/run/docker.sock:/var/run/docker.sock",
      "--volume", `${process.env.RUNNER_TEMP}/ludock-trivy-cache:/root/.cache/trivy`, TRIVY_IMAGE,
      "image", "--scanners", "vuln", "--severity", "HIGH,CRITICAL", "--ignore-unfixed", "--exit-code", "1", "--timeout", "8m", value]);
  } else throw new Error("Unknown CI container operation");
}
