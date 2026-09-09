import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, describe, it } from "node:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const [{ createApp }, { SetupWindow }] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
]);

let now = 1_000;
const setupWindow = new SetupWindow(() => now, 100);
now = 1_100;
const server = createServer(createApp({ frontendDist: false, setupWindow }));
let baseUrl = "";

before(async () => {
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

after(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
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
      }),
    });
    assert.equal(response.status, 403);
    assert.deepEqual(await response.json(), {
      error: "Initial setup has expired. Restart the panel to reopen setup.",
    });
  });
});
