import assert from "node:assert/strict";
import crypto from "node:crypto";
import { serve, type Server } from "bun";
import { afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
const [{ createApp }, auth, database] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
  import("../src/database.js"),
]);
const password = "original-password-123";
const passwordHash = await auth.hashPassword(password);
const resetHash = await auth.hashPassword("reset-password-123");
const passwords = await import("../src/password.js");
let server: Server<unknown>;
let baseUrl: string;
let cookie: string;
const releases: Array<() => void> = [];

/** Hold real password work so authorization changes happen at the exact
 * asynchronous boundary without depending on CPU speed or sleeps. */
function pausePasswordWork(operation: "hashPassword" | "verifyPassword" = "verifyPassword") {
  let started!: () => void;
  const pending = new Promise<void>((resolve) => { started = resolve; });
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  releases.push(release);
  if (operation === "hashPassword") {
    const original = passwords.hashPassword;
    spyOn(passwords, "hashPassword").mockImplementation(async (password) => {
      const result = original(password);
      started();
      const hash = await result;
      await gate;
      return hash;
    });
  } else {
    const original = passwords.verifyPassword;
    spyOn(passwords, "verifyPassword").mockImplementation(async (password, encoded) => {
      const result = original(password, encoded);
      started();
      const valid = await result;
      await gate;
      return valid;
    });
  }
  return { pending, release };
}

beforeEach(async () => {
  database.closeDatabase();
  for (const id of ["admin", "target"]) database.createUser({
    id, username: id, role: "admin", disabled: false, passwordHash, createdAt: 1,
  });
  const session = auth.createSession(
    { id: "admin", username: "admin", role: "admin" },
    new Request("http://127.0.0.1/", { headers: { "User-Agent": "fixture" } }),
    "127.0.0.1",
  );
  cookie = `ludock_session=${session.token}`;
  server = serve({ ...createApp({ frontendDist: false }), hostname: "127.0.0.1", port: 0 });
  baseUrl = server.url.origin;
});

afterEach(async () => {
  releases.splice(0).forEach((release) => release());
  mock.restore();
  await server.stop(true);
  database.closeDatabase();
});

const post = (path: string, body: unknown) => fetch(`${baseUrl}${path}`, {
  method: "POST",
  headers: { Cookie: cookie, "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

describe("account changes during password work", () => {
  it("upgrades a valid legacy password only after authentication succeeds", async () => {
    const salt = Buffer.alloc(16, 1);
    const key = crypto.scryptSync(password, salt, 64, { N: 16384, r: 8, p: 1 });
    const legacy = `scrypt$16384$8$1$${salt.toString("base64url")}$${key.toString("base64url")}`;
    database.updateUserPassword("admin", legacy);
    assert.equal(await auth.authenticateUser("admin", "wrong-password"), null);
    assert.equal(database.findUserById("admin")?.passwordHash, legacy);
    assert.equal((await auth.authenticateUser("admin", password))?.id, "admin");
    const upgraded = database.findUserById("admin")!.passwordHash;
    assert.match(upgraded, /^\$argon2id\$/);
    assert.equal(await auth.verifyPassword(password, upgraded), true);
  });

  for (const change of ["reset", "disable", "delete"] as const) {
    it(`rejects a login when the account is changed by ${change} during verification`, async () => {
      const hold = pausePasswordWork();
      const result = auth.authenticateUser("admin", password);
      await hold.pending;
      if (change === "reset") database.updateUserPassword("admin", resetHash);
      else if (change === "disable") database.updateUserAccess("admin", "admin", true);
      else database.deleteUser("admin");
      hold.release();
      assert.equal(await result, null);
    });
  }

  it("does not overwrite a password reset during a delayed hash-cost upgrade", async () => {
    const salt = Buffer.alloc(16, 1);
    const key = crypto.scryptSync(password, salt, 64, { N: 16384, r: 8, p: 1 });
    database.updateUserPassword("admin", `scrypt$16384$8$1$${salt.toString("base64url")}$${key.toString("base64url")}`);
    const hold = pausePasswordWork("hashPassword");
    const result = auth.authenticateUser("admin", password);
    await hold.pending;
    database.updateUserPassword("admin", resetHash);
    hold.release();
    assert.equal(await result, null);
    assert.equal(database.findUserById("admin")?.passwordHash, resetHash);
  });

  it("returns the current role when it changes during login", async () => {
    const hold = pausePasswordWork();
    const result = auth.authenticateUser("admin", password);
    await hold.pending;
    database.updateUserAccess("admin", "viewer", false);
    hold.release();
    assert.equal((await result)?.role, "viewer");
  });

  for (const operation of ["create", "reset"] as const) {
    for (const change of ["demote", "revoke-session"] as const) {
      it(`rejects an administrator ${operation} after ${change} during hashing`, async () => {
        const hold = pausePasswordWork("hashPassword");
        const result = operation === "create"
          ? post("/api/v1/users", { username: "new-admin", password: "new-password-123", role: "admin" })
          : post("/api/v1/users/target/reset-password", { password: "new-password-123" });
        await hold.pending;
        if (change === "demote") database.updateUserAccess("admin", "viewer", false);
        else database.deleteUserSessions("admin");
        hold.release();
        const response = await result;
        assert.equal(response.status, change === "demote" ? 403 : 401);
        assert.equal(database.findUserByUsername("new-admin"), null);
        assert.equal(database.findUserById("target")?.passwordHash, passwordHash);
      });
    }
  }

  it("does not restore a revoked session or overwrite a reset during password change", async () => {
    const hold = pausePasswordWork("hashPassword");
    const result = post("/api/v1/account/change-password", {
      currentPassword: password, newPassword: "new-password-123",
    });
    await hold.pending;
    database.updateUserPassword("admin", resetHash);
    hold.release();
    const response = await result;
    assert.equal(response.status, 401);
    assert.equal(response.headers.get("set-cookie"), null);
    assert.equal(database.findUserById("admin")?.passwordHash, resetHash);
  });
});
