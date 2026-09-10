import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const [auth, database, passwords, { accountRoutes }] = await Promise.all([
  import("../src/auth.js"),
  import("../src/database.js"),
  import("../src/password.js"),
  import("../src/routes/accounts.js"),
]);
const setupCode = "fixture-setup-throttle-code-0123456789abcdef";
const wrongCode = "fixture-incorrect-code-0123456789abcdef";
const password = "fixture-correct-password";
const user = {
  id: "80000000-0000-4000-8000-000000000001",
  username: "admin",
  role: "admin" as const,
};
let now = 10_000;

beforeEach(() => {
  database.closeDatabase();
  now = 10_000;
  spyOn(Date, "now").mockImplementation(() => now);
});

afterEach(() => {
  mock.restore();
  database.closeDatabase();
});

function setup(window = new auth.SetupWindow(() => now, 300_000, setupCode)) {
  const handler = accountRoutes(window)["/api/v1/auth/setup"].POST!;
  return (ipAddress: string, bootstrapCode?: string) => handler({
    request: new Request("http://panel.example/api/v1/auth/setup", { method: "POST" }),
    url: new URL("http://panel.example/api/v1/auth/setup"),
    params: {},
    // Deliberately invalid account fields prove code authorization runs first
    // without creating a user or starting any password work.
    body: { username: "x", password: "short", bootstrapCode },
    headers: new Headers(),
    ipAddress,
    user: null,
  });
}

async function exhaustSetupSource(attempt: ReturnType<typeof setup>, source = "192.0.2.10") {
  for (let failure = 0; failure < 20; failure++) {
    const response = await attempt(source, failure % 2 ? wrongCode : undefined);
    assert.equal(response.status, 403);
    assert.deepEqual(await response.json(), { error: "Initial setup authorization failed" });
  }
}

describe("setup-code source throttling", () => {
  it("counts missing and incorrect codes uniformly and rejects blocked sources before KDF work", async () => {
    const hash = spyOn(passwords, "hashPassword");
    const attempt = setup();
    await exhaustSetupSource(attempt);
    for (const code of [undefined, wrongCode, setupCode]) {
      const response = await attempt("192.0.2.10", code);
      assert.equal(response.status, 429);
      assert.deepEqual(await response.json(), { error: "Too many attempts. Try again later." });
    }
    assert.equal((await attempt("198.51.100.20", setupCode)).status, 400);
    assert.equal(hash.mock.calls.length, 0);
    assert.equal(database.countUsers(), 0);
  });

  it("clears failed attempts after successful code authorization", async () => {
    const attempt = setup();
    for (let failure = 0; failure < 19; failure++)
      assert.equal((await attempt("192.0.2.10", wrongCode)).status, 403);
    assert.equal((await attempt("192.0.2.10", setupCode)).status, 400);
    await exhaustSetupSource(attempt);
    assert.equal((await attempt("192.0.2.10", wrongCode)).status, 429);
  });

  it("shares limits across routes for one window, but not a fresh window with the same expiry", async () => {
    const window = new auth.SetupWindow(() => now, 300_000, setupCode);
    await exhaustSetupSource(setup(window));
    assert.equal((await setup(window)("192.0.2.10", setupCode)).status, 429);
    const freshWindow = new auth.SetupWindow(() => now, 300_000, setupCode);
    assert.equal(window.expiresAt, freshWindow.expiresAt);
    assert.equal((await setup(freshWindow)("192.0.2.10", setupCode)).status, 400);

    now = window.expiresAt;
    const expired = await setup(window)("192.0.2.10", setupCode);
    assert.equal(expired.status, 403);
    assert.deepEqual(await expired.json(), {
      error: "Initial setup has expired. Restart the panel to reopen setup.",
    });
  });

  it("reports completed setup instead of an obsolete source lockout", async () => {
    const attempt = setup();
    await exhaustSetupSource(attempt);
    database.createUser({ ...user, disabled: false, passwordHash: "fixture-unused-hash", createdAt: now });
    const completed = await attempt("192.0.2.10", setupCode);
    assert.equal(completed.status, 409);
    assert.deepEqual(await completed.json(), { error: "Initial setup has already been completed" });
  });
});

function login() {
  database.createUser({ ...user, disabled: false, passwordHash: "fixture-unused-hash", createdAt: now });
  const authenticate = spyOn(auth, "authenticateUser").mockImplementation(async (_username, candidate) =>
    candidate === password ? user : null);
  const handler = accountRoutes(
    new auth.SetupWindow(() => now, 300_000, setupCode),
  )["/api/v1/auth/login"].POST!;
  const attempt = (ipAddress: string, candidate = "fixture-incorrect-password", username = "admin") =>
    handler({
      request: new Request("http://panel.example/api/v1/auth/login", { method: "POST" }),
      url: new URL("http://panel.example/api/v1/auth/login"),
      params: {},
      body: { username, password: candidate },
      headers: new Headers(),
      ipAddress,
      user: null,
    });
  return { attempt, authenticate };
}

const accountKey = new Bun.CryptoHasher("sha256")
  .update("login-account-failures:admin").digest("hex");
const accountThrottle = () => database.getLoginThrottle(accountKey, now, 15 * 60_000);

describe("cross-source login cooldown", () => {
  it("slows distributed failures with a capped cooldown that rejected requests cannot extend", async () => {
    const { attempt, authenticate } = login();
    for (let failure = 0; failure < 50; failure++) {
      const prior = accountThrottle();
      now = Math.max(now, prior.blockedUntil);
      const response = await attempt(`192.0.2.${failure + 1}`, undefined, failure % 2 ? " Admin " : "admin");
      assert.equal(response.status, 401);
      const current = accountThrottle();
      assert.equal(current.failures, failure + 1);
      if (failure < 19) {
        assert.equal(current.blockedUntil, 0);
        continue;
      }
      const expectedMs = Math.min(5_000, 250 * 2 ** Math.floor((failure + 1 - 20) / 5));
      assert.equal(current.blockedUntil - now, expectedMs);
      const checksBefore = authenticate.mock.calls.length;
      now = current.blockedUntil - 1;
      const blocked = await attempt("198.51.100.20");
      assert.equal(blocked.status, 429);
      assert.equal(blocked.headers.get("retry-after"), "1");
      assert.deepEqual(accountThrottle(), current);
      assert.equal(authenticate.mock.calls.length, checksBefore);
    }
    const cooldown = accountThrottle();
    now = cooldown.blockedUntil;
    assert.equal((await attempt("198.51.100.21", password)).status, 200);
    assert.deepEqual(accountThrottle(), { failures: 0, blockedUntil: 0 });
    assert.equal((await attempt("198.51.100.22")).status, 401);
    assert.deepEqual(accountThrottle(), { failures: 1, blockedUntil: 0 });
  });

  it("expires the account failure window and preserves another account's counters during pruning", async () => {
    const { attempt } = login();
    const unrelatedKey = "fixture-unrelated-login-counter";
    database.recordLoginFailure(unrelatedKey, now, 15 * 60_000, 20);
    for (let failure = 0; failure < 80; failure++) {
      now = Math.max(now, accountThrottle().blockedUntil);
      assert.equal((await attempt(`192.0.2.${failure + 1}`)).status, 401);
    }
    assert.equal(database.getLoginThrottle(unrelatedKey, now, 15 * 60_000).failures, 1);
    now = 10_000 + 15 * 60_000;
    assert.equal((await attempt("198.51.100.20")).status, 401);
    assert.deepEqual(accountThrottle(), { failures: 1, blockedUntil: 0 });
  });

  it("does not count busy password work as a failed credential", async () => {
    const { attempt, authenticate } = login();
    authenticate.mockImplementation(async () => { throw new passwords.PasswordWorkBusyError(); });
    assert.equal((await attempt("192.0.2.10")).status, 429);
    assert.deepEqual(accountThrottle(), { failures: 0, blockedUntil: 0 });
  });

  it("keeps a five-failure source lockout from denying the owner at another source", async () => {
    const { attempt } = login();
    for (let failure = 0; failure < 5; failure++)
      assert.equal((await attempt("192.0.2.10")).status, 401);
    assert.equal((await attempt("192.0.2.10", password)).status, 429);
    assert.equal((await attempt("198.51.100.20", password)).status, 200);
    assert.deepEqual(accountThrottle(), { failures: 0, blockedUntil: 0 });
  });
});
