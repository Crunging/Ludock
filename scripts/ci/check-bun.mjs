// Development and CI use the same exact release as the pinned Docker images.
const selected = (await Bun.file(".bun-version").text()).trim();
if (!/^\d+\.\d+\.\d+$/.test(selected) || Bun.version !== selected) {
  throw new Error(`Bun ${Bun.version} does not match .bun-version: ${selected}. Install the pinned release.`);
}
const required = (await Bun.file("package.json").json()).engines?.bun;
if (typeof required !== "string" || !required.trim() || !Bun.semver.satisfies(Bun.version, required)) {
  throw new Error(`Bun ${Bun.version} does not satisfy package.json engines.bun: ${String(required)}`);
}
