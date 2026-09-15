import { test as base, expect, type Route, type WebSocketRoute } from "@playwright/test";
import {
  SERVER_CAPABILITIES,
  apiErrorSchema,
  authStatusSchema,
  authUserResponseSchema,
  attentionResponseSchema,
  availabilityResponseSchema,
  backupsResponseSchema,
  backupStorageResponseSchema,
  backupPreflightResponseSchema,
  createDirectoryRequestSchema,
  credentialsRequestSchema,
  fileListingSchema,
  fileLocationSchema,
  okResponseSchema,
  operationsResponseSchema,
  renameFileRequestSchema,
  schedulesResponseSchema,
  serverResponseSchema,
  serverSchema,
  serversResponseSchema,
  updateCapabilityResponseSchema,
  type AuthUser,
  type FileEntry,
  type ResponseSchema,
  type Server,
} from "@ludock/shared";

export const ADMIN: AuthUser = {
  id: "11111111-1111-4111-8111-111111111111",
  username: "Crunging",
  role: "admin",
};
export const RUNNING_ID = "53bfe195-b78c-4c14-aebb-1bd09384f33b";
export const STOPPED_ID = "29701921-ccf4-4b62-b0a1-bf47f7aec285";
export const RUNNING_NAME = "Friends’ survival world";
const CREATED_AT = Date.UTC(2026, 0, 1);

export function makeServers(): Server[] {
  const running = serverSchema.parse({
    id: RUNNING_ID,
    shortId: "abcd123",
    name: "minecraft",
    displayName: RUNNING_NAME,
    image: "itzg/minecraft-server:java21",
    state: "running",
    status: "Up 2 days",
    gameType: "minecraft",
    gameConsole: {
      id: "minecraft-rcon",
      name: "Minecraft RCON",
      commandPlaceholder: "help",
    },
    fileRoots: [
      { id: "data", name: "Data", path: "/data" },
      { id: "config", name: "Configuration", path: "/config" },
    ],
    ports: [{ private: 25565, public: 25565, type: "tcp" }],
    created: CREATED_AT,
    labels: {},
    latestBackup: null,
    bindingStatus: "active",
    permissions: [...SERVER_CAPABILITIES],
  });
  return [
    running,
    serverSchema.parse({
      ...running,
      id: STOPPED_ID,
      name: "factorio",
      displayName: "Factorio weekend",
      image: "factoriotools/factorio:stable",
      gameType: "factorio",
      state: "exited",
      status: "Exited (0)",
      gameConsole: null,
      ports: [{ private: 34197, public: 34197, type: "udp" }],
    }),
  ];
}

export interface ApiRequest {
  method: string;
  path: string;
  query: Record<string, string>;
  body: unknown;
}

interface FixtureSocket {
  path: string;
  route: WebSocketRoute;
  messages: string[];
}

export interface AppFixture {
  user: AuthUser | null;
  servers: Server[];
  requests: ApiRequest[];
  sockets: FixtureSocket[];
  files: Map<string, FileEntry[]>;
  open: (path?: string) => Promise<void>;
}

async function respond<T>(route: Route, schema: ResponseSchema<T>, body: unknown, status = 200) {
  await route.fulfill({ status, json: schema.parse(body) });
}

export const test = base.extend<{ app: AppFixture }>({
  app: async ({ page, baseURL }, runTest) => {
    const unexpected: string[] = [];
    const pageErrors: string[] = [];
    const state: AppFixture = {
      user: { ...ADMIN },
      servers: makeServers(),
      requests: [],
      sockets: [],
      files: new Map([
        ["data:", [
          { name: "world", type: "directory", size: 0, modifiedAt: CREATED_AT },
          { name: "server.properties", type: "file", size: 342, modifiedAt: CREATED_AT },
        ]],
        ["data:world", [
          { name: "level.dat", type: "file", size: 4_096, modifiedAt: CREATED_AT },
        ]],
        ["config:", [
          { name: "settings.yml", type: "file", size: 120, modifiedAt: CREATED_AT },
        ]],
      ]),
      open: async (path = "/") => { await page.goto(path); },
    };
    page.on("pageerror", (error) => pageErrors.push(error.message));
    // Block external resources as well as accidental, unmocked API traffic.
    await page.route("**/*", async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== baseURL) {
        unexpected.push(`External request: ${request.method()} ${url.origin}${url.pathname}`);
        await route.abort();
        return;
      }
      if (!url.pathname.startsWith("/api/")) {
        await route.continue();
        return;
      }
      const method = request.method();
      const path = url.pathname.replace(/^\/api\/v1/, "");
      const query = Object.fromEntries(url.searchParams);
      const body: unknown = request.postData()
        ? request.headers()["content-type"]?.includes("application/json")
          ? request.postDataJSON()
          : request.postData()
        : undefined;
      state.requests.push({ method, path, query, body });
      if (path === "/auth/status" && method === "GET") {
        await respond(route, authStatusSchema, {
          setupRequired: false,
          setupLocked: false,
          setupExpiresAt: null,
          setupRemainingMs: null,
          authenticated: Boolean(state.user),
          user: state.user,
        });
        return;
      }
      if (path === "/auth/logout" && method === "POST") {
        state.user = null;
        await respond(route, okResponseSchema, { ok: true });
        return;
      }
      if (path === "/auth/login" && method === "POST") {
        const credentials = credentialsRequestSchema.parse(body);
        state.user = {
          ...ADMIN,
          id: credentials.username === ADMIN.username
            ? ADMIN.id
            : "44444444-4444-4444-8444-444444444444",
          username: credentials.username,
        };
        await respond(route, authUserResponseSchema, { user: state.user });
        return;
      }
      if (!state.user) {
        await respond(route, apiErrorSchema, { error: "Authentication required" }, 401);
        return;
      }
      if (path === "/servers" && method === "GET") {
        await respond(route, serversResponseSchema, { servers: state.servers });
        return;
      }
      if (path === "/settings/backups/status" && method === "GET") {
        await respond(route, backupStorageResponseSchema, { storage: {
          configured: false, archiveBytes: 0, maxBytes: null, reserveBytes: null, availableBytes: null,
          issues: [{ code: "not_configured", message: "Save a backup destination in Settings." }],
        } });
        return;
      }
      if (path === "/attention" && method === "GET") {
        await respond(route, attentionResponseSchema, { items: [], discoveryUnavailable: false });
        return;
      }
      const match = path.match(/^\/servers\/([^/]+)(\/.*)?$/);
      const server = state.servers.find((item) => item.id === match?.[1]);
      const resource = match?.[2] || "";
      if (match && !server) {
        await respond(route, apiErrorSchema, { error: "Server unavailable" }, 404);
        return;
      }
      if (server && method === "GET") {
        if (!resource) {
          await respond(route, serverResponseSchema, { server, stats: null });
          return;
        }
        if (resource === "/operations") {
          await respond(route, operationsResponseSchema, { operations: [] });
          return;
        }
        if (resource === "/backups") {
          await respond(route, backupsResponseSchema, { backups: [] });
          return;
        }
        if (resource === "/backups/preflight") {
          await respond(route, backupPreflightResponseSchema, {
            preflight: { ready: true, checkedAt: Date.now(), issues: [] },
          });
          return;
        }
        if (resource === "/schedules") {
          await respond(route, schedulesResponseSchema, { schedules: [] });
          return;
        }
        if (resource === "/update-capability") {
          await respond(route, updateCapabilityResponseSchema, {
            capability: {
              available: true,
              actionLabel: "Update server",
              manager: "compose",
              projectName: "fixture-games",
              serviceName: server.name,
              image: server.image,
            },
          });
          return;
        }
        if (resource === "/availability") {
          await respond(route, availabilityResponseSchema, {
            policy: { enabled: false, maintenance: false, graceSeconds: 120 },
            state: { outageStartedAt: null, notified: false, suppressedUntil: 0,
              intentionallyStopped: false, lastState: server.state },
          });
          return;
        }
        if (resource === "/files") {
          const location = fileLocationSchema.parse(query);
          const root = server.fileRoots.find((item) => item.id === location.root);
          await respond(route, fileListingSchema, {
            root,
            path: location.path,
            entries: state.files.get(`${location.root}:${location.path}`) || [],
          });
          return;
        }
      }
      if (server && method === "POST" && /^\/(start|stop|restart)$/.test(resource)) {
        server.state = resource === "/stop" ? "exited" : "running";
        await respond(route, okResponseSchema, { ok: true });
        return;
      }
      if (server && resource === "/files/directory" && method === "POST") {
        const input = createDirectoryRequestSchema.parse(body);
        const key = `${input.root}:${input.path}`;
        state.files.set(key, [...(state.files.get(key) || []), {
          name: input.name, type: "directory", size: 0, modifiedAt: CREATED_AT,
        }]);
        await respond(route, okResponseSchema, { ok: true });
        return;
      }
      if (server && resource === "/files/rename" && method === "PATCH") {
        const input = renameFileRequestSchema.parse(body);
        const parts = input.path.split("/");
        const name = parts.pop();
        const key = `${input.root}:${parts.join("/")}`;
        state.files.set(key, (state.files.get(key) || []).map((entry) =>
          entry.name === name ? { ...entry, name: input.newName } : entry,
        ));
        await respond(route, okResponseSchema, { ok: true });
        return;
      }
      if (server && resource === "/files" && method === "DELETE") {
        const input = fileLocationSchema.parse(query);
        const parts = input.path.split("/");
        const name = parts.pop();
        const key = `${input.root}:${parts.join("/")}`;
        state.files.set(key, (state.files.get(key) || []).filter((entry) => entry.name !== name));
        await respond(route, okResponseSchema, { ok: true });
        return;
      }
      unexpected.push(`${method} ${path}`);
      await respond(route, apiErrorSchema, { error: "Unmocked browser fixture request" }, 501);
    });
    await page.routeWebSocket("**/ws/**", (route) => {
      const socket: FixtureSocket = { path: new URL(route.url()).pathname, route, messages: [] };
      state.sockets.push(socket);
      route.onMessage((message) => socket.messages.push(String(message)));
    });
    await runTest(state);
    expect(unexpected, "All browser requests must use explicit local fixtures").toEqual([]);
    expect(pageErrors, "The browser should have no uncaught application errors").toEqual([]);
  },
});

export { expect };
