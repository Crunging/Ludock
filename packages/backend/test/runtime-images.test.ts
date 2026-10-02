import { expect, describe, it, afterEach, mock, spyOn } from "bun:test";
import { resolveHelperImage, validateHelperImage } from "../src/runtime-images.js";
import { docker, type Container } from "../src/docker-client.js";

const previousHelper = process.env.FILE_HELPER_IMAGE;
const previousSelf = process.env.LUDOCK_SELF_CONTAINER;
afterEach(() => {
  mock.restore();
  process.env.FILE_HELPER_IMAGE = previousHelper;
  process.env.LUDOCK_SELF_CONTAINER = previousSelf;
});

describe("trusted helper image references", () => {
  it("rejects tags, image IDs, malformed digests, and credential-bearing URLs", () => {
    for (const image of [
      "oven/bun:latest", `sha256:${"a".repeat(64)}`,
      `oven/bun@sha256:${"x".repeat(64)}`,
      `https://user:secret@registry.example/helper@sha256:${"a".repeat(64)}`,
    ]) {
      expect(() => validateHelperImage(image)).toThrow(/immutable sha256 digest/);
    }
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
