import { afterEach, describe, expect, it, vi } from "vitest";
import {
  apiFetch,
  apiJson,
  apiResponse,
  AUTH_REQUIRED_EVENT,
  authenticatedWebSocketUrl,
} from "../src/api";

import {
  authStatusSchema,
  authUserResponseSchema,
  okResponseSchema,
  operationResponseSchema,
  serversResponseSchema,
} from "@ludock/shared";

afterEach(() => vi.restoreAllMocks());

describe("authenticated requests", () => {
  it("uses same-origin cookies and notifies the auth provider on expiration", async () => {
    const request = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response("{}", { status: 401 }));
    const expired = vi.fn();
    window.addEventListener(AUTH_REQUIRED_EVENT, expired);
    await apiFetch("/api/v1/servers");
    expect(request).toHaveBeenCalledWith(
      "/api/v1/servers",
      expect.objectContaining({ credentials: "same-origin" }),
    );
    expect(expired).toHaveBeenCalledOnce();
    window.removeEventListener(AUTH_REQUIRED_EVENT, expired);
  });
  it("constructs a versioned websocket without credentials in the URL", () => {
    const url = new URL(authenticatedWebSocketUrl("/ws/v1/events"));
    expect(url.pathname).toBe("/ws/v1/events");
    expect(url.search).toBe("");
    expect(url.username).toBe("");
  });
});

describe("API response contracts", () => {
  it("validates successful data and strips fields outside the shared contract", async () => {
    const request = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      Response.json({
        user: {
          id: "1675bade-833b-4c97-8bab-be48f44b5691",
          username: "admin",
          role: "admin",
          passwordHash: "private",
        },
        sessionToken: "private",
      }),
    );
    const result = await apiJson("/auth/login", authUserResponseSchema, {
      method: "POST",
    });
    expect(result).toEqual({
      user: {
        id: "1675bade-833b-4c97-8bab-be48f44b5691",
        username: "admin",
        role: "admin",
      },
    });
    expect(request).toHaveBeenCalledWith(
      "/api/v1/auth/login",
      expect.objectContaining({ method: "POST", credentials: "same-origin" }),
    );
  });

  it("rejects malformed success bodies without exposing response contents", async () => {
    for (const body of [
      {
        user: {
          id: "1675bade-833b-4c97-8bab-be48f44b5691",
          username: "admin",
          role: "secret-invalid-role",
        },
      },
      { error: "unexpected-sensitive-body" },
      null,
    ]) {
      await expect(
        apiResponse(Response.json(body), authUserResponseSchema),
      ).rejects.toThrow("The server returned an invalid response.");
    }
    await expect(
      apiResponse(
        new Response("<html>sensitive proxy error</html>"),
        okResponseSchema,
      ),
    ).rejects.toThrow("The server returned an invalid response.");
  });

  it("requires the complete expected response instead of treating absent collections as empty", async () => {
    await expect(
      apiResponse(Response.json({}), serversResponseSchema),
    ).rejects.toThrow("invalid response");
    await expect(
      apiResponse(
        Response.json({ operation: { status: "unknown" } }),
        operationResponseSchema,
      ),
    ).rejects.toThrow("invalid response");
    await expect(
      apiResponse(Response.json({ ok: false }), okResponseSchema),
    ).rejects.toThrow("invalid response");
  });

  it("rejects contradictory authentication state", async () => {
    await expect(
      apiResponse(
        Response.json({
          setupRequired: false,
          setupLocked: false,
          setupExpiresAt: null,
          setupRemainingMs: null,
          authenticated: true,
          user: null,
        }),
        authStatusSchema,
      ),
    ).rejects.toThrow("invalid response");
  });

  it("uses only a validated public error message with the original HTTP status", async () => {
    await expect(
      apiResponse(
        Response.json(
          { error: "Access revoked", code: "ACCESS_REVOKED" },
          { status: 403 },
        ),
        okResponseSchema,
      ),
    ).rejects.toMatchObject({ message: "Access revoked", status: 403 });
    await expect(
      apiResponse(
        Response.json({ error: { password: "private" } }, { status: 500 }),
        okResponseSchema,
      ),
    ).rejects.toMatchObject({
      message: "Request failed (HTTP 500)",
      status: 500,
    });
    await expect(
      apiResponse(new Response("untrusted", { status: 502 }), okResponseSchema),
    ).rejects.toMatchObject({
      message: "Request failed (HTTP 502)",
      status: 502,
    });
  });
});
