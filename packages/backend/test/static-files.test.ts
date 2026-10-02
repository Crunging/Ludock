import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { staticFiles } from "../src/static-files.js";

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
});
