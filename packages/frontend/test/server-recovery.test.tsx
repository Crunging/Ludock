import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { useServers } from "../src/hooks/useServers";
import { useWebSocket } from "../src/hooks/useWebSocket";
import { apiJson, ApiRequestError } from "../src/api";
import { AuthContext, type AuthContextValue, type AuthUser } from "../src/auth-context";
import type { ManagedContainer } from "../src/types";

vi.mock("../src/api", async (original) => ({
  ...(await original<typeof import("../src/api")>()), apiJson: vi.fn(),
}));
vi.mock("../src/hooks/useWebSocket", () => ({ useWebSocket: vi.fn() }));

const server = { id: "server-1", displayName: "World", permissions: ["server.view"] } as ManagedContainer;
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
  transport = { status: "connected", send: vi.fn(() => true), retry: vi.fn(), canRetry: false, error: null, accessDenied: false };
  vi.mocked(useWebSocket).mockImplementation((options) => { callbacks = options; return transport; });
  vi.mocked(apiJson).mockReset().mockResolvedValue({ servers: [server] });
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
    expect(transport.retry).toHaveBeenCalledOnce();

    const changed = { ...server, displayName: "Renamed while offline" };
    vi.mocked(apiJson).mockResolvedValue({ servers: [changed] });
    transport = { ...transport, status: "connected", canRetry: false, error: null, accessDenied: false };
    view.rerender();
    await act(async () => { await callbacks.onOpen?.(); });
    expect(apiJson).toHaveBeenCalledTimes(2);
    expect(view.result.current.servers).toEqual([changed]);
    expect(view.result.current.stale).toBe(false);
  });

  it("aborts superseded reads and ignores a late response even if the transport ignores abort", async () => {
    const older = deferred<{ servers: ManagedContainer[] }>();
    const newer = deferred<{ servers: ManagedContainer[] }>();
    vi.mocked(apiJson).mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
    const view = renderHook(useServers, { wrapper });
    const firstSignal = vi.mocked(apiJson).mock.calls[0][2]?.signal;
    act(() => { callbacks.onOpen?.(); });
    expect(firstSignal?.aborted).toBe(true);
    const updated = { ...server, displayName: "Latest state" };
    await act(async () => { newer.resolve({ servers: [updated] }); });
    await act(async () => { older.resolve({ servers: [server] }); });
    expect(view.result.current.servers).toEqual([updated]);
    expect(view.result.current.loading).toBe(false);
  });

  it("keeps a stale snapshot for transient failures and replaces it on successful refresh", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.loading).toBe(false));
    vi.mocked(apiJson).mockRejectedValueOnce(new TypeError("Network unavailable"));
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([server]);
    expect(view.result.current.stale).toBe(true);
    expect(view.result.current.error).toBe("Network unavailable");
    vi.mocked(apiJson).mockResolvedValueOnce({ servers: [] });
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.stale).toBe(false);
    expect(view.result.current.error).toBeNull();
  });

  it.each([401, 403, 200])("clears stored data on authorization or contract failure (HTTP %s)", async (status) => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    vi.mocked(apiJson).mockRejectedValueOnce(new ApiRequestError("Access or response rejected", status));
    await act(async () => { await view.result.current.refresh(); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(view.result.current.stale).toBe(true);
  });

  it("discards the previous account's state and ignores its late completion", async () => {
    const late = deferred<{ servers: ManagedContainer[] }>();
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    vi.mocked(apiJson).mockReturnValueOnce(late.promise);
    act(() => { void view.result.current.refresh(); });
    currentUser = { id: "viewer", username: "viewer", role: "viewer" };
    vi.mocked(apiJson).mockResolvedValueOnce({ servers: [] });
    view.rerender();
    expect(view.result.current.servers).toEqual([]);
    await waitFor(() => expect(view.result.current.loading).toBe(false));
    await act(async () => { late.resolve({ servers: [server] }); });
    expect(view.result.current.servers).toEqual([]);
  });

  it("hides cached rows immediately after a policy close and revalidates over HTTP", async () => {
    const view = renderHook(useServers, { wrapper });
    await waitFor(() => expect(view.result.current.servers).toEqual([server]));
    const revalidation = deferred<{ servers: ManagedContainer[] }>();
    vi.mocked(apiJson).mockReturnValueOnce(revalidation.promise);
    transport = { ...transport, status: "disconnected", accessDenied: true, canRetry: false };
    view.rerender();
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.lastUpdated).toBeNull();
    expect(apiJson).toHaveBeenCalledTimes(2);
    await act(async () => { revalidation.resolve({ servers: [] }); });
    expect(view.result.current.servers).toEqual([]);
    expect(view.result.current.canRetry).toBe(false);
  });

  it("aborts pending reads on unmount and ignores malformed events", async () => {
    const late = deferred<{ servers: ManagedContainer[] }>();
    vi.mocked(apiJson).mockReturnValueOnce(late.promise);
    const view = renderHook(useServers, { wrapper });
    const signal = vi.mocked(apiJson).mock.calls[0][2]?.signal;
    act(() => { callbacks.onMessage?.("not JSON"); });
    expect(apiJson).toHaveBeenCalledOnce();
    view.unmount();
    expect(signal?.aborted).toBe(true);
    await act(async () => { late.resolve({ servers: [server] }); });
    expect(apiJson).toHaveBeenCalledOnce();
  });
});
