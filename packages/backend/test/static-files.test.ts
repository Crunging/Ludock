import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { staticFiles } from "../src/static-files.js";
import { createApp } from "../src/app.js";

let scratch: string;
let serve: ReturnType<typeof staticFiles>;
beforeEach(async () => {
  scratch = await mkdtemp(join(tmpdir(), "ludock-preview-test-"));
  const directory = join(scratch, "dist");
  await mkdir(directory);
  await writeFile(join(directory, "index.html"), "<!doctype html><h1>Ludock fixture</h1>");
  await writeFile(join(directory, "app.js"), "console.log('fixture');");
  await writeFile(join(scratch, "outside.txt"), "outside the approved static root");
  await symlink(join(scratch, "outside.txt"), join(directory, "outside.txt"));
  serve = staticFiles(directory);
});
afterEach(async () => { await rm(scratch, { recursive: true, force: true }); });

describe("shared production and preview static files", () => {
  it("serves built files and SPA routes with safe response headers", async () => {
    const response = await serve(new Request("http://localhost/servers/world"));
    expect(await response.text()).toBe("<!doctype html><h1>Ludock fixture</h1>");
    expect(response.headers.get("content-type")).toContain("text/html");
    expect(response.headers.get("cache-control")).toBe("no-cache");
    expect(response.headers.get("x-content-type-options")).toBe("nosniff");
    const script = await serve(new Request("http://localhost/app.js"));
    expect(await script.text()).toBe("console.log('fixture');");
    expect(script.headers.get("content-type")).toMatch(/javascript/);
  });

  it("returns a HEAD response without a document body", async () => {
    const response = await serve(new Request("http://localhost/servers/world", { method: "HEAD" }));
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/html");
    expect(await response.text()).toBe("");
  });

  it.each(["/api", "/API/v1/servers", "/ws", "/ws/v1/console", "/missing.js", "/.env", "/%00"])(
    "does not substitute the SPA document for %s", async (path) => {
      const response = await serve(new Request(`http://localhost${path}`));
      expect(response.status).toBe(404);
      expect(await response.text()).not.toContain("Ludock fixture");
    },
  );

  it("rejects write requests without treating them as SPA navigation", async () => {
    const response = await serve(new Request("http://localhost/servers/world", { method: "POST" }));
    expect(response.status).toBe(405);
    expect(response.headers.get("allow")).toBe("GET, HEAD");
  });

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

  it("explains when the frontend has not been built", async () => {
    const missing = staticFiles(join(scratch, "not-built"));
    const response = await missing(new Request("http://localhost/"));
    expect(response.status).toBe(503);
    expect(await response.text()).toBe("Frontend build unavailable");
  });
  it("uses the same asset and navigation behavior behind the production wrapper", async () => {
    const app = createApp({ frontendDist: join(scratch, "dist") });
    for (const pathname of ["/missing.js", "/app.js", "/servers/world", "/.env"]) {
      const request = new Request(`http://localhost${pathname}`);
      const production = await app.fetch(request, { requestIP: () => null, timeout: () => {} });
      const preview = await serve(request);
      expect(production.status).toBe(preview.status);
      expect(await production.text()).toBe(await preview.text());
      expect(production.headers.get("content-security-policy")).toContain("default-src 'self'");
    }
  });
});
