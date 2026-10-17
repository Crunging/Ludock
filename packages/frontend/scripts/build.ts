import { cp, rename, rm } from "node:fs/promises";
import { extname, resolve } from "node:path";

const frontend = resolve(import.meta.dir, "..");
const output = resolve(frontend, "dist");
const staging = resolve(frontend, `.dist-${process.pid}`);
// Fonts and images are already compressed; small files gain nothing.
const COMPRESSIBLE = new Set([".js", ".css", ".html", ".svg", ".json"]);
const MIN_COMPRESSED_BYTES = 1024;

try {
  const result = await Bun.build({
    entrypoints: [resolve(frontend, "index.html")],
    outdir: staging,
    target: "browser",
    minify: true,
    splitting: true,
    publicPath: "/",
    env: "disable",
    define: { "process.env.NODE_ENV": '"production"' },
    // Content-hashed files live under assets/, which the server caches as immutable.
    naming: { chunk: "assets/[name]-[hash].[ext]", asset: "assets/[name]-[hash].[ext]" },
  });
  if (!result.success) throw new AggregateError(result.logs, "Frontend build failed.");
  // The server picks these by Accept-Encoding, so requests never compress at runtime.
  for (const artifact of result.outputs) {
    if (!COMPRESSIBLE.has(extname(artifact.path))) continue;
    const bytes = new Uint8Array(await artifact.arrayBuffer());
    if (bytes.byteLength < MIN_COMPRESSED_BYTES) continue;
    await Bun.write(`${artifact.path}.zst`, Bun.zstdCompressSync(bytes, { level: 19 }));
    await Bun.write(`${artifact.path}.gz`, Bun.gzipSync(bytes, { level: 9 }));
  }
  // Keep stable public URLs available alongside the HTML's hashed asset links.
  await cp(resolve(frontend, "public"), staging, { recursive: true });
  await rm(output, { recursive: true, force: true });
  await rename(staging, output);
  console.log(`Built frontend: ${result.outputs.length} bundled files.`);
} finally {
  await rm(staging, { recursive: true, force: true });
}
