import assert from "node:assert/strict";
import { describe, it } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
process.env.LUDOCK_API_TOKEN = "test-api-token-0123456789abcdef0123";

const {
  SetupWindow,
  authenticateUser,
  authenticateWsRequest,
  createInitialAdmin,
  createSession,
  hashPassword,
  isSetupRequired,
  ludockApiToken,
  verifyPassword,
} = await import("../src/auth.js");

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
    assert.match(encoded, /^\$argon2id\$v=19\$m=65536,t=2,p=1\$/);
    assert.equal(encoded.includes("a-long-test-password"), false);
    assert.equal(await verifyPassword("a-long-test-password", encoded), true);
    assert.equal(await verifyPassword("wrong-password", encoded), false);
  });

  it("fails closed for malformed stored hashes, including an empty decoded key", async () => {
    const salt = Buffer.alloc(16, 1).toString("base64url");
    const key = Buffer.alloc(64, 2).toString("base64url");
    for (const encoded of [
      `scrypt$32768$8$3$${salt}$!`,
      `scrypt$32768$8$3$!$${key}`,
      `scrypt$32769$8$3$${salt}$${key}`,
      `scrypt$32768$8$3$${salt}$${key}$extra`,
      `scrypt$32768$8$3$${salt}$${key.slice(1)}`,
    ]) assert.equal(await verifyPassword("any-password", encoded), false);
  });

  it("locks initial setup when the startup window expires", async () => {
    let now = 1_000;
    const setupWindow = new SetupWindow(() => now, 100);
    now = 1_100;

    assert.deepEqual(setupWindow.getState(), {
      required: true,
      locked: true,
      expiresAt: 1_100,
      remainingMs: 0,
    });
    await assert.rejects(
      createInitialAdmin(
        {
          username: "admin",
          password: "a-long-test-password",
        },
        setupWindow
      ),
      (error: Error & { code?: string }) => {
        assert.equal(error.code, "SETUP_LOCKED");
        return true;
      }
    );
    assert.equal(isSetupRequired(), true);
  });

  it("creates exactly one initial administrator during an open window", async () => {
    const user = await createInitialAdmin(
      {
        username: "admin",
        password: "a-long-test-password",
      },
      new SetupWindow()
    );
    assert.equal(user.role, "admin");
    assert.equal(isSetupRequired(), false);

    await assert.rejects(
      createInitialAdmin(
        {
          username: "other",
          password: "another-long-password",
        },
        new SetupWindow()
      ),
      (error: Error & { code?: string }) => {
        assert.equal(error.code, "SETUP_COMPLETE");
        return true;
      }
    );
  });

  it("authenticates valid credentials", async () => {
    assert.equal(
      (await authenticateUser("ADMIN", "a-long-test-password"))?.username,
      "admin"
    );
    assert.equal(
      await authenticateUser("admin", "definitely-wrong-password"),
      null
    );
    assert.equal(
      await authenticateUser("missing", "definitely-wrong-password"),
      null
    );
  });

  it("requires same-origin WebSocket cookies and header-based API tokens", async () => {
    const user = await authenticateUser("admin", "a-long-test-password");
    assert.ok(user);
    const session = createSession(user, new Request("http://panel.example/", {
      headers: { "User-Agent": "test-agent" },
    }), "127.0.0.1");
    const cookie = `ludock_session=${session.token}`;

    assert.ok(
      authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "panel.example",
          origin: "https://panel.example",
          "x-forwarded-proto": "https",
        })
      )
    );
    assert.ok(
      authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "ludock:3000",
          origin: "https://panel.example",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      "accepts the public origin metadata supplied by Traefik"
    );
    assert.equal(
      authenticateWsRequest(
        websocketRequest({
          cookie,
          host: "panel.example",
          origin: "https://attacker.example",
        })
      ),
      null
    );
    assert.equal(
      authenticateWsRequest(
        websocketRequest(
          { host: "panel.example" },
          "/ws/console/server?token=test-api-token-0123456789abcdef0123"
        )
      ),
      null
    );
    assert.ok(
      authenticateWsRequest(
        websocketRequest({
          authorization: "Bearer test-api-token-0123456789abcdef0123",
          host: "panel.example",
        })
      )
    );
  });

  it("ignores an API token below the strength floor", () => {
    const previous = process.env.LUDOCK_API_TOKEN;
    process.env.LUDOCK_API_TOKEN = "short";
    try {
      assert.equal(ludockApiToken(), "");
      assert.equal(
        authenticateWsRequest(
          websocketRequest({
            authorization: "Bearer short",
            host: "panel.example",
          })
        ),
        null
      );
    } finally {
      process.env.LUDOCK_API_TOKEN = previous;
    }
  });
});
