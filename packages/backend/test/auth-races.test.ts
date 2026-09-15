import { serve, type Server } from "bun";
import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
const [{ createApp }, auth, database] = await Promise.all([
  import("../src/app.js"),
  import("../src/auth.js"),
  import("../src/database.js"),
]);
const { accountRoutes } = await import("../src/routes/accounts.js");
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
  it("rejects overlapping password verification for the same login", async () => {
    const hold = pausePasswordWork();
    const first = fetch(`${baseUrl}/api/v1/auth/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: "admin", password: "incorrect-password" }),
    });
    await hold.pending;
    const overlapping = await Promise.all(
      [
        "wrong-one-password",
        "wrong-two-password",
        "wrong-three-password",
        "wrong-four-password",
        password,
      ].map((candidate) => fetch(`${baseUrl}/api/v1/auth/login`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: "admin", password: candidate }),
      })),
    );
    expect(overlapping.map((response) => response.status)).toStrictEqual([
      429, 429, 429, 429, 429,
    ]);
    hold.release();
    expect((await first).status).toBe(401);
  });

  it("scopes a login lockout to the source and account pair", async () => {
    database.deleteUser("admin");
    database.createUser({
      id: crypto.randomUUID(),
      username: "admin",
      role: "admin",
      disabled: false,
      passwordHash,
      createdAt: 1,
    });
    const handler = accountRoutes(
      new auth.SetupWindow(Date.now, 60_000, "fixture-code-0123456789abcdef012345"),
    )["/api/v1/auth/login"].POST!;
    const login = (ipAddress: string, candidate: string) =>
      handler({
        request: new Request("http://panel.example/api/v1/auth/login", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
        }),
        url: new URL("http://panel.example/api/v1/auth/login"),
        params: {},
        body: { username: "admin", password: candidate },
        headers: new Headers(),
        ipAddress,
        user: null,
      });

    for (let attempt = 0; attempt < 5; attempt += 1)
      expect((await login("192.0.2.10", "incorrect-password")).status).toBe(401);
    expect((await login("192.0.2.10", password)).status).toBe(429);
    expect((await login("198.51.100.20", password)).status).toBe(200);
  });

  it("rejects overlapping password changes for the same session", async () => {
    const hold = pausePasswordWork();
    const first = post("/api/v1/account/change-password", {
      currentPassword: "incorrect-password",
      newPassword: "new-password-123",
    });
    await hold.pending;
    const overlapping = await Promise.all(
      [
        "wrong-one-password",
        "wrong-two-password",
        "wrong-three-password",
        "wrong-four-password",
        password,
      ].map((candidate) => post("/api/v1/account/change-password", {
        currentPassword: candidate,
        newPassword: "other-password-123",
      })),
    );
    expect(overlapping.map((response) => response.status)).toStrictEqual([
      429, 429, 429, 429, 429,
    ]);
    hold.release();
    expect((await first).status).toBe(400);
  });

  it("locks current-password guesses to one session rather than the account", async () => {
    const token = cookie.slice("ludock_session=".length);
    const tokenHash = new Bun.CryptoHasher("sha256").update(token).digest("hex");
    const throttleKey = new Bun.CryptoHasher("sha256")
      .update(`password-change-credential:${tokenHash}`)
      .digest("hex");
    for (let attempt = 0; attempt < 5; attempt += 1) {
      database.recordLoginFailure(throttleKey, Date.now(), 15 * 60_000, 5);
    }
    expect((await post("/api/v1/account/change-password", {
      currentPassword: password,
      newPassword: "new-password-123",
    })).status).toBe(429);

    const otherSession = auth.createSession(
      { id: "admin", username: "admin", role: "admin" },
      new Request("http://127.0.0.1/"),
      "127.0.0.1",
    );
    const response = await fetch(`${baseUrl}/api/v1/account/change-password`, {
      method: "POST",
      headers: {
        Cookie: `ludock_session=${otherSession.token}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        currentPassword: password,
        newPassword: "new-password-123",
      }),
    });
    expect(response.status).toBe(200);
  });

  it("upgrades a valid Argon2 cost only after authentication succeeds", async () => {
    const legacy = await Bun.password.hash(password, { algorithm: "argon2id", memoryCost: 8192, timeCost: 1 });
    database.updateUserPassword("admin", legacy);
    expect(await auth.authenticateUser("admin", "wrong-password")).toBe(null);
    expect(database.findUserById("admin")?.passwordHash).toBe(legacy);
    expect((await auth.authenticateUser("admin", password))?.id).toBe("admin");
    const upgraded = database.findUserById("admin")!.passwordHash;
    expect(upgraded).toMatch(/^\$argon2id\$/);
    expect(await auth.verifyPassword(password, upgraded)).toBe(true);
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
        expect(response.status).toBe(change === "demote" ? 403 : 401);
        expect(database.findUserByUsername("new-admin")).toBe(null);
        expect(database.findUserById("target")?.passwordHash).toBe(passwordHash);
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
    expect(response.status).toBe(401);
    expect(response.headers.get("set-cookie")).toBe(null);
    expect(database.findUserById("admin")?.passwordHash).toBe(resetHash);
  });
});
