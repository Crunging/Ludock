import { cp, rename, rm } from "node:fs/promises";
import { resolve } from "node:path";

const frontend = resolve(import.meta.dir, "..");
const output = resolve(frontend, "dist");
const staging = resolve(frontend, `.dist-${process.pid}`);

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
  });
  if (!result.success) throw new AggregateError(result.logs, "Frontend build failed.");
  // Keep stable public URLs available alongside the HTML's hashed asset links.
  await cp(resolve(frontend, "public"), staging, { recursive: true });
  await rm(output, { recursive: true, force: true });
  await rename(staging, output);
  console.log(`Built frontend: ${result.outputs.length} bundled files.`);
} finally {
  await rm(staging, { recursive: true, force: true });
}
