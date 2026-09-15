import { describe, expect, it } from "bun:test";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildBackend, bundleInventory } from "../build-backend.mjs";

describe("backend deployment bundle", () => {
  it("loads both entry points without installed packages and keeps maps, inventory, and notices", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-bundle-test-"));
    const output = join(directory, "dist");
    try {
      await buildBackend(output);
      const environment = { ...process.env, LUDOCK_DB_PATH: ":memory:", LUDOCK_RECOVERY_PASSWORD: "" };
      const run = (args) => Bun.spawnSync([process.execPath, "--no-install", ...args], {
        cwd: directory, env: environment, stdout: "pipe", stderr: "pipe",
      });
      const imported = run(["-e", `await import(${JSON.stringify(Bun.pathToFileURL(join(output, "index.js")).href)})`]);
      expect(imported.exitCode, new TextDecoder().decode(imported.stderr)).toBe(0);
      const recovery = run([join(output, "recovery.js")]);
      expect(recovery.exitCode).toBe(2);
      expect(new TextDecoder().decode(recovery.stderr)).toContain("Usage:");
      for (const filename of new Bun.Glob("*.js").scanSync({ cwd: output })) {
        expect(await Bun.file(join(output, filename)).text()).toContain(`//# sourceMappingURL=${filename}.map`);
        const map = await Bun.file(join(output, `${filename}.map`)).json();
        expect(map.sourcesContent.length).toBeGreaterThan(0);
      }
      const inventory = await Bun.file(join(output, "dependencies.cdx.json")).json();
      expect(inventory.bomFormat).toBe("CycloneDX");
      expect(inventory.components.map((component) => component.name)).toStrictEqual(["yaml", "zod"]);
      const notices = await Bun.file(join(output, "THIRD-PARTY-NOTICES.txt")).text();
      for (const component of inventory.components) {
        const manifest = await Bun.file(new URL(`../../packages/backend/node_modules/${component.name}/package.json`, import.meta.url)).json();
        expect(component.version).toBe(manifest.version);
        expect(component.purl).toBe(`pkg:npm/${component.name}@${manifest.version}`);
        expect(notices).toContain(`${component.name}@${manifest.version}`);
      }
      expect(notices).toContain("Permission");
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  });

  it("records contributing scoped/transitive packages once and excludes unused dependencies", async () => {
    const directory = await mkdtemp(join(tmpdir(), "ludock-inventory-test-"));
    try {
      for (const name of ["node_modules/@fixture/parent", "node_modules/child", "node_modules/child-copy"]) {
        const scoped = name.includes("@fixture");
        await Bun.write(join(directory, name, "package.json"), JSON.stringify({
          name: scoped ? "@fixture/parent" : "child", version: "1.2.3", license: "MIT",
        }));
        await Bun.write(join(directory, name, "LICENSE"), "Fixture license text\n");
      }
      const { sbom, notices } = await bundleInventory({ outputs: { "index.js": { inputs: {
        "src/index.ts": { bytesInOutput: 1 },
        "node_modules/@fixture/parent/lib/index.js": { bytesInOutput: 1 },
        "node_modules/child/index.js": { bytesInOutput: 1 },
        "node_modules/child-copy/index.js": { bytesInOutput: 1 },
        "node_modules/unused/index.js": { bytesInOutput: 0 },
      } } } }, directory);
      expect(sbom.components.map((component) => component.purl)).toStrictEqual([
        "pkg:npm/%40fixture/parent@1.2.3", "pkg:npm/child@1.2.3",
      ]);
      expect(notices.match(/Fixture license text/g)?.length).toBe(2);
      await Bun.write(join(directory, "node_modules/child/package.json"), "{}");
      await expect(bundleInventory({ outputs: { "index.js": { inputs: {
        "node_modules/child/index.js": { bytesInOutput: 1 },
      } } } }, directory)).rejects.toThrow("Missing bundled package identity");
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  });
});
