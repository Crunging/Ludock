import assert from "node:assert/strict";
import { serve } from "bun";
import { afterAll as after, describe, it } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const [{ createApp }, { SetupWindow }] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
]);

let now = 1_000;
const setupCode = "fixture-window-setup-code-0123456789abcdef";
const setupWindow = new SetupWindow(() => now, 100, setupCode);
now = 1_100;
const server = serve({ ...createApp({ frontendDist: false, setupWindow }), hostname: "127.0.0.1", port: 0 });
const baseUrl = server.url.origin;

after(async () => {
  await server.stop(true);
});

describe("initial setup window", () => {
  it("reports a locked setup after the startup deadline", async () => {
    const response = await fetch(`${baseUrl}/api/v1/auth/status`);
    assert.equal(response.status, 200);
    assert.deepEqual(await response.json(), {
      setupRequired: true,
      setupLocked: true,
      setupExpiresAt: 1_100,
      setupRemainingMs: 0,
      authenticated: false,
      user: null,
    });
  });

  it("does not create an administrator after the deadline", async () => {
    const response = await fetch(`${baseUrl}/api/v1/auth/setup`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: "admin",
        password: "locked-test-password",
        bootstrapCode: setupCode,
      }),
    });
    assert.equal(response.status, 403);
    assert.deepEqual(await response.json(), {
      error: "Initial setup has expired. Restart the panel to reopen setup.",
    });
  });
});
