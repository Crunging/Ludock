import { serve, type Server } from "bun";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
const [{ createApp }, auth, database, { hashPassword }] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
  import("../src/database.js"),
  import("../src/password.js"),
]);
const password = "original-password-123";
const passwordHash = await hashPassword(password);
const resetHash = await hashPassword("reset-password-123");
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

beforeEach(() => {
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
  for (const change of ["reset", "disable"] as const) {
    it(`rejects a login when the account is changed by ${change} during verification`, async () => {
      const hold = pausePasswordWork();
      const result = auth.authenticateUser("admin", password);
      await hold.pending;
      if (change === "reset") database.updateUserPassword("admin", resetHash);
      else database.updateUserAccess("admin", "admin", true);
      hold.release();
      expect(await result).toBe(null);
    });
  }

  it("does not overwrite a password reset during a delayed hash-cost upgrade", async () => {
    database.updateUserPassword("admin", await Bun.password.hash(password, { algorithm: "argon2id", memoryCost: 8192, timeCost: 1 }));
    const hold = pausePasswordWork("hashPassword");
    const result = auth.authenticateUser("admin", password);
    await hold.pending;
    database.updateUserPassword("admin", resetHash);
    hold.release();
    expect(await result).toBe(null);
    expect(database.findUserById("admin")?.passwordHash).toBe(resetHash);
  });

  it("returns the current role when it changes during login", async () => {
    const hold = pausePasswordWork();
    const result = auth.authenticateUser("admin", password);
    await hold.pending;
    database.updateUserAccess("admin", "viewer", false);
    hold.release();
    expect((await result)?.role).toBe("viewer");
  });

  for (const [operation, change] of [["create", "demote"], ["reset", "revoke-session"]] as const) {
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
      expect(response.status).toBe(change === "demote" ? 403 : 401);
      expect(database.findUserByUsername("new-admin")).toBe(null);
      expect(database.findUserById("target")?.passwordHash).toBe(passwordHash);
    });
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
    expect(response.status).toBe(401);
    expect(response.headers.get("set-cookie")).toBe(null);
    expect(database.findUserById("admin")?.passwordHash).toBe(resetHash);
  });
});
