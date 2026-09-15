#!/usr/bin/env bun
import { run, summary } from "./common.mjs";

const [baseline, candidate, platform = "linux/arm64"] = process.argv.slice(2);
if (!baseline || !candidate || process.argv.length > 5 || !["linux/arm64", "linux/amd64"].includes(platform)) {
  throw new Error("Usage: bun scripts/benchmarks/startup.mjs <baseline-image> <candidate-image> [linux/arm64|linux/amd64]");
}
const images = { baseline, candidate };
const identities = {};
for (const [label, image] of Object.entries(images)) {
  const [info] = JSON.parse(await run(["docker", "image", "inspect", "--platform", platform, image]));
  identities[label] = { image, id: info.Id, architecture: info.Architecture, os: info.Os, imageBytes: info.Size };
}

// Executed inside each image. Container launch is outside the timer, and every
// backend process receives a new SQLite database with Docker disconnected.
async function measureStartup() {
  const { mkdtemp, rm } = await import("node:fs/promises");
  const directory = await mkdtemp("/tmp/ludock-startup-benchmark-");
  const samples = [];
  async function stop(child) {
    child.kill("SIGTERM");
    const kill = setTimeout(() => child.kill("SIGKILL"), 5000);
    const code = await child.exited;
    clearTimeout(kill);
    if (code !== 0) throw new Error("Benchmark server stopped with " + code + ": " + await new Response(child.stderr).text());
  }
  try {
    for (let index = 0; index < 13; index++) {
      const started = performance.now();
      const child = Bun.spawn([process.execPath, "packages/backend/dist/index.js"], {
        env: { ...process.env, HOST: "127.0.0.1", PORT: "40401", LOG_LEVEL: "error",
          DOCKER_SOCKET: "/tmp/no-benchmark-docker.sock", LUDOCK_DB_PATH: directory + "/trial-" + index + ".db",
          LUDOCK_SETUP_CODE: "fixture-benchmark-setup-code-0123456789" },
        stdin: "ignore", stdout: "ignore", stderr: "pipe",
      });
      try {
        let ready = false;
        const deadline = performance.now() + 10_000;
        while (performance.now() < deadline) {
          try {
            const response = await fetch("http://127.0.0.1:40401/api/v1/health", { signal: AbortSignal.timeout(1000) });
            if (response.status === 503 && (await response.json()).status === "degraded") { ready = true; break; }
          } catch { /* Wait for this process's listener. */ }
          await Bun.sleep(1);
        }
        const ms = performance.now() - started;
        if (!ready) throw new Error("Benchmark server never became ready");
        if (index >= 3) samples.push({ ms });
      } finally {
        await stop(child);
      }
    }
    const disk = Bun.spawnSync(["du", "-sk", "/app"], { stdout: "pipe" });
    if (disk.exitCode !== 0) throw new Error("Could not measure application disk usage");
    console.log(JSON.stringify({ bun: Bun.version, arch: process.arch, samples,
      appDiskKiB: Number(new TextDecoder().decode(disk.stdout).split(/\s/)[0]) }));
  } finally { await rm(directory, { recursive: true, force: true }); }
}

const blocks = [];
for (let block = 0; block < 4; block++) {
  for (const label of block % 2 ? ["candidate", "baseline"] : ["baseline", "candidate"]) {
    const name = "ludock-startup-benchmark-" + crypto.randomUUID();
    try {
      const result = JSON.parse(await run(["docker", "run", "--rm", "--name", name, "--network", "none", "--platform", platform,
        "--entrypoint", "/usr/local/bin/bun", images[label], "-e", "await (" + measureStartup.toString() + ")()"]));
      if (blocks.length && blocks[0].bun !== result.bun) throw new Error("Use the same Bun version in both images");
      blocks.push({ label, ...result });
    } finally {
      // Also remove the owned container and anonymous volume if the trial fails.
      Bun.spawnSync(["docker", "rm", "-fv", name], { stdout: "ignore", stderr: "ignore" });
    }
  }
}
console.log(JSON.stringify({ benchmark: "startup", platform, images: identities,
  method: "Process spawn to first parsed degraded-health response; fresh SQLite database per trial; four alternating blocks per image, three warmups plus ten measured trials per block; Docker launch excluded; no socket or external network.",
  summary: Object.fromEntries(Object.keys(images).map((label) => [label, {
    elapsedMs: summary(blocks.filter((block) => block.label === label).flatMap((block) => block.samples)),
    appDiskKiB: blocks.find((block) => block.label === label).appDiskKiB,
  }])), blocks }, null, 2));
