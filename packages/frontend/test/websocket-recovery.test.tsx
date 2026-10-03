import { afterEach, beforeEach, describe, expect, it, mock, jest } from "bun:test";
import { act, renderHook } from "@testing-library/react";
import { useWebSocket } from "../src/hooks/useWebSocket";

class FakeWebSocket {
  static OPEN = 1;
  static instances: FakeWebSocket[] = [];
  readyState = 0;
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  send = mock<(data: string) => void>();
  close = mock(() => { this.readyState = 2; });

  constructor(readonly url: string) {
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

const originalWebSocket = globalThis.WebSocket;

beforeEach(() => {
  jest.useFakeTimers();
  FakeWebSocket.instances = [];
  globalThis.WebSocket = FakeWebSocket as unknown as typeof WebSocket;
});
afterEach(() => {
  jest.useRealTimers();
  globalThis.WebSocket = originalWebSocket;
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
    act(() => jest.advanceTimersByTime(2000));
    act(() => latest().open());
    expect(latest().send).not.toHaveBeenCalled();
    expect(ws.send.mock.calls.map(([data]) => data)).toEqual(["first", "kept draft"]);
  });

  it("ignores all callbacks and controls from a replaced URL", () => {
    const onMessage = mock();
    const onOpen = mock();
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
      jest.advanceTimersByTime(5000);
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

  it("waits for the close code after an error so denial cannot trigger recovery", () => {
    const { result } = renderHook(() => useWebSocket({ url: "ws://local/one" }));
    act(() => latest().onerror?.());
    act(() => jest.advanceTimersByTime(5000));
    expect(FakeWebSocket.instances).toHaveLength(1);
    act(() => latest().serverClose(1008));
    act(() => result.current.retry());
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(result.current.canRetry).toBe(false);
  });
});
