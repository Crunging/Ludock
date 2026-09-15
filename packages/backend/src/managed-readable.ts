/** A streaming response owns its resources until EOF, cancellation, or failure.
 * Completion checks run before publishing EOF; cleanup also runs for an idle
 * consumer when its signal aborts. `completed` includes all cleanup work. */
export function managedReadable(
  source: ReadableStream<Uint8Array>,
  options: {
    signal?: AbortSignal;
    complete?(): void | Promise<void>;
    cleanup?(): void | Promise<void>;
    mapError?(error: unknown): unknown;
  } = {},
): { stream: ReadableStream<Uint8Array>; completed: Promise<void> } {
  const reader = source.getReader();
  const completion = Promise.withResolvers<void>();
  void completion.promise.catch(() => {});
  let cleaning: Promise<void> | undefined;
  let stopped = false;
  let controller: ReadableStreamDefaultController<Uint8Array>;
  const cleanup = () => cleaning ??= (async () => {
    options.signal?.removeEventListener("abort", abort);
    try {
      await reader.cancel().catch(() => {});
      reader.releaseLock();
      await options.cleanup?.();
      completion.resolve();
    } catch (error) {
      completion.reject(error);
      throw error;
    }
  })();
  const fail = (error: unknown) => {
    if (stopped) return;
    stopped = true;
    controller.error(options.mapError?.(error) ?? error);
    void cleanup().catch(() => {});
  };
  const abort = () => fail(options.signal!.reason);
  const stream = new ReadableStream<Uint8Array>({
    start(target) {
      controller = target;
      options.signal?.addEventListener("abort", abort, { once: true });
      if (options.signal?.aborted) abort();
    },
    async pull() {
      try {
        const { value, done } = await reader.read();
        if (stopped) return;
        if (!done) { controller.enqueue(value); return; }
        await options.complete?.();
        if (!stopped) { stopped = true; controller.close(); }
        await cleanup();
      } catch (error) { fail(error); }
    },
    cancel() { stopped = true; return cleanup(); },
  }, new ByteLengthQueuingStrategy({ highWaterMark: 65_536 }));
  // A source can fail while the HTTP reader is paused behind its own queue.
  void reader.closed.catch(fail);
  return { stream, completed: completion.promise };
}
