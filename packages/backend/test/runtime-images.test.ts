import { expect, describe, it, afterEach, mock, spyOn } from "bun:test";
import { FALLBACK_HELPER_IMAGE, resolveHelperImage, validateHelperImage } from "../src/runtime-images.js";
import { docker, type Container } from "../src/docker-client.js";

const previousHelper = process.env.FILE_HELPER_IMAGE;
const previousSelf = process.env.LUDOCK_SELF_CONTAINER;
const previousHostname = process.env.HOSTNAME;
afterEach(() => {
  mock.restore();
  process.env.FILE_HELPER_IMAGE = previousHelper;
  process.env.LUDOCK_SELF_CONTAINER = previousSelf;
  process.env.HOSTNAME = previousHostname;
});

describe("trusted helper image references", () => {
  it("permits digest-pinned private mirrors", () => {
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
      expect(validateHelperImage(image), name).toBe(image);
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
      expect(() => validateHelperImage(`${name}@sha256:${"a".repeat(64)}`), name).toThrow(/immutable sha256 digest/);
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
      expect(() => validateHelperImage(image)).toThrow(/immutable sha256 digest/);
    }
  });

  it("uses the running deployment's image ID and shares concurrent lookups", async () => {
    delete process.env.FILE_HELPER_IMAGE;
    process.env.LUDOCK_SELF_CONTAINER = crypto.randomUUID();
    const image = `sha256:${"b".repeat(64)}`;
    const inspect = mock(async () => ({ Image: image, Config: { Image: "ludock:latest" } }));
    const get = spyOn(docker, "getContainer").mockReturnValue({ inspect } as unknown as Container);
    expect(await Promise.all([resolveHelperImage(), resolveHelperImage()])).toEqual([image, image]);
    expect(await resolveHelperImage()).toBe(image);
    expect(inspect).toHaveBeenCalledTimes(1);
    expect(get).toHaveBeenCalledWith(process.env.LUDOCK_SELF_CONTAINER);
  });

  it("honors validated overrides without inspecting the daemon", async () => {
    const get = spyOn(docker, "getContainer");
    process.env.FILE_HELPER_IMAGE = `registry.example/helper@sha256:${"c".repeat(64)}`;
    expect(await resolveHelperImage()).toBe(process.env.FILE_HELPER_IMAGE);
    process.env.FILE_HELPER_IMAGE = "registry.example/helper:latest";
    await expect(resolveHelperImage()).rejects.toThrow("immutable sha256 digest");
    expect(get).not.toHaveBeenCalled();
  });

  it("keeps native runs automatic when no container identity is available", async () => {
    delete process.env.FILE_HELPER_IMAGE;
    delete process.env.LUDOCK_SELF_CONTAINER;
    delete process.env.HOSTNAME;
    const get = spyOn(docker, "getContainer");
    expect(await resolveHelperImage()).toBe(FALLBACK_HELPER_IMAGE);
    expect(get).not.toHaveBeenCalled();
  });

  it("uses the trusted fallback for custom hostnames without a new setting", async () => {
    delete process.env.FILE_HELPER_IMAGE;
    delete process.env.LUDOCK_SELF_CONTAINER;
    process.env.HOSTNAME = `custom-${crypto.randomUUID()}`;
    const inspect = mock().mockRejectedValue({ statusCode: 404 });
    spyOn(docker, "getContainer").mockReturnValue({ inspect } as unknown as Container);
    expect(await resolveHelperImage()).toBe(FALLBACK_HELPER_IMAGE);
    expect(await resolveHelperImage()).toBe(FALLBACK_HELPER_IMAGE);
    expect(inspect).toHaveBeenCalledTimes(1);
  });

  it("does not expose synchronous Docker lookup errors", async () => {
    delete process.env.FILE_HELPER_IMAGE;
    process.env.LUDOCK_SELF_CONTAINER = crypto.randomUUID();
    spyOn(docker, "getContainer").mockImplementation(() => { throw new Error("private Docker detail"); });
    await expect(resolveHelperImage()).rejects.toMatchObject({ code: "HELPER_IMAGE_UNAVAILABLE" });
  });

  it("retries failed discovery and rejects tags without exposing Docker errors", async () => {
    delete process.env.FILE_HELPER_IMAGE;
    process.env.LUDOCK_SELF_CONTAINER = crypto.randomUUID();
    const image = `sha256:${"d".repeat(64)}`;
    const inspect = mock()
      .mockRejectedValueOnce(new Error("private daemon detail"))
      .mockResolvedValueOnce({ Image: "ludock:latest" })
      .mockResolvedValue({ Image: image });
    spyOn(docker, "getContainer").mockReturnValue({ inspect } as unknown as Container);
    await expect(resolveHelperImage()).rejects.toMatchObject({ code: "HELPER_IMAGE_UNAVAILABLE" });
    await expect(resolveHelperImage()).rejects.toThrow("could not identify its runtime image");
    expect(await resolveHelperImage()).toBe(image);
    expect(inspect).toHaveBeenCalledTimes(3);
  });
});
