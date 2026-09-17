import { realpath } from "node:fs/promises";
import { join } from "node:path";

export function summary(samples, field = "ms") {
  const sorted = samples.map((sample) => sample[field]).sort((a, b) => a - b);
  if (!sorted.length || sorted.some((value) => !Number.isFinite(value))) throw new Error("Invalid benchmark samples");
  const middle = Math.floor(sorted.length / 2);
  return { n: sorted.length, median: sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2,
    p95: sorted[Math.ceil(sorted.length * 0.95) - 1], min: sorted[0], max: sorted.at(-1) };
}

export async function sourceInfo(directory) {
  directory = await realpath(directory);
  const source = join(directory, "packages/backend/src");
  const hash = new Bun.CryptoHasher("sha256");
  for (const filename of [...new Bun.Glob("**/*").scanSync({ cwd: source, onlyFiles: true })].sort()) {
    hash.update(filename + "\0").update(await Bun.file(join(source, filename)).arrayBuffer());
  }
  const reference = Bun.file(join(directory, ".benchmark-ref"));
  return { directory, source, revision: await reference.exists() ? (await reference.text()).trim() : null, sourceSha256: hash.digest("hex") };
}

export async function run(args, options = {}) {
  const child = Bun.spawn(args, { timeout: 120_000, ...options, stdin: "ignore", stdout: "pipe", stderr: "pipe" });
  const [code, stdout, stderr] = await Promise.all([child.exited, new Response(child.stdout).text(), new Response(child.stderr).text()]);
  if (code !== 0) throw new Error(args[0] + " failed: " + stderr.slice(-4000));
  return stdout;
}
