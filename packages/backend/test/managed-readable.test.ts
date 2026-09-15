import { describe, expect, it } from "bun:test";
import { managedReadable } from "../src/managed-readable.js";

const tick = () => new Promise<void>(resolve => setImmediate(resolve));

describe("stream resource ownership", () => {
  it("checks completion before EOF and retains ownership until cleanup finishes", async () => {
    const release = Promise.withResolvers<void>();
    let checked = false, cleaned = false, completed = false;
    const output = managedReadable(new Response("payload").body!, {
      complete() { checked = true; },
      async cleanup() { await release.promise; cleaned = true; },
    });
    void output.completed.then(() => { completed = true; });
    expect(await new Response(output.stream).text()).toBe("payload");
    expect(checked).toBe(true);
    expect(completed).toBe(false);
    release.resolve();
    await output.completed;
    expect(cleaned).toBe(true);
  });

  it("cancels an idle source and waits for cleanup after revocation", async () => {
    const revoke = new AbortController();
    const release = Promise.withResolvers<void>();
    let cancelled = false, calls = 0, completed = false;
    const source = new ReadableStream<Uint8Array>({ cancel() { cancelled = true; } });
    const output = managedReadable(source, {
      signal: revoke.signal,
      async cleanup() { calls++; await release.promise; },
    });
    void output.completed.then(() => { completed = true; });
    await tick();
    revoke.abort(new Error("Access revoked"));
    await expect(new Response(output.stream).text()).rejects.toThrow("Access revoked");
    expect(cancelled).toBe(true);
    expect(completed).toBe(false);
    release.resolve();
    await output.completed;
    expect(calls).toBe(1);
  });

  it("stops prefetching a large source and cleans up when its reader cancels", async () => {
    let reads = 0, cancelled = false, cleaned = false;
    const output = managedReadable(new ReadableStream<Uint8Array>({
      pull(controller) { reads++; controller.enqueue(new Uint8Array(65_536)); },
      cancel() { cancelled = true; },
    }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 })), {
      cleanup() { cleaned = true; },
    });
    await tick();
    expect(reads).toBeLessThanOrEqual(3);
    await output.stream.cancel();
    await output.completed;
    expect(cancelled && cleaned).toBe(true);
  });

  it("cleans up when the source fails behind a paused output queue", async () => {
    let source!: ReadableStreamDefaultController<Uint8Array>;
    let cleaned = false;
    const output = managedReadable(new ReadableStream<Uint8Array>({
      start(controller) { source = controller; controller.enqueue(new Uint8Array(65_536)); },
    }), { cleanup() { cleaned = true; } });
    await tick();
    source.error(new Error("Connection lost"));
    await output.completed;
    expect(cleaned).toBe(true);
    await expect(new Response(output.stream).text()).rejects.toThrow("Connection lost");
  });

  it("fails the response when the final helper status rejects success", async () => {
    let cleaned = false;
    const output = managedReadable(new Response("partial output").body!, {
      complete() { throw new Error("Helper failed"); },
      cleanup() { cleaned = true; },
    });
    await expect(new Response(output.stream).text()).rejects.toThrow("Helper failed");
    await output.completed;
    expect(cleaned).toBe(true);
  });

  it("does not publish EOF or repeat cleanup after cancellation during the final check", async () => {
    const checking = Promise.withResolvers<void>(), release = Promise.withResolvers<void>();
    const abort = new AbortController();
    let calls = 0;
    const output = managedReadable(new Response("payload").body!, {
      signal: abort.signal,
      async complete() { checking.resolve(); await release.promise; },
      cleanup() { calls++; },
    });
    const reading = new Response(output.stream).text();
    await checking.promise;
    abort.abort(new Error("Disconnected"));
    await expect(reading).rejects.toThrow("Disconnected");
    await output.completed;
    release.resolve();
    await tick();
    expect(calls).toBe(1);
  });
});
