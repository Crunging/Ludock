import type { Server } from "@ludock/shared";
import { beforeEach, describe, expect, it, mock } from "bun:test";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { useServers } from "../src/hooks/useServers";
import { useWebSocket } from "../src/hooks/useWebSocket";
import { apiJson, ApiRequestError } from "../src/api";
import { AuthContext, type AuthContextValue, type AuthUser } from "../src/auth-context";

const originalApi = { ...await import("../src/api") };
const apiJsonMock = mock<typeof apiJson>();
mock.module("../src/api", () => ({
  ...originalApi,
  apiJson: apiJsonMock,
}));
const useWebSocketMock = mock<typeof useWebSocket>();
mock.module("../src/hooks/useWebSocket", () => ({ useWebSocket: useWebSocketMock }));

const server = { id: "server-1", displayName: "World", permissions: ["server.view"] } as Server;
const refreshEvent = JSON.stringify({ type: "container_event", action: "refresh", time: 1 });
const administrator: AuthUser = { id: "admin", username: "admin", role: "admin" };
let currentUser = administrator;
let transport: ReturnType<typeof useWebSocket>;
let callbacks: Parameters<typeof useWebSocket>[0];

function wrapper({ children }: { children: ReactNode }) {
  return <AuthContext.Provider value={{ user: currentUser } as AuthContextValue}>{children}</AuthContext.Provider>;
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

beforeEach(() => {
  currentUser = administrator;
  transport = { status: "connected", send: mock(() => true), retry: mock(), canRetry: false, error: null, accessDenied: false };
  useWebSocketMock.mockImplementation((options) => { callbacks = options; return transport; });
  apiJsonMock.mockReset().mockResolvedValue({ servers: [server] });
});

describe("server snapshot recovery", () => {
  it("marks disconnected data stale and refreshes on reconnect even when no event follows", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    expect(view.result.current.stale).toBe(false);
    transport = { ...transport, status: "disconnected", canRetry: true, error: "Connection lost." };
    view.rerender();
    expect(view.result.current.stale).toBe(true);
    expect(view.result.current.servers).toEqual([server]);
    expect(view.result.current.connectionError).toBe("Connection lost.");
    view.result.current.retry();
    expect(transport.retry).toHaveBeenCalledTimes(1);

    const changed = { ...server, displayName: "Renamed while offline" };
    apiJsonMock.mockResolvedValue({ servers: [changed] });
    transport = { ...transport, status: "connected", canRetry: false, error: null, accessDenied: false };
    view.rerender();
    await act(async () => { await callbacks.onOpen?.(); });
    expect(apiJson).toHaveBeenCalledTimes(2);
    expect(view.result.current.servers).toEqual([changed]);
    expect(view.result.current.stale).toBe(false);
  });

  it.each(["connection", "manual"] as const)("immediately supersedes invalidated reads on %s refresh and ignores late responses", async (trigger) => {
    const older = deferred<{ servers: Server[] }>();
    const newer = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
    const view = renderHook(useServers, { wrapper });
    const firstSignal = apiJsonMock.mock.calls[0][2]?.signal;
    act(() => {
      callbacks.onMessage?.(refreshEvent);
      if (trigger === "connection") callbacks.onOpen?.();
      else void view.result.current.refresh();
    });
    expect(firstSignal?.aborted).toBe(true);
    const updated = { ...server, displayName: "Latest state" };
    await act(async () => { newer.resolve({ servers: [updated] }); });
    await act(async () => { older.resolve({ servers: [server] }); });
    expect(view.result.current.servers).toEqual([updated]);
    expect(view.result.current.loading).toBe(false);
    expect(apiJson).toHaveBeenCalledTimes(2);
  });

  it("coalesces a burst into one in-flight read and one trailing refresh without exposing the obsolete result", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    const obsolete = deferred<{ servers: Server[] }>();
    const latest = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(obsolete.promise).mockReturnValueOnce(latest.promise);

    act(() => {
      for (let index = 0; index < 100; index += 1) callbacks.onMessage?.(refreshEvent);
    });
    expect(apiJson).toHaveBeenCalledTimes(2);
    expect(apiJsonMock.mock.calls[1][2]?.signal?.aborted).toBe(false);
    expect(view.result.current.loading).toBe(true);

    await act(async () => { obsolete.resolve({ servers: [{ ...server, displayName: "Obsolete state" }] }); });
    expect(apiJson).toHaveBeenCalledTimes(3);
    expect(view.result.current.servers).toEqual([server]);
    expect(view.result.current.loading).toBe(true);
    const updated = { ...server, displayName: "Latest state" };
    await act(async () => { latest.resolve({ servers: [updated] }); });
    expect(view.result.current.servers).toEqual([updated]);
    expect(view.result.current.loading).toBe(false);

    // A later event after the burst must start a new request normally.
    await act(async () => { callbacks.onMessage?.(refreshEvent); });
    expect(apiJson).toHaveBeenCalledTimes(4);
  });

  it("revalidates again for events during the trailing read, including after a transport failure", async () => {
    const original = deferred<{ servers: Server[] }>();
    const trailing = deferred<{ servers: Server[] }>();
    const latest = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(original.promise)
      .mockReturnValueOnce(trailing.promise)
      .mockReturnValueOnce(latest.promise);
    const view = renderHook(useServers, { wrapper });
    act(() => { callbacks.onMessage?.(refreshEvent); });
    await act(async () => { original.reject(new TypeError("Network unavailable")); });
    expect(apiJson).toHaveBeenCalledTimes(2);
    expect(view.result.current.error).toBeNull();
    act(() => { callbacks.onMessage?.(refreshEvent); });
    await act(async () => { trailing.resolve({ servers: [server] }); });
    expect(apiJson).toHaveBeenCalledTimes(3);
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.loading).toBe(true);
    await act(async () => { latest.resolve({ servers: [] }); });
    expect(view.result.current.loading).toBe(false);
    expect(view.result.current.error).toBeNull();
  });

  it("keeps a stale snapshot for transient failures and replaces it on successful refresh", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.loading).toBe(false));
    apiJsonMock.mockRejectedValueOnce(new TypeError("Network unavailable"));
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([server]);
    expect(view.result.current.stale).toBe(true);
    expect(view.result.current.error).toBe("Network unavailable");
    apiJsonMock.mockResolvedValueOnce({ servers: [] });
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.stale).toBe(false);
    expect(view.result.current.error).toBeNull();
  });

  it.each([401, 403, 200])("clears stored data on authorization or contract failure (HTTP %s)", async (status) => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    apiJsonMock.mockRejectedValueOnce(new ApiRequestError("Access or response rejected", status));
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(view.result.current.stale).toBe(true);
  });

  it("clears an invalidated access rejection before retrying, even if the trailing read fails transiently", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    const rejected = deferred<{ servers: Server[] }>();
    const trailing = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(rejected.promise).mockReturnValueOnce(trailing.promise);
    act(() => {
      callbacks.onMessage?.(refreshEvent);
      callbacks.onMessage?.(refreshEvent);
    });
    await act(async () => { rejected.reject(new ApiRequestError("Access revoked", 403)); });
    expect(apiJson).toHaveBeenCalledTimes(3);
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(view.result.current.loading).toBe(true);
    await act(async () => { trailing.reject(new TypeError("Network unavailable")); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(view.result.current.loading).toBe(false);
    expect(view.result.current.stale).toBe(true);
  });

  it("discards the previous account's state and ignores its late completion", async () => {
    const late = deferred<{ servers: Server[] }>();
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    apiJsonMock.mockReturnValueOnce(late.promise);
    act(() => {
      void view.result.current.refresh();
      callbacks.onMessage?.(refreshEvent);
    });
    currentUser = { id: "viewer", username: "viewer", role: "viewer" };
    apiJsonMock.mockResolvedValueOnce({ servers: [] });
    view.rerender();
    expect(view.result.current.servers).toEqual([]);
    await waitFor(() => expect(view.result.current.loading).toBe(false));
    await act(async () => { late.resolve({ servers: [server] }); });
    expect(view.result.current.servers).toEqual([]);
  });

  it("hides cached rows immediately after a policy close and revalidates over HTTP", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    const obsolete = deferred<{ servers: Server[] }>();
    const revalidation = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(obsolete.promise).mockReturnValueOnce(revalidation.promise);
    act(() => {
      callbacks.onMessage?.(refreshEvent);
      callbacks.onMessage?.(refreshEvent);
    });
    const obsoleteSignal = apiJsonMock.mock.calls[1][2]?.signal;
    transport = { ...transport, status: "disconnected", accessDenied: true, canRetry: false };
    view.rerender();
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(apiJson).toHaveBeenCalledTimes(3);
    expect(obsoleteSignal?.aborted).toBe(true);
    await act(async () => { revalidation.resolve({ servers: [] }); });
    await act(async () => { obsolete.resolve({ servers: [server] }); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.canRetry).toBe(false);
    expect(apiJson).toHaveBeenCalledTimes(3);
  });

  it("aborts pending reads on unmount and ignores malformed events", async () => {
    const late = deferred<{ servers: Server[] }>();
    apiJsonMock.mockReturnValueOnce(late.promise);
    const view = renderHook(useServers, { wrapper });
    const signal = apiJsonMock.mock.calls[0][2]?.signal;
    act(() => { callbacks.onMessage?.("not JSON"); });
    expect(apiJson).toHaveBeenCalledTimes(1);
    act(() => { callbacks.onMessage?.(refreshEvent); });
    view.unmount();
    expect(signal?.aborted).toBe(true);
    await act(async () => { late.resolve({ servers: [server] }); });
    expect(apiJson).toHaveBeenCalledTimes(1);
  });
});
