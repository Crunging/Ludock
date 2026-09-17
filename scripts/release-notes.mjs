#!/usr/bin/env bun

/** Publish the release-please entry without inventing a second changelog policy. */
export function releaseNotes(changelog, tag) {
  if (!/^v\d+\.\d+\.\d+$/.test(tag)) throw new Error("Expected a stable vMAJOR.MINOR.PATCH tag");
  const version = tag.slice(1);
  const headings = [...changelog.matchAll(/^## (.+)$/gm)];
  const entries = headings.flatMap((heading, index) => {
    const match = heading[1].match(/^(?:\[([^\]]+)\]\([^)]*\)|(\d+\.\d+\.\d+))(?:\s|$)/);
    if ((match?.[1] ?? match?.[2]) !== version) return [];
    const body = changelog.slice(heading.index + heading[0].length, headings[index + 1]?.index).trim();
    if (!body) throw new Error(`Changelog entry for ${tag} is empty`);
    return [`${heading[0]}\n\n${body}\n`];
  });
  if (entries.length !== 1) throw new Error(`Expected exactly one changelog entry for ${tag}`);
  return entries[0];
}

if (import.meta.main) {
  const [tag, output] = process.argv.slice(2);
  if (!output) throw new Error("Usage: bun scripts/release-notes.mjs <tag> <output>");
  await Bun.write(output, releaseNotes(await Bun.file("CHANGELOG.md").text(), tag));
}
