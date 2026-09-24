import { readFile, readdir } from "node:fs/promises";
import { expect, describe, it } from "bun:test";
import { FALLBACK_HELPER_IMAGE } from "../../packages/backend/src/runtime-images.ts";

const root = new URL("../../", import.meta.url);
const read = (name) => readFile(new URL(name, root), "utf8");

describe("security dependency pins", () => {
  it("pins build and fixture images to immutable manifests", async () => {
    const dockerfile = await read("Dockerfile");
    const bun = dockerfile.match(/^ARG BUN_IMAGE=(.+)$/m)?.[1];
    const alpine = dockerfile.match(/^ARG ALPINE_IMAGE=(.+)$/m)?.[1];
    expect(bun).toMatch(/^[^\s@]+@sha256:[a-f0-9]{64}$/);
    expect(FALLBACK_HELPER_IMAGE).toMatch(/^ghcr\.io\/crunging\/ludock@sha256:[a-f0-9]{64}$/);
    expect(alpine).toMatch(/^alpine:3@sha256:[a-f0-9]{64}$/);
    const fixtures = await read("scripts/test-compose.mjs");
    expect(await read("packages/backend/test/docker-client.integration.ts")).toContain(alpine);
    expect(fixtures.match(/^const expectedFixtureImage = "(.+)";$/m)?.[1]).toBe(alpine);
    const fixtureImages = [...fixtures.matchAll(/^\s+image: (.+)$/gm)].map((match) => match[1]);
    // Compose update acceptance needs a tag, whose contents the harness checks
    // against the shared immutable pin before it creates fixture services.
    expect(fixtureImages).toStrictEqual(["alpine:3", "alpine:3"]);
  });

  it("requires full immutable references for external workflow actions", async () => {
    for (const file of await readdir(new URL(".github/workflows/", root))) {
      if (!/\.ya?ml$/.test(file)) continue;
      for (const [, reference] of (await read(`.github/workflows/${file}`)).matchAll(/^\s*(?:- )?uses:\s+(\S+)/gm)) {
        if (reference.startsWith("./")) continue;
        expect(reference, `${file}: ${reference}`).toMatch(reference.startsWith("docker://")
          ? /^docker:\/\/[^\s@]+@sha256:[a-f0-9]{64}$/
          : /^[^\s@]+@[a-f0-9]{40}$/);
      }
    }
  });
});
