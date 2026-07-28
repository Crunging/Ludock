import assert from "node:assert/strict";
import type { IncomingMessage } from "node:http";
import { describe, it } from "node:test";
import type { Request } from "express";

process.env.PANEL_DB_PATH = ":memory:";
process.env.PANEL_API_TOKEN = "test-api-token";

const {
  SetupWindow,
  authenticateUser,
  authenticateWsRequest,
  createInitialAdmin,
  createSession,
  hashPassword,
  isSetupRequired,
  verifyPassword,
} = await import("../src/auth.js");

function websocketRequest(
  headers: IncomingMessage["headers"],
  url = "/ws/console/server"
): IncomingMessage {
  return {
    headers,
    socket: {},
    url,
  } as unknown as IncomingMessage;
}

describe("account authentication", () => {
  it("hashes and verifies passwords without storing plaintext", async () => {
    const encoded = await hashPassword("a-long-test-password");
    assert.match(encoded, /^scrypt\$32768\$8\$3\$/);
    assert.equal(encoded.includes("a-long-test-password"), false);
    assert.equal(await verifyPassword("a-long-test-password", encoded), true);
    assert.equal(await verifyPassword("wrong-password", encoded), false);
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
    const session = createSession(user, {
      ip: "127.0.0.1",
      get: () => "test-agent",
    } as unknown as Request);
    const cookie = `dgm_session=${session.token}`;

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
          "/ws/console/server?token=test-api-token"
        )
      ),
      null
    );
    assert.ok(
      authenticateWsRequest(
        websocketRequest({
          authorization: "Bearer test-api-token",
          host: "panel.example",
        })
      )
    );
  });
});
