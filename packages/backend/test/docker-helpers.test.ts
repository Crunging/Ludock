import { PassThrough } from "node:stream";
import type Docker from "dockerode";
import { afterEach, describe, expect, it, mock, spyOn } from "bun:test";
import { docker } from "../src/docker-client.js";
import { createHelperContainer, removeHelperContainer } from "../src/docker-helpers.js";

const options = { Image: "fixture@sha256:" + "a".repeat(64), Labels: { "ludock.enable": "false" } };
const missing = Object.assign(new Error("missing"), { statusCode: 404 });
const unavailable = Object.assign(new Error("private Docker detail"), { statusCode: 503 });
afterEach(() => mock.restore());

describe("Docker helper lifetime", () => {
  it("pulls a missing image to completion and retries the same creation options", async () => {
    const helper = { id: "fixture" } as Docker.Container;
    const create = spyOn(docker, "createContainer").mockRejectedValueOnce(missing).mockResolvedValue(helper);
    const stream = new PassThrough();
    const pull = spyOn(docker, "pull").mockResolvedValue(stream);
    const progress = spyOn(docker.modem, "followProgress").mockImplementation((_stream, done) => done(null, []));
    expect(await createHelperContainer(options)).toBe(helper);
    expect(create.mock.calls).toEqual([[options], [options]]);
    expect(pull).toHaveBeenCalledWith(options.Image);
    expect(progress).toHaveBeenCalledWith(stream, expect.any(Function));
  });

  it("does not pull on a daemon failure or retry after a failed pull", async () => {
    const create = spyOn(docker, "createContainer").mockRejectedValueOnce(unavailable).mockRejectedValue(missing);
    const pull = spyOn(docker, "pull").mockResolvedValue(new PassThrough());
    spyOn(docker.modem, "followProgress").mockImplementation((_stream, done) => done(unavailable, []));
    await expect(createHelperContainer(options)).rejects.toBe(unavailable);
    expect(pull).not.toHaveBeenCalled();
    await expect(createHelperContainer(options)).rejects.toBe(unavailable);
    expect(create).toHaveBeenCalledTimes(2);
  });

  for (const initiallyMissing of [false, true]) {
    it(`finishes cleanup without stopping an already ${initiallyMissing ? "missing" : "removed"} helper`, async () => {
      const remove = mock(async () => { if (initiallyMissing) throw missing; });
      const stop = mock(async () => {});
      await removeHelperContainer({ remove, stop } as unknown as Docker.Container);
      expect(remove).toHaveBeenCalledWith({ force: true });
      expect(stop).not.toHaveBeenCalled();
    });
  }

  it("stops a writer and retries removal when the first removal fails", async () => {
    const calls: string[] = [];
    const remove = mock(async () => { calls.push("remove"); if (calls.length === 1) throw unavailable; });
    const stop = mock(async () => { calls.push("stop"); });
    await removeHelperContainer({ remove, stop } as unknown as Docker.Container);
    expect(calls).toEqual(["remove", "stop", "remove"]);
    expect(stop).toHaveBeenCalledWith({ t: 0 });
  });

  for (const statusCode of [304, 404]) {
    it(`tolerates a concurrently stopped or removed helper (${statusCode})`, async () => {
      const remove = mock().mockRejectedValueOnce(unavailable).mockRejectedValue(missing);
      const stop = mock().mockRejectedValue({ statusCode });
      await removeHelperContainer({ remove, stop } as unknown as Docker.Container);
      expect(remove).toHaveBeenCalledTimes(2);
    });
  }

  it("reports incomplete cleanup without exposing Docker diagnostics", async () => {
    const remove = mock().mockRejectedValue(unavailable);
    const stop = mock().mockResolvedValue(undefined);
    await expect(removeHelperContainer({ remove, stop } as unknown as Docker.Container)).rejects.toMatchObject({
      code: "HELPER_CLEANUP_FAILED", statusCode: 409,
      message: "A temporary data helper could not be removed. Check Docker and administrator diagnostics before retrying.",
    });
  });
});
