import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { staticFiles } from "../src/static-files.js";
import { fixtureBytes } from "./fixtures/bytes.js";

let scratch: string;
let serve: ReturnType<typeof staticFiles>;
beforeEach(async () => {
  scratch = await mkdtemp(join(tmpdir(), "ludock-preview-test-"));
  const directory = join(scratch, "dist");
  await mkdir(directory);
  await writeFile(join(directory, "index.html"), "<!doctype html><h1>Ludock fixture</h1>");
  await writeFile(join(scratch, "outside.txt"), "outside the approved static root");
  await symlink(join(scratch, "outside.txt"), join(directory, "outside.txt"));
  serve = staticFiles(directory);
});
afterEach(async () => { await rm(scratch, { recursive: true, force: true }); });

describe("shared production and preview static files", () => {
  it("confines encoded paths and symlinks to the static root", async () => {
    for (const path of ["/%2e%2e%2foutside.txt", "/outside.txt"]) {
      const response = await serve(new Request(`http://localhost${path}`));
      expect(await response.text()).not.toContain("outside the approved static root");
    }
    expect((await serve(new Request("http://localhost/%ZZ"))).status).toBe(400);
  });

  it("validates the SPA fallback's real path before serving it", async () => {
    const index = join(scratch, "dist", "index.html");
    await rm(index);
    await symlink(join(scratch, "outside.txt"), index);
    const response = await serve(new Request("http://localhost/servers/world"));
    expect(response.status).toBe(404);
    expect(await response.text()).not.toContain("outside the approved static root");
  });

  it("caches hashed assets as immutable and revalidates everything else", async () => {
    const directory = join(scratch, "dist");
    await mkdir(join(directory, "assets"));
    await writeFile(join(directory, "assets", "index-abc123.js"), "console.log(1)");
    await writeFile(join(directory, "favicon.svg"), "<svg/>");
    const cache = async (path: string) =>
      (await serve(new Request(`http://localhost${path}`))).headers.get("cache-control");
    expect(await cache("/assets/index-abc123.js")).toBe("public, max-age=31536000, immutable");
    expect(await cache("/favicon.svg")).toBe("no-cache");
    expect(await cache("/servers/world")).toBe("no-cache");
  });

  it("serves precompressed siblings the client accepts", async () => {
    const assets = join(scratch, "dist", "assets");
    await mkdir(assets);
    const source = "console.log('ludock');".repeat(100);
    await writeFile(join(assets, "app-abc123.js"), source);
    await writeFile(join(assets, "app-abc123.js.zst"), Bun.zstdCompressSync(fixtureBytes(source)));
    await writeFile(join(assets, "app-abc123.js.gz"), Bun.gzipSync(fixtureBytes(source)));
    const fetchWith = (encoding: string) => serve(new Request("http://localhost/assets/app-abc123.js", {
      headers: { "Accept-Encoding": encoding },
    }));

    const zstd = await fetchWith("gzip, deflate, br, zstd");
    expect(zstd.headers.get("content-encoding")).toBe("zstd");
    expect(zstd.headers.get("content-type")).toContain("javascript");
    expect(zstd.headers.get("vary")).toBe("Accept-Encoding");
    expect(new TextDecoder().decode(Bun.zstdDecompressSync(new Uint8Array(await zstd.arrayBuffer())))).toBe(source);

    const gzip = await fetchWith("gzip, zstd;q=0");
    expect(gzip.headers.get("content-encoding")).toBe("gzip");
    expect(new TextDecoder().decode(Bun.gunzipSync(new Uint8Array(await gzip.arrayBuffer())))).toBe(source);

    const identity = await fetchWith("identity");
    expect(identity.headers.get("content-encoding")).toBe(null);
    expect(identity.headers.get("vary")).toBe("Accept-Encoding");
    expect(await identity.text()).toBe(source);
  });

  it("never follows a symlinked precompressed sibling", async () => {
    const assets = join(scratch, "dist", "assets");
    await mkdir(assets);
    await writeFile(join(assets, "app-abc123.js"), "console.log(1)");
    await writeFile(join(scratch, "outside.gz"), Bun.gzipSync(fixtureBytes("outside the approved static root")));
    await symlink(join(scratch, "outside.gz"), join(assets, "app-abc123.js.gz"));
    const response = await serve(new Request("http://localhost/assets/app-abc123.js", {
      headers: { "Accept-Encoding": "gzip" },
    }));
    expect(response.headers.get("content-encoding")).toBe(null);
    expect(await response.text()).toBe("console.log(1)");
  });
});
