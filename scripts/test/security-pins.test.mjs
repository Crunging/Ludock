import assert from "node:assert/strict";
import { readFile, readdir } from "node:fs/promises";
import { describe, it } from "bun:test";
import { DEFAULT_HELPER_IMAGE } from "../../packages/backend/src/runtime-images.ts";

const root = new URL("../../", import.meta.url);
const read = (name) => readFile(new URL(name, root), "utf8");

describe("security dependency pins", () => {
  it("keeps the build and privileged helpers on the same immutable Bun image", async () => {
    const dockerfile = await read("Dockerfile");
    const bun = dockerfile.match(/^ARG BUN_IMAGE=(.+)$/m)?.[1];
    const alpine = dockerfile.match(/^ARG ALPINE_IMAGE=(.+)$/m)?.[1];
    assert.equal(bun, DEFAULT_HELPER_IMAGE);
    assert.match(bun, /^oven\/bun:1-alpine@sha256:[a-f0-9]{64}$/);
    assert.match(alpine, /^alpine:3@sha256:[a-f0-9]{64}$/);
    const fixtures = await read("scripts/test-compose.mjs");
    assert.equal(fixtures.match(/^const expectedFixtureImage = "(.+)";$/m)?.[1], alpine);
    const fixtureImages = [...fixtures.matchAll(/^\s+image: (.+)$/gm)].map((match) => match[1]);
    // Compose update acceptance needs a tag, whose contents the harness checks
    // against the shared immutable pin before it creates fixture services.
    assert.deepEqual(fixtureImages, ["alpine:3", "alpine:3"]);
  });

  it("requires full immutable references for external workflow actions", async () => {
    for (const file of await readdir(new URL(".github/workflows/", root))) {
      if (!/\.ya?ml$/.test(file)) continue;
      for (const [, reference] of (await read(`.github/workflows/${file}`)).matchAll(/^\s*(?:- )?uses:\s+(\S+)/gm)) {
        if (reference.startsWith("./")) continue;
        assert.match(reference, reference.startsWith("docker://")
          ? /^docker:\/\/[^\s@]+@sha256:[a-f0-9]{64}$/
          : /^[^\s@]+@[a-f0-9]{40}$/, `${file}: ${reference}`);
      }
    }
  });
});
