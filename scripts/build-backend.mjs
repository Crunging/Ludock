import { readdir, rename, rm } from "node:fs/promises";
import { dirname, join, resolve, sep } from "node:path";

const repository = resolve(import.meta.dir, "..");
const backend = join(repository, "packages/backend");

// Use the files that contributed to the bundle, including transitive packages,
// so the inventory follows the shipped code instead of all development tools.
export async function bundleInventory(metafile, cwd = process.cwd()) {
  const directories = new Set();
  for (const output of Object.values(metafile.outputs)) {
    for (const [input, contribution] of Object.entries(output.inputs)) {
      if (!contribution.bytesInOutput) continue;
      const filename = resolve(cwd, input);
      const marker = `${sep}node_modules${sep}`;
      const offset = filename.lastIndexOf(marker);
      if (offset < 0) continue;
      const start = offset + marker.length;
      const parts = filename.slice(start).split(sep);
      directories.add(join(filename.slice(0, start), ...parts.slice(0, parts[0].startsWith("@") ? 2 : 1)));
    }
  }
  const components = [];
  const identities = new Set();
  const notices = [];
  for (const directory of [...directories].sort()) {
    const manifest = await Bun.file(join(directory, "package.json")).json();
    if (!manifest.name || !manifest.version) throw new Error(`Missing bundled package identity: ${directory}`);
    const purl = `pkg:npm/${manifest.name.replaceAll("@", "%40")}@${manifest.version}`;
    if (identities.has(purl)) continue;
    identities.add(purl);
    components.push({
      type: "library", name: manifest.name, version: manifest.version,
      "bom-ref": purl, purl,
      ...(typeof manifest.license === "string" ? { licenses: [{ license: { name: manifest.license } }] } : {}),
    });
    const files = (await readdir(directory, { withFileTypes: true }))
      .filter((entry) => entry.isFile() && /^(?:licen[cs]e|notice)(?:[._-]|$)/i.test(entry.name))
      .map((entry) => entry.name).sort();
    for (const name of files) {
      notices.push(`${manifest.name}@${manifest.version} — ${name}\n\n${await Bun.file(join(directory, name)).text()}`);
    }
  }
  components.sort((left, right) => left.purl.localeCompare(right.purl));
  const manifest = await Bun.file(join(repository, "package.json")).json();
  return {
    sbom: {
      bomFormat: "CycloneDX", specVersion: "1.5", version: 1,
      metadata: { component: { type: "application", name: "ludock-backend", version: manifest.version } },
      components,
    },
    notices: notices.join("\n\n--------------------\n\n") + "\n",
  };
}

export async function buildBackend(output = join(backend, "dist")) {
  const staging = join(dirname(output), `.dist-${process.pid}-${crypto.randomUUID()}`);
  try {
    const result = await Bun.build({
      entrypoints: [join(backend, "src/index.ts"), join(backend, "src/recovery.ts")],
      outdir: staging, target: "bun", format: "esm", packages: "bundle",
      splitting: true, sourcemap: "linked", metafile: true,
      // An unresolved dynamic import could quietly reintroduce node_modules.
      allowUnresolved: [],
    });
    if (!result.success || !result.metafile) throw new AggregateError(result.logs, "Backend build failed");
    const inventory = await bundleInventory(result.metafile);
    // The .cdx.json suffix lets Trivy discover bundled packages inside images.
    await Bun.write(join(staging, "dependencies.cdx.json"), JSON.stringify(inventory.sbom, null, 2) + "\n");
    await Bun.write(join(staging, "THIRD-PARTY-NOTICES.txt"), inventory.notices);
    await rm(output, { recursive: true, force: true });
    await rename(staging, output);
    console.log(`Built backend: ${result.outputs.length} files, ${inventory.sbom.components.length} bundled dependencies.`);
  } finally {
    await rm(staging, { recursive: true, force: true });
  }
}

if (import.meta.main) await buildBackend();
