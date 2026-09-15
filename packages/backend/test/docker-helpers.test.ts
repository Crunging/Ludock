import type * as Docker from "../src/docker-client.js";
import { afterEach, describe, expect, it, mock, spyOn } from "bun:test";
import { docker } from "../src/docker-client.js";
import { createHelperContainer, removeHelperContainer } from "../src/docker-helpers.js";

const options = { Image: "fixture@sha256:" + "a".repeat(64), Labels: { "ludock.enable": "false" } };
const missing = Object.assign(new Error("missing"), { statusCode: 404 });
const unavailable = Object.assign(new Error("private Docker detail"), { statusCode: 503 });
const removing = Object.assign(new Error("private removal conflict"), { statusCode: 409 });
afterEach(() => mock.restore());

describe("Docker helper lifetime", () => {
  it("pulls a missing image to completion and retries the same creation options", async () => {
    const helper = { id: "fixture" } as Docker.Container;
    const create = spyOn(docker, "createContainer").mockRejectedValueOnce(missing).mockResolvedValue(helper);
    const pull = spyOn(docker, "pull").mockResolvedValue(undefined);
    expect(await createHelperContainer(options)).toBe(helper);
    expect(create.mock.calls).toEqual([[options], [options]]);
    expect(pull).toHaveBeenCalledWith(options.Image);
  });

  it("does not pull on a daemon failure or retry after a failed pull", async () => {
    const create = spyOn(docker, "createContainer").mockRejectedValueOnce(unavailable).mockRejectedValue(missing);
    const pull = spyOn(docker, "pull").mockRejectedValue(unavailable);
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

  it("waits for concurrent auto-removal instead of reporting cleanup failure", async () => {
    let resume!: () => void;
    const waiting = new Promise<void>((resolve) => { resume = resolve; });
    spyOn(Bun, "sleep").mockImplementation(async () => { await waiting; });
    const remove = mock().mockRejectedValueOnce(removing).mockRejectedValue(missing);
    const stop = mock().mockRejectedValue(removing);
    let finished = false;
    const cleanup = removeHelperContainer({ remove, stop } as unknown as Docker.Container)
      .then(() => { finished = true; });
    try {
      await Promise.resolve();
      await Promise.resolve();
      expect(finished).toBe(false);
      resume();
      await cleanup;
      expect(remove).toHaveBeenCalledTimes(2);
      expect(stop).not.toHaveBeenCalled();
    } finally {
      resume();
      await cleanup.catch(() => {});
    }
  });

  it("bounds removal conflicts and still reports a helper that cannot be removed", async () => {
    spyOn(Bun, "sleep").mockImplementation(async () => {});
    const remove = mock().mockRejectedValue(removing);
    const stop = mock().mockRejectedValue(removing);
    await expect(removeHelperContainer({ remove, stop } as unknown as Docker.Container)).rejects.toMatchObject({
      code: "HELPER_CLEANUP_FAILED", statusCode: 409,
    });
    expect(remove.mock.calls.length).toBeGreaterThan(1);
    expect(remove.mock.calls.length).toBeLessThanOrEqual(21);
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
