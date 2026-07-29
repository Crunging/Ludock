import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { after, before, describe, it } from "node:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const [{ createApp }, { listUsers }] = await Promise.all([
  import("../src/app.js"),
  import("../src/database.js"),
]);

const server = createServer(createApp({ frontendDist: false }));
let baseUrl = "";

before(async () => {
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

after(async () => {
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve()))
  );
});

describe("concurrent initial setup", () => {
  it("creates at most one administrator when setup requests race", async () => {
    const attempt = (username: string) =>
      fetch(`${baseUrl}/api/auth/setup`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password: "race-test-password-1" }),
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
