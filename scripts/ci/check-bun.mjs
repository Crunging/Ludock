// .bun-version may select a major release ("1"); package.json supplies the
// minimum supported version. The image digest pins the exact CI executable.
const selected = (await Bun.file(".bun-version").text()).trim();
const required = (await Bun.file("package.json").json()).engines?.bun;
for (const [source, range] of [[".bun-version", selected], ["package.json engines.bun", required]]) {
  if (typeof range !== "string" || !range.trim() || !Bun.semver.satisfies(Bun.version, range)) {
    throw new Error(`Bun ${Bun.version} does not satisfy ${source}: ${String(range)}`);
  }
}
