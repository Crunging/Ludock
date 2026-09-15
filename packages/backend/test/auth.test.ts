import { rejectedBy } from "./fixtures/errors.js";
import { expect, describe, it } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "test-api-token-0123456789abcdef0123";

const {
  SetupWindow,
  SETUP_WINDOW_MS,
  authenticateUser,
  authenticateRequest,
  authenticateWsRequest,
  createInitialAdmin,
  createSession,
  hashPassword,
  isSetupRequired,
  ludockApiToken,
  verifyPassword,
} = await import("../src/auth.js");
const { createSessionRecord, findSessionUser } = await import("../src/database.js");
const setupCode = "fixture-setup-code-0123456789abcdef";

function websocketRequest(
  headers: HeadersInit,
  url = "/ws/console/server"
): Request {
  const values = new Headers(headers);
  return new Request(`http://${values.get("host") || "panel.example"}${url}`, { headers: values });
}

describe("account authentication", () => {
  it("hashes and verifies passwords without storing plaintext", async () => {
    const encoded = await hashPassword("a-long-test-password");
    expect(encoded).toMatch(/^\$argon2id\$v=19\$m=65536,t=2,p=1\$/);
    expect(encoded.includes("a-long-test-password")).toBe(false);
    expect(await verifyPassword("a-long-test-password", encoded)).toBe(true);
    expect(await verifyPassword("wrong-password", encoded)).toBe(false);
  });


  it("locks initial setup when the startup window expires", async () => {
    let now = 1_000;
    const setupWindow = new SetupWindow(() => now, 100, setupCode);
    now = 1_100;

    expect(setupWindow.getState()).toStrictEqual({
      required: true,
      locked: true,
      expiresAt: 1_100,
      remainingMs: 0,
    });
    await expect(await rejectedBy(createInitialAdmin(
        {
          username: "admin",
          password: "a-long-test-password",
          bootstrapCode: setupCode,
        },
        setupWindow
      ))).toSatisfy((error: Error & { code?: string }) => {
        expect(error.code).toBe("SETUP_LOCKED");
        return true;
      });
    expect(isSetupRequired()).toBe(true);
  });

  it("creates exactly one initial administrator during an open window", async () => {
    const user = await createInitialAdmin(
      {
        username: "admin",
        password: "a-long-test-password",
        bootstrapCode: setupCode,
      },
      new SetupWindow(Date.now, SETUP_WINDOW_MS, setupCode)
    );
    expect(user.role).toBe("admin");
    expect(isSetupRequired()).toBe(false);

    await expect(await rejectedBy(createInitialAdmin(
        {
          username: "other",
          password: "another-long-password",
          bootstrapCode: setupCode,
        },
        new SetupWindow(Date.now, SETUP_WINDOW_MS, setupCode)
      ))).toSatisfy((error: Error & { code?: string }) => {
        expect(error.code).toBe("SETUP_COMPLETE");
        return true;
      });
  });

  it("authenticates valid credentials", async () => {
    expect((await authenticateUser("ADMIN", "a-long-test-password"))?.username).toBe("admin");
    expect(await authenticateUser("admin", "definitely-wrong-password")).toBe(null);
    expect(await authenticateUser("missing", "definitely-wrong-password")).toBe(null);
  });

  it("requires same-origin WebSocket cookies and header-based API tokens", async () => {
    const user = await authenticateUser("admin", "a-long-test-password");
    expect(user).toBeTruthy();
    const session = createSession(user, new Request("http://panel.example/", {
      headers: { "User-Agent": "test-agent" },
    }), "127.0.0.1");
    expect(session.token).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(Uint8Array.fromBase64(session.token, { alphabet: "base64url" }).length).toBe(32);
    expect(findSessionUser(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(session.token))).toHex())?.id, "new sessions retain the existing SHA-256 storage format").toBe(user.id);
    const cookie = `ludock_session=${session.token}`;

    expect(authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "panel.example",
          origin: "https://panel.example",
          "x-forwarded-proto": "https",
        })
      )).toBeTruthy();
    expect(authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "ludock:3000",
          origin: "https://panel.example",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      ), "accepts the public origin metadata supplied by Traefik").toBeTruthy();
    expect(authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "panel.example",
          origin: "https://attacker.example",
        })
      )).toBe(null);
    expect(authenticateWsRequest(
        websocketRequest(
          { host: "panel.example" },
          "/ws/console/server?token=test-api-token-0123456789abcdef0123"
        )
      )).toBe(null);
    expect(authenticateWsRequest(
        websocketRequest({
          authorization: "Bearer test-api-token-0123456789abcdef0123",
          host: "panel.example",
        })
      )).toBeTruthy();
  });

  it("authenticates sessions stored before the native hashing conversion", async () => {
    const user = await authenticateUser("admin", "a-long-test-password");
    expect(user).toBeTruthy();
    const token = new Uint8Array(32).fill(17).toBase64({ alphabet: "base64url", omitPadding: true });
    const now = Date.now();
    createSessionRecord({
      sessionId: crypto.randomUUID(),
      tokenHash: new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(token))).toHex(),
      userId: user.id,
      createdAt: now,
      expiresAt: now + 60_000,
    });
    const session = authenticateRequest(new Request("http://panel.example/", {
      headers: { Cookie: `ludock_session=${token}` },
    }));
    expect(session?.user.id).toBe(user.id);
  });

  it("ignores an API token below the strength floor", () => {
    const previous = process.env.LUDOCK_API_TOKEN;
    process.env.LUDOCK_API_TOKEN = "short";
    try {
      expect(ludockApiToken()).toBe("");
      expect(authenticateWsRequest(
          websocketRequest({
            authorization: "Bearer short",
            host: "panel.example",
          })
        )).toBe(null);
    } finally {
      process.env.LUDOCK_API_TOKEN = previous;
    }
  });
});
