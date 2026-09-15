import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, mock } from "bun:test";
import { usePageRead } from "../src/hooks/usePageRead";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

describe("page reads", () => {
  it("retains a polling snapshot only for the same enabled reader", async () => {
    const pending = deferred<string>();
    const read = mock<(signal: AbortSignal) => Promise<string>>()
      .mockResolvedValueOnce("Saved snapshot").mockReturnValueOnce(pending.promise);
    const view = renderHook(({ enabled }) => usePageRead(enabled ? read : null, "Read failed", { retainWhileRefreshing: true }), {
      initialProps: { enabled: true },
    });
    await waitFor(() => expect(view.result.current.data).toBe("Saved snapshot"));
    act(() => { void view.result.current.refresh(false); });
    expect(view.result.current.data).toBe("Saved snapshot");
    expect(view.result.current.loading).toBe(true);
    await act(async () => { await view.result.current.refresh(false); });
    expect(read).toHaveBeenCalledTimes(2);
    view.rerender({ enabled: false });
    expect(read.mock.calls[1][0].aborted).toBe(true);
    expect(view.result.current.data).toBeNull();
    await act(async () => { pending.resolve("Obsolete snapshot"); });
    expect(view.result.current.data).toBeNull();
    expect(view.result.current.loading).toBe(false);
  });
  it.each(["success", "failure"] as const)("ignores a superseded read's late %s", async (outcome) => {
    const obsolete = deferred<string>();
    const read = mock<(signal: AbortSignal) => Promise<string>>()
      .mockReturnValueOnce(obsolete.promise)
      .mockResolvedValueOnce("Current result");
    const view = renderHook(() => usePageRead(read, "Read failed"));
    const firstSignal = read.mock.calls[0][0];

    await act(async () => { await view.result.current.refresh(); });
    expect(firstSignal.aborted).toBe(true);
    expect(view.result.current.data).toBe("Current result");
    await act(async () => {
      if (outcome === "success") obsolete.resolve("Obsolete result");
      else obsolete.reject(new Error("Obsolete error"));
    });
    expect(view.result.current.data).toBe("Current result");
    expect(view.result.current.error).toBeNull();
    expect(view.result.current.loading).toBe(false);
    expect(read).toHaveBeenCalledTimes(2);
  });

  it("hides a previous result throughout refresh and failure, then recovers", async () => {
    const refresh = deferred<string>();
    const read = mock<(signal: AbortSignal) => Promise<string>>()
      .mockResolvedValueOnce("Previous result")
      .mockReturnValueOnce(refresh.promise)
      .mockResolvedValueOnce("Recovered result");
    const view = renderHook(() => usePageRead(read, "Read failed"));
    await waitFor(() => expect(view.result.current.data).toBe("Previous result"));

    act(() => { void view.result.current.refresh(); });
    expect(view.result.current.data).toBeNull();
    expect(view.result.current.loading).toBe(true);
    await act(async () => { refresh.reject(new Error("Read unavailable")); });
    expect(view.result.current.data).toBeNull();
    expect(view.result.current.error).toBe("Read unavailable");
    expect(view.result.current.loading).toBe(false);

    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.data).toBe("Recovered result");
    expect(view.result.current.error).toBeNull();
  });

  it("cancels replaced readers and unmounted reads without starting further work", async () => {
    const first = deferred<string>();
    const second = deferred<string>();
    const firstRead = mock<(signal: AbortSignal) => Promise<string>>().mockReturnValue(first.promise);
    const secondRead = mock<(signal: AbortSignal) => Promise<string>>().mockReturnValue(second.promise);
    const view = renderHook(({ read }) => usePageRead(read, "Read failed"), {
      initialProps: { read: firstRead },
    });
    const retiredRefresh = view.result.current.refresh;
    view.rerender({ read: secondRead });
    await act(async () => { await retiredRefresh(); });
    expect(firstRead).toHaveBeenCalledTimes(1);
    expect(secondRead.mock.calls[0][0].aborted).toBe(false);
    expect(firstRead.mock.calls[0][0].aborted).toBe(true);
    await act(async () => { first.resolve("Previous page result"); });
    expect(view.result.current.data).toBeNull();
    expect(view.result.current.loading).toBe(true);

    const refresh = view.result.current.refresh;
    view.unmount();
    expect(secondRead.mock.calls[0][0].aborted).toBe(true);
    await act(async () => { second.resolve("Unmounted result"); });
    await refresh();
    expect(secondRead).toHaveBeenCalledTimes(1);
  });
});
