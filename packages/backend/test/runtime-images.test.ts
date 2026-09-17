import { expect, describe, it } from "bun:test";
import { DEFAULT_HELPER_IMAGE, getHelperImage } from "../src/runtime-images.js";

describe("trusted helper image references", () => {
  it("uses an immutable default and permits digest-pinned private mirrors", () => {
    expect(getHelperImage(DEFAULT_HELPER_IMAGE)).toBe(DEFAULT_HELPER_IMAGE);
    for (const name of [
      "bun", "oven/bun", "oven/bun:1-alpine", "oven/bun:_RC.1-ALPINE",
      "registry.example:5000/ludock/bun:1-alpine", "REGISTRY.example/ludock/bun",
      "localhost/bun", "localhost:5000/bun", "registry:5000/bun",
      "127.0.0.1:5000/bun", "[::1]/bun", "[2001:db8::1]:5000/ludock/bun:1",
      "registry.example/team_name/bun__helper---alpine.1",
      `registry.example/${"a".repeat(255)}`, "a".repeat(247),
      `oven/bun:${"a".repeat(128)}`,
    ]) {
      const image = `${name}@sha256:${"a".repeat(64)}`;
      expect(getHelperImage(image), name).toBe(image);
    }
  });

  it("rejects malformed repository names, registry hosts, ports, and tags", () => {
    for (const name of [
      "", "Bun", "oven/Bun", "registry.example/Team/bun", "oven//bun",
      "/oven/bun", "oven/bun/", "oven/.bun", "oven/bun-", "oven/bun..helper",
      "oven/bun___helper", "registry..example/bun", "-registry.example/bun",
      "registry-.example/bun", "registry_name.example/bun",
      "registry.example:/bun", "registry.example:port/bun", "registry.example:5000:5001/bun",
      "::1/bun", "[::g]/bun", "[fe80::1%eth0]/bun", "[::ffff:127.0.0.1]/bun",
      "[::1]:port/bun", "[::1]:/bun", "oven/bun:", "oven/bun:-alpine",
      "oven/bun:.alpine", "oven/bun:alpine+1", "oven/bun:tag:other",
      `oven/bun:${"a".repeat(129)}`, `registry.example/${"a".repeat(256)}`,
      "a".repeat(248), `docker.io/${"a".repeat(248)}`,
      `index.docker.io/${"a".repeat(248)}`,
    ]) {
      expect(() => getHelperImage(`${name}@sha256:${"a".repeat(64)}`), name).toThrow(/immutable sha256 digest/);
    }
  });

  it("rejects tags, image IDs, malformed digests, and credential-bearing URLs", () => {
    for (const image of [
      "oven/bun:1-alpine", "oven/bun:latest", `sha256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(63)}`, `oven/bun@sha256:${"x".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(65)}`, `oven/bun@sha256:${"A".repeat(64)}`,
      `oven/bun@sha512:${"a".repeat(128)}`, `oven/bun@SHA256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(64)}@sha256:${"b".repeat(64)}`,
      `https://user:secret@registry.example/helper@sha256:${"a".repeat(64)}`,
      `https://registry.example/helper@sha256:${"a".repeat(64)}`,
      ` oven/bun@sha256:${"a".repeat(64)}`,
      `oven/bun\n@sha256:${"a".repeat(64)}`,
      `oven/bun:tag\n@sha256:${"a".repeat(64)}`,
      `registry.example\n/bun@sha256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"a".repeat(64)}\n`,
    ]) {
      expect(() => getHelperImage(image)).toThrow(/immutable sha256 digest/);
    }
  });
});
