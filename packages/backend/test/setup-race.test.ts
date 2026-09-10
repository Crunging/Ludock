import assert from "node:assert/strict";
import { serve } from "bun";
import { afterAll as after, describe, it } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
const setupCode = "fixture-race-setup-code-0123456789abcdef";

const [{ createApp }, { SetupWindow }, { listUsers }] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
  import("../src/database.js"),
]);

const server = serve({
  ...createApp({
    frontendDist: false,
    setupWindow: new SetupWindow(Date.now, 60_000, setupCode),
  }),
  hostname: "127.0.0.1",
  port: 0,
});
const baseUrl = server.url.origin;

after(async () => {
  await server.stop(true);
});

describe("concurrent initial setup", () => {
  it("creates at most one administrator when setup requests race", async () => {
    const attempt = (username: string) =>
      fetch(`${baseUrl}/api/v1/auth/setup`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          username,
          password: "race-test-password-1",
          bootstrapCode: setupCode,
        }),
      });

    const responses = await Promise.all([
      attempt("legit-admin"),
      attempt("attacker"),
      attempt("attacker-two"),
    ]);

    const created = responses.filter((response) => response.status === 201);
    assert.equal(created.length, 1, "exactly one setup request should succeed");
    assert.equal(listUsers().length, 1, "only one user should exist");
  });
});
