import assert from "node:assert/strict";
import crypto from "node:crypto";
import { createServer, type Server } from "node:http";
import { syncBuiltinESMExports } from "node:module";
import type { AddressInfo } from "node:net";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import type { Request } from "express";

process.env.LUDOCK_DB_PATH = ":memory:";
const [{ createApp }, auth, database] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
  import("../src/database.js"),
]);
const password = "original-password-123";
const passwordHash = await auth.hashPassword(password);
const resetHash = await auth.hashPassword("reset-password-123");
const originalScrypt = crypto.scrypt;
let server: Server;
let baseUrl: string;
let cookie: string;
const releases: Array<() => void> = [];

/** Hold a real scrypt completion so the test can change authorization at the
 * exact asynchronous boundary without depending on CPU speed or sleeps. */
function pausePasswordWork(callNumber = 1) {
  let started!: () => void;
  const pending = new Promise<void>((resolve) => { started = resolve; });
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  releases.push(release);
  let calls = 0;
  mock.method(crypto, "scrypt", (...args: Parameters<typeof crypto.scrypt>) => {
    calls += 1;
    if (calls !== callNumber) return originalScrypt(...args);
    const callback = args[4];
    started();
    originalScrypt(args[0], args[1], args[2], args[3], (error, derived) => {
      void gate.then(() => callback(error, derived));
    });
  });
  syncBuiltinESMExports();
  return { pending, release };
}

beforeEach(async () => {
  database.closeDatabase();
  for (const id of ["admin", "target"]) database.createUser({
    id, username: id, role: "admin", disabled: false, passwordHash, createdAt: 1,
  });
  const session = auth.createSession(
    { id: "admin", username: "admin", role: "admin" },
    { ip: "127.0.0.1", get: () => "fixture" } as unknown as Request,
  );
  cookie = `ludock_session=${session.token}`;
  server = createServer(createApp({ frontendDist: false }));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

afterEach(async () => {
  releases.splice(0).forEach((release) => release());
  mock.restoreAll();
  syncBuiltinESMExports();
  await new Promise<void>((resolve, reject) =>
    server.close((error) => error ? reject(error) : resolve()),
  );
  database.closeDatabase();
});

const post = (path: string, body: unknown) => fetch(`${baseUrl}${path}`, {
  method: "POST",
  headers: { Cookie: cookie, "Content-Type": "application/json" },
  body: JSON.stringify(body),
});

describe("account changes during password work", () => {
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

  it("does not overwrite a password reset with a delayed legacy hash upgrade", async () => {
    const salt = Buffer.from("legacy-salt-1234");
    const key = crypto.scryptSync(password, salt, 64, { N: 16384, r: 8, p: 1 });
    database.updateUserPassword("admin", `scrypt$16384$8$1$${salt.toString("base64url")}$${key.toString("base64url")}`);
    const hold = pausePasswordWork(2);
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
        const hold = pausePasswordWork();
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
    const hold = pausePasswordWork(2);
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
