import type { Server } from "@ludock/shared";
import { beforeEach, describe, expect, it, mock } from "bun:test";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { useServers } from "../src/hooks/useServers";
import { useWebSocket } from "../src/hooks/useWebSocket";
import { apiJson } from "../src/api";
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
  const promise = new Promise<T>((complete) => { resolve = complete; });
  return { promise, resolve };
}

beforeEach(() => {
  currentUser = administrator;
  transport = { status: "connected", send: mock(() => true), retry: mock(), canRetry: false, error: null, accessDenied: false };
  useWebSocketMock.mockImplementation((options) => { callbacks = options; return transport; });
  apiJsonMock.mockReset().mockResolvedValue({ servers: [server] });
});

describe("server snapshot recovery", () => {
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
});
