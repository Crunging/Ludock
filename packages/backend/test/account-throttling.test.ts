import { expect, afterEach, beforeEach, describe, it, mock, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const [auth, database, passwords, { accountRoutes }, { createApp }] = await Promise.all([
  import("../src/auth.js"),
  import("../src/database.js"),
  import("../src/password.js"),
  import("../src/routes/accounts.js"),
  import("../src/app.js"),
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
    expect(response.status).toBe(403);
    expect(await response.json()).toStrictEqual({ error: "Initial setup authorization failed" });
  }
}

describe("setup-code source throttling", () => {
  it("counts missing and incorrect codes uniformly and rejects blocked sources before KDF work", async () => {
    const hash = spyOn(passwords, "hashPassword");
    const attempt = setup();
    await exhaustSetupSource(attempt);
    for (const code of [undefined, wrongCode, setupCode]) {
      const response = await attempt("192.0.2.10", code);
      expect(response.status).toBe(429);
      expect(await response.json()).toStrictEqual({ error: "Too many attempts. Try again later." });
    }
    expect((await attempt("198.51.100.20", setupCode)).status).toBe(400);
    expect(hash.mock.calls.length).toBe(0);
    expect(database.countUsers()).toBe(0);
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
  it("keeps another client's proxy login available after a source lockout", async () => {
    login();
    const previous = process.env.LUDOCK_TRUSTED_PROXIES;
    process.env.LUDOCK_TRUSTED_PROXIES = "172.18.0.2";
    try {
      const app = createApp({ frontendDist: false });
      const peer = { requestIP: () => ({ address: "172.18.0.2", family: "IPv4" as const, port: 12345 }), timeout: () => {} };
      const attempt = async (client: string, username: string, candidate: string) => {
        const response = await app.fetch(new Request("http://panel.example/api/v1/auth/login", {
          method: "POST",
          headers: { "content-type": "application/json", "x-forwarded-for": client },
          body: JSON.stringify({ username, password: candidate }),
        }), peer);
        await response.body?.cancel();
        return response.status;
      };
      for (let failure = 0; failure < 20; failure++)
        expect(await attempt("198.51.100.10", `missing${Math.floor(failure / 5)}`, "fixture-incorrect-password")).toBe(401);
      expect(await attempt("198.51.100.10", "admin", password)).toBe(429);
      expect(await attempt("203.0.113.20", "admin", password)).toBe(200);
    } finally {
      if (previous === undefined) delete process.env.LUDOCK_TRUSTED_PROXIES;
      else process.env.LUDOCK_TRUSTED_PROXIES = previous;
    }
  });

  it("slows distributed failures with a capped cooldown that rejected requests cannot extend", async () => {
    const { attempt, authenticate } = login();
    for (let failure = 0; failure < 50; failure++) {
      const prior = accountThrottle();
      now = Math.max(now, prior.blockedUntil);
      const response = await attempt(`192.0.2.${failure + 1}`, undefined, failure % 2 ? " Admin " : "admin");
      expect(response.status).toBe(401);
      const current = accountThrottle();
      expect(current.failures).toBe(failure + 1);
      if (failure < 19) {
        expect(current.blockedUntil).toBe(0);
        continue;
      }
      const expectedMs = Math.min(5_000, 250 * 2 ** Math.floor((failure + 1 - 20) / 5));
      expect(current.blockedUntil - now).toBe(expectedMs);
      const checksBefore = authenticate.mock.calls.length;
      now = current.blockedUntil - 1;
      const blocked = await attempt("198.51.100.20");
      expect(blocked.status).toBe(429);
      expect(blocked.headers.get("retry-after")).toBe("1");
      expect(accountThrottle()).toStrictEqual(current);
      expect(authenticate.mock.calls.length).toBe(checksBefore);
    }
    const cooldown = accountThrottle();
    now = cooldown.blockedUntil;
    expect((await attempt("198.51.100.21", password)).status).toBe(200);
    expect(accountThrottle()).toStrictEqual({ failures: 0, blockedUntil: 0 });
    expect((await attempt("198.51.100.22")).status).toBe(401);
    expect(accountThrottle()).toStrictEqual({ failures: 1, blockedUntil: 0 });
  });
});
