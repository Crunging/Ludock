import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import { DEFAULT_HELPER_IMAGE, getHelperImage } from "../src/runtime-images.js";

describe("trusted helper image references", () => {
  it("uses an immutable default and permits digest-pinned private mirrors", () => {
    assert.equal(getHelperImage(DEFAULT_HELPER_IMAGE), DEFAULT_HELPER_IMAGE);
    const mirror = `registry.example:5000/ludock/bun:1-alpine@sha256:${"a".repeat(64)}`;
    assert.equal(getHelperImage(mirror), mirror);
  });

  it("rejects tags, image IDs, malformed digests, and credential-bearing URLs", () => {
    for (const image of [
      "oven/bun:1-alpine", "oven/bun:latest", `sha256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(63)}`, `oven/bun@sha256:${"x".repeat(64)}`,
      `https://user:secret@registry.example/helper@sha256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(64)}\n`,
    ]) {
      assert.throws(() => getHelperImage(image), /immutable sha256 digest/);
    }
  });
});
