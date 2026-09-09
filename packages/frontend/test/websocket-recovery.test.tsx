import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useWebSocket } from "../src/hooks/useWebSocket";

class FakeWebSocket {
  static OPEN = 1;
  static instances: FakeWebSocket[] = [];
  static failConstruction = false;
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  send = vi.fn<(data: string) => void>();
  close = vi.fn(() => { this.readyState = 2; });

  constructor(readonly url: string) {
    if (FakeWebSocket.failConstruction) throw new Error("private socket detail");
    FakeWebSocket.instances.push(this);
  }
  open() {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.();
  }
  serverClose(code = 1006, reason = "private close detail") {
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  FakeWebSocket.instances = [];
  FakeWebSocket.failConstruction = false;
  vi.stubGlobal("WebSocket", FakeWebSocket);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const latest = () => FakeWebSocket.instances.at(-1)!;

describe("WebSocket recovery", () => {
  it("acknowledges only a send accepted by an open socket and never resends", () => {
    const { result } = renderHook(() => useWebSocket({ url: "ws://local/one" }));
    expect(result.current.send("draft")).toBe(false);
    const ws = latest();
    act(() => ws.open());
    expect(result.current.send("first")).toBe(true);
    ws.send.mockImplementationOnce(() => { throw new Error("send race"); });
    expect(result.current.send("kept draft")).toBe(false);
    expect(ws.close).toHaveBeenCalledTimes(1);
    expect(result.current.send("still kept")).toBe(false);
    act(() => ws.serverClose());
    act(() => vi.advanceTimersByTime(2000));
    act(() => latest().open());
    expect(latest().send).not.toHaveBeenCalled();
    expect(ws.send.mock.calls.map(([data]) => data)).toEqual(["first", "kept draft"]);
  });

  it("returns false when the socket closes during sending", () => {
    const { result } = renderHook(() => useWebSocket({ url: "ws://local/one" }));
    const ws = latest();
    act(() => ws.open());
    ws.send.mockImplementation(() => { ws.readyState = 2; });
    expect(result.current.send("draft")).toBe(false);
  });

  it("ignores all callbacks and controls from a replaced URL", () => {
    const onMessage = vi.fn();
    const onOpen = vi.fn();
    const { result, rerender } = renderHook(
      ({ url }) => useWebSocket({ url, onMessage, onOpen }),
      { initialProps: { url: "ws://local/one" } },
    );
    const first = latest();
    const saved = {
      open: first.onopen!,
      message: first.onmessage!,
      close: first.onclose!,
      error: first.onerror!,
      send: result.current.send,
      retry: result.current.retry,
    };
    act(() => first.open());
    rerender({ url: "ws://local/two" });
    const second = latest();
    expect(first.close).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("connecting");
    act(() => {
      saved.open();
      saved.message({ data: "old output" });
      saved.close({ code: 1008, reason: "old denial" });
      saved.error();
      saved.retry();
      vi.advanceTimersByTime(5000);
    });
    expect(onMessage).not.toHaveBeenCalled();
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("connecting");
    expect(FakeWebSocket.instances).toHaveLength(2);
    act(() => second.open());
    expect(saved.send("old draft")).toBe(false);
    expect(result.current.send("new draft")).toBe(true);
    expect(second.send).toHaveBeenCalledWith("new draft");
    rerender({ url: "" });
    expect(result.current.status).toBe("disconnected");
    expect(result.current.canRetry).toBe(false);
    expect(result.current.error).toBeNull();
  });

  it("exhausts bounded retries even if each connection briefly opens", () => {
    const onOpen = vi.fn();
    const { result } = renderHook(() => useWebSocket({
      url: "ws://local/one", maxRetries: 2, reconnectDelay: 10, onOpen,
    }));
    for (let attempt = 0; attempt < 3; attempt += 1) {
      act(() => latest().open());
      act(() => latest().serverClose());
      act(() => vi.advanceTimersByTime(10));
    }
    expect(FakeWebSocket.instances).toHaveLength(3);
    expect(onOpen).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("disconnected");
    expect(result.current.canRetry).toBe(true);
    expect(result.current.error).toContain("Automatic retries have stopped");
    act(() => vi.advanceTimersByTime(100_000));
    expect(FakeWebSocket.instances).toHaveLength(3);
    act(() => result.current.retry());
    expect(FakeWebSocket.instances).toHaveLength(4);
    expect(result.current.canRetry).toBe(false);
    act(() => result.current.retry());
    expect(FakeWebSocket.instances).toHaveLength(4);
  });

  it.each([1008, 4001, 4003, 4401, 4403])(
    "does not automatically or manually retry access denial %i or display its reason",
    (code) => {
      const { result } = renderHook(() => useWebSocket({ url: "ws://local/one" }));
      act(() => latest().serverClose(code, "secret-token-value"));
      expect(result.current.canRetry).toBe(false);
      expect(result.current.accessDenied).toBe(true);
      expect(result.current.error).toContain("check your access");
      expect(result.current.error).not.toContain("secret-token-value");
      act(() => {
        result.current.retry();
        vi.advanceTimersByTime(100_000);
      });
      expect(FakeWebSocket.instances).toHaveLength(1);
    },
  );

  it("waits for the close code after an error so denial cannot trigger recovery", () => {
    const { result } = renderHook(() => useWebSocket({ url: "ws://local/one" }));
    act(() => latest().onerror?.());
    act(() => vi.advanceTimersByTime(5000));
    expect(FakeWebSocket.instances).toHaveLength(1);
    act(() => latest().serverClose(1008));
    act(() => result.current.retry());
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(result.current.canRetry).toBe(false);
  });

  it("cleans up pending recovery and suppresses saved callbacks after unmount", () => {
    const onMessage = vi.fn();
    const onOpen = vi.fn();
    const { result, unmount } = renderHook(() => useWebSocket({
      url: "ws://local/one", onMessage, onOpen,
    }));
    const first = latest();
    const lateMessage = first.onmessage!;
    const lateOpen = first.onopen!;
    const retry = result.current.retry;
    act(() => first.serverClose());
    unmount();
    act(() => {
      vi.advanceTimersByTime(100_000);
      lateMessage({ data: "late output" });
      lateOpen();
      retry();
    });
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(onMessage).not.toHaveBeenCalled();
    expect(onOpen).not.toHaveBeenCalled();
  });

  it("contains constructor failures and exposes manual recovery after the budget", () => {
    FakeWebSocket.failConstruction = true;
    const { result } = renderHook(() => useWebSocket({
      url: "ws://local/one", maxRetries: 1, reconnectDelay: 10,
    }));
    act(() => vi.advanceTimersByTime(10));
    expect(result.current.canRetry).toBe(true);
    expect(result.current.error).not.toContain("private socket detail");
    FakeWebSocket.failConstruction = false;
    act(() => result.current.retry());
    expect(FakeWebSocket.instances).toHaveLength(1);
    act(() => latest().open());
    expect(result.current.status).toBe("connected");
  });

  it("starts a fresh retry budget only after a stable connection", () => {
    const { result } = renderHook(() => useWebSocket({
      url: "ws://local/one", maxRetries: 1, reconnectDelay: 10,
    }));
    act(() => latest().serverClose());
    act(() => vi.advanceTimersByTime(10));
    act(() => latest().open());
    act(() => vi.advanceTimersByTime(30_000));
    act(() => latest().serverClose());
    expect(result.current.canRetry).toBe(false);
    act(() => vi.advanceTimersByTime(10));
    expect(FakeWebSocket.instances).toHaveLength(3);
  });
});
