import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, spyOn, jest } from "bun:test";
import { AuthProvider } from "../src/AuthContext";
import { useAuth } from "../src/auth-context";
import { AUTH_REQUIRED_EVENT } from "../src/api";

const user = { id: "1675bade-833b-4c97-8bab-be48f44b5691", username: "admin", role: "admin" };
const status = (authenticated = false) => Response.json({
  setupRequired: false, setupLocked: false, setupExpiresAt: null,
  setupRemainingMs: null, authenticated, user: authenticated ? user : null,
});
const setupStatus = (setupLocked = false) => Response.json({
  setupRequired: true,
  setupLocked,
  setupExpiresAt: setupLocked ? Date.now() : Date.now() + 60_000,
  setupRemainingMs: setupLocked ? 0 : 60_000,
  authenticated: false,
  user: null,
});
function deferredResponse() {
  let resolve!: (response: Response) => void;
  const promise = new Promise<Response>((complete) => { resolve = complete; });
  return { promise, resolve };
}
function renderAuth() {
  return renderHook(useAuth, { wrapper: AuthProvider });
}

afterEach(() => {
  jest.restoreAllMocks();
  jest.useRealTimers();
});

describe("authentication request ownership", () => {
  it("submits the one-time setup code only with initial setup", async () => {
    const fetch = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(Response.json({
        setupRequired: true,
        setupLocked: false,
        setupExpiresAt: Date.now() + 60_000,
        setupRemainingMs: 60_000,
        authenticated: false,
        user: null,
      }))
      .mockResolvedValueOnce(Response.json({ user }));
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.setup(
        "admin",
        "valid password for tests",
        "fixture-setup-code-0123456789abcdef",
      )).toBeNull();
    });
    expect(fetch.mock.calls[1][0]).toBe("/api/v1/auth/setup");
    expect(JSON.parse(String(fetch.mock.calls[1][1]?.body))).toEqual({
      username: "admin",
      password: "valid password for tests",
      bootstrapCode: "fixture-setup-code-0123456789abcdef",
    });
  });

  it("keeps setup unlocked after a rejected code when fresh status confirms it is open", async () => {
    const fetch = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(setupStatus())
      .mockResolvedValueOnce(Response.json({ error: "Setup code is invalid" }, { status: 403 }))
      .mockResolvedValueOnce(setupStatus());
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.setup(
        "admin",
        "valid password for tests",
        "rejected-fixture-setup-code-123456",
      )).toBe("Setup code is invalid");
    });
    expect(fetch.mock.calls.map(([path]) => path)).toEqual([
      "/api/v1/auth/status",
      "/api/v1/auth/setup",
      "/api/v1/auth/status",
    ]);
    expect(auth.result.current.setupRequired).toBe(true);
    expect(auth.result.current.setupLocked).toBe(false);
  });

  it("locks setup only when fresh status confirms the window expired", async () => {
    spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(setupStatus())
      .mockResolvedValueOnce(Response.json({ error: "Setup is unavailable" }, { status: 403 }))
      .mockResolvedValueOnce(setupStatus(true));
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.setup(
        "admin",
        "valid password for tests",
        "expired-fixture-setup-code-1234567",
      )).toBe("Setup is unavailable");
    });
    expect(auth.result.current.setupRequired).toBe(true);
    expect(auth.result.current.setupLocked).toBe(true);
  });

  it("does not infer expiry when the status probe is unavailable", async () => {
    spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(setupStatus())
      .mockResolvedValueOnce(Response.json({ error: "Setup is unavailable" }, { status: 403 }))
      .mockRejectedValueOnce(new TypeError("Network unavailable"));
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.setup(
        "admin",
        "valid password for tests",
        "unknown-fixture-setup-code-1234567",
      )).toBe("Setup is unavailable");
    });
    expect(auth.result.current.setupRequired).toBe(true);
    expect(auth.result.current.setupLocked).toBe(false);
  });

  it("moves to normal sign-in when another client completes setup during submission", async () => {
    spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(setupStatus())
      .mockResolvedValueOnce(Response.json({ error: "Initial setup has already been completed" }, { status: 409 }))
      .mockResolvedValueOnce(status());
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.setup(
        "admin",
        "valid password for tests",
        "completed-fixture-setup-code-12345",
      )).toBe("Initial setup has already been completed");
    });
    expect(auth.result.current.setupRequired).toBe(false);
    expect(auth.result.current.setupLocked).toBe(false);
  });

  it("does not restore a signed-out user from an older status response", async () => {
    const oldStatus = deferredResponse();
    const fetch = spyOn(globalThis, "fetch")
      .mockImplementationOnce(() => oldStatus.promise)
      .mockResolvedValueOnce(Response.json({ ok: true }));
    const auth = renderAuth();
    await act(() => auth.result.current.logout());
    expect(fetch.mock.calls[0][1]?.signal?.aborted).toBe(true);
    await act(async () => { oldStatus.resolve(status(true)); });
    expect(auth.result.current.authenticated).toBe(false);
    expect(auth.result.current.loading).toBe(false);
  });

  it("only applies the newest status refresh", async () => {
    const oldStatus = deferredResponse();
    spyOn(globalThis, "fetch")
      .mockImplementationOnce(() => oldStatus.promise)
      .mockResolvedValueOnce(status());
    const auth = renderAuth();
    await act(() => auth.result.current.refreshStatus());
    await act(async () => { oldStatus.resolve(status(true)); });
    expect(auth.result.current.authenticated).toBe(false);
    expect(auth.result.current.statusError).toBe(false);
  });

  it("ignores an aborted read's late 401 after a successful sign-in", async () => {
    const oldStatus = deferredResponse();
    spyOn(globalThis, "fetch")
      .mockImplementationOnce(() => oldStatus.promise)
      .mockResolvedValueOnce(Response.json({ user }));
    const auth = renderAuth();
    await act(async () => { expect(await auth.result.current.login("admin", "valid password for tests")).toBeNull(); });
    await act(async () => { oldStatus.resolve(Response.json({ error: "Expired" }, { status: 401 })); });
    expect(auth.result.current.user).toEqual(user);
  });

  it("serializes cookie changes and rejects a response invalidated by session expiration", async () => {
    const signingIn = deferredResponse();
    const fetch = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(status())
      .mockImplementationOnce(() => signingIn.promise);
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    let first!: Promise<string | null>;
    await act(async () => {
      first = auth.result.current.login("admin", "valid password for tests");
      expect(await auth.result.current.setup(
        "another",
        "another valid password",
        "fixture-setup-code-0123456789abcdef",
      )).toContain("still in progress");
      await auth.result.current.logout();
      await auth.result.current.refreshStatus();
    });
    expect(fetch).toHaveBeenCalledTimes(2);
    await act(async () => {
      window.dispatchEvent(new Event(AUTH_REQUIRED_EVENT));
      signingIn.resolve(Response.json({ user }));
      expect(await first).toContain("session changed");
    });
    expect(auth.result.current.authenticated).toBe(false);
  });

  it("preserves a failed sign-in error and allows a subsequent attempt", async () => {
    spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(status())
      .mockResolvedValueOnce(Response.json({ error: "Invalid username or password" }, { status: 401 }))
      .mockResolvedValueOnce(Response.json({ user }));
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    await act(async () => {
      expect(await auth.result.current.login("admin", "incorrect password")).toBe("Invalid username or password");
    });
    expect(auth.result.current.authenticated).toBe(false);
    await act(async () => { expect(await auth.result.current.login("admin", "valid password for tests")).toBeNull(); });
    expect(auth.result.current.user).toEqual(user);
  });

  it("reports an unconfirmed sign-out without rejecting the event handler", async () => {
    spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(status(true))
      .mockRejectedValueOnce(new TypeError("Network unavailable"))
      .mockResolvedValueOnce(status(true));
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.authenticated).toBe(true));
    await act(async () => { await expect(auth.result.current.logout()).resolves.toBeUndefined(); });
    expect(auth.result.current.statusError).toBe(true);
    expect(auth.result.current.authenticated).toBe(false);
    expect(auth.result.current.loading).toBe(false);
    await act(() => auth.result.current.refreshStatus());
    expect(auth.result.current.statusError).toBe(false);
    expect(auth.result.current.authenticated).toBe(true);
  });

  it("cancels retry timers on unmount", async () => {
    jest.useFakeTimers();
    const fetch = spyOn(globalThis, "fetch").mockRejectedValue(new TypeError("Offline"));
    const auth = renderAuth();
    await act(async () => {});
    expect(fetch).toHaveBeenCalledTimes(1);
    auth.unmount();
    await act(async () => { jest.advanceTimersByTime(10_000); });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(jest.getTimerCount()).toBe(0);
  });

  it("aborts pending authentication on unmount", async () => {
    const signingIn = deferredResponse();
    const fetch = spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(status())
      .mockImplementationOnce(() => signingIn.promise);
    const auth = renderAuth();
    await waitFor(() => expect(auth.result.current.loading).toBe(false));
    const request = auth.result.current.login("admin", "valid password for tests");
    auth.unmount();
    expect(fetch.mock.calls[1][1]?.signal?.aborted).toBe(true);
    signingIn.resolve(Response.json({ user }));
    expect(await request).toContain("session changed");
  });
});
