import { describe, expect, it, vi } from "vitest";
import {
  apiFetch,
  AUTH_REQUIRED_EVENT,
  authenticatedWebSocketUrl,
} from "../src/api";

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
