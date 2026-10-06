import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { createApp } from "../src/app.js";
import { createSession } from "../src/auth.js";
import { closeDatabase, createUser } from "../src/database.js";
import { listAuditHistory } from "../src/history.js";
import { connectionHost, setSetting } from "../src/settings.js";
import type { HttpServer } from "../src/routes/request.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const roles = ["admin", "operator", "viewer"] as const;
type Role = typeof roles[number];
let cookies: Record<Role, string>;
let app: ReturnType<typeof createApp>;
const server: HttpServer = { requestIP: () => null, timeout: () => {} };

beforeEach(() => {
  closeDatabase();
  cookies = { admin: "", operator: "", viewer: "" };
  for (const role of roles) {
    const user = { id: crypto.randomUUID(), username: role, role };
    createUser({ ...user, disabled: false, passwordHash: "unused-fixture", createdAt: 1 });
    cookies[role] = `ludock_session=${createSession(user, new Request("http://localhost")).token}`;
  }
  app = createApp({ frontendDist: false });
});

afterEach(() => closeDatabase());

function request(method: string, role: Role, body?: unknown) {
  return app.fetch(new Request("http://localhost/api/v1/settings/connection", {
    method,
    headers: {
      Cookie: cookies[role],
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  }), server);
}

describe("connection address setting", () => {
  it("lets administrators save, normalize, and clear the address players use", async () => {
    expect(await (await request("GET", "admin")).json()).toStrictEqual({ host: null });

    const saved = await request("PUT", "admin", { host: "  Play.Example.com " });
    expect(saved.status).toBe(200);
    expect(await saved.json()).toStrictEqual({ host: "play.example.com" });
    expect(connectionHost()).toBe("play.example.com");
    expect(listAuditHistory({ limit: 10 }).entries.some((entry) => entry.action === "settings.connection_updated")).toBe(true);

    expect((await request("PUT", "admin", { host: "2001:db8::1" })).status).toBe(200);
    expect(connectionHost()).toBe("2001:db8::1");

    expect(await (await request("PUT", "admin", { host: null })).json()).toStrictEqual({ host: null });
    expect(connectionHost()).toBeNull();
  });

  it("rejects URLs, ports, and other values that are not a host name or IP address", async () => {
    for (const host of ["https://play.example.com", "play.example.com:25565", "two words", ""]) {
      expect((await request("PUT", "admin", { host })).status, host).toBe(400);
    }
    expect(connectionHost()).toBeNull();
  });

  it("treats an invalid stored value as unset", () => {
    setSetting("connection.host", "not a host");
    expect(connectionHost()).toBeNull();
  });

  it("is limited to administrators", async () => {
    for (const role of ["operator", "viewer"] as const) {
      expect((await request("GET", role)).status).toBe(403);
      expect((await request("PUT", role, { host: "play.example.com" })).status).toBe(403);
    }
    expect(connectionHost()).toBeNull();
  });
});
