import { hardenedContainerArguments } from "./test-container-options.mjs";

// Keep the CLI and the mounted socket on the same dedicated test daemon.
// Docker Desktop exposes a host proxy socket and a different VM-side socket;
// compare daemon IDs before allowing tests to use either mapping.
export function createTestDocker({ environment = process.env, secrets = [] } = {}) {
  const env = { ...environment };
  let endpoint;
  const run = (args, stdout = "pipe") => {
    const command = ["docker", ...(endpoint ? ["--host", endpoint] : []), ...args];
    const result = Bun.spawnSync(command, { env, stdin: "inherit", stdout, stderr: stdout === "inherit" ? "inherit" : "pipe" });
    if (result.exitCode !== 0) {
      let message = new TextDecoder().decode(result.stderr ?? undefined).slice(-2000);
      for (const secret of secrets) if (secret) message = message.replaceAll(secret, "[redacted]");
      throw new Error(`Docker ${args[0]} failed (${result.exitCode}): ${message}`);
    }
    return result;
  };
  const docker = (...args) => new TextDecoder().decode(run(args).stdout).trim();
  // DOCKER_CONTEXT takes precedence over DOCKER_HOST, just as in the CLI.
  endpoint = env.DOCKER_CONTEXT || !env.DOCKER_HOST
    ? JSON.parse(docker("context", "inspect", ...(env.DOCKER_CONTEXT ? [env.DOCKER_CONTEXT] : []),
      "--format", "{{json .Endpoints.docker.Host}}"))
    : env.DOCKER_HOST;
  if (typeof endpoint !== "string" || !/^unix:\/\/\/[^\r\n]+$/.test(endpoint)) {
    throw new Error("Docker acceptance requires a local Unix-socket context or DOCKER_HOST.");
  }
  // Pin every later command even if the user's current context changes.
  delete env.DOCKER_CONTEXT;
  delete env.DOCKER_HOST;
  const selected = JSON.parse(docker("info", "--format", '{{json .}}'));
  if (typeof selected.ID !== "string" || !selected.ID) throw new Error("Docker did not report a daemon ID.");
  const socket = env.LUDOCK_TEST_DOCKER_SOCKET ||
    (selected.OperatingSystem === "Docker Desktop" ? "/var/run/docker.sock" : endpoint.slice("unix://".length));
  if (!socket.startsWith("/") || /[,\r\n]/.test(socket)) {
    throw new Error("LUDOCK_TEST_DOCKER_SOCKET must be an absolute daemon-visible Unix socket path without commas.");
  }
  // --mount fails for a missing source instead of creating a host directory.
  const socketArguments = ["--mount", `type=bind,source=${socket},target=/var/run/docker.sock,readonly`];
  const image = env.LUDOCK_TEST_IMAGE || "ludock:test";
  const mountedId = docker("run", "--rm", "--pull=never", "--network", "none",
    "--label", "ludock.enable=false", ...hardenedContainerArguments, ...socketArguments,
    "--entrypoint", "docker", image, "--host", "unix:///var/run/docker.sock", "info", "--format", "{{.ID}}");
  if (mountedId !== selected.ID) {
    throw new Error("The mounted Docker socket belongs to a different daemon. Check the selected test context and LUDOCK_TEST_DOCKER_SOCKET.");
  }
  const removeContainer = (name) => {
    if (docker("container", "ls", "--all", "--quiet", "--filter", `name=^/${name}$`))
      docker("rm", "-fv", name);
  };
  return { docker, run, image, socketArguments, removeContainer };
}

export async function waitForReady(check, message, timeout = 20_000) {
  const deadline = Date.now() + timeout;
  let lastError;
  while (Date.now() < deadline) {
    try { await check(); return; }
    catch (error) { lastError = error; }
    await Bun.sleep(200);
  }
  throw new Error(message, { cause: lastError });
}

// Preserve the original failure while making leftover fixture resources visible.
export async function cleanupFixtures(actions) {
  const errors = [];
  for (const action of actions) {
    try { await action(); } catch (error) { errors.push(error); }
  }
  if (errors.length) {
    console.error(new AggregateError(errors, "Docker fixture cleanup failed"));
    process.exitCode = 1;
  }
}
