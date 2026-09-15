import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import {
  attentionResponseSchema,
  operationsResponseSchema,
  schedulesResponseSchema,
  serverResponseSchema,
  type Operation,
  type ScheduleInput,
} from "@ludock/shared";
import { createApp } from "../src/app.js";
import { listAttention } from "../src/attention.js";
import { createSession } from "../src/auth.js";
import { setServerGrant } from "../src/authorization.js";
import {
  closeDatabase,
  createUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import { getLogicalServer, listLogicalServers } from "../src/identity.js";
import { configureAvailability } from "../src/monitoring.js";
import { acquireLocks } from "../src/operation-locks.js";
import { enqueueOperation, stopOperationRunner } from "../src/operations.js";
import { createSchedule } from "../src/schedules.js";
import { getServer, refreshServers, resolveAuthorizedServer } from "../src/servers.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: crypto.randomUUID(), username: "admin", role: "admin" };
const operator: SessionUser = { id: crypto.randomUUID(), username: "operator", role: "operator" };
const other: SessionUser = { id: crypto.randomUUID(), username: "other", role: "operator" };
const viewer: SessionUser = { id: crypto.randomUUID(), username: "viewer", role: "viewer" };
const now = 50_000;
const scheduleInput: ScheduleInput = {
  action: "start",
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: "UTC",
};
interface ContainerFixture {
  id: string;
  name: string;
  mounts?: Array<{ Type: string; Source: string; Destination: string; RW: boolean }>;
}
const docker = getDockerInstance();
const originals = { listContainers: docker.listContainers, getContainer: docker.getContainer };
let containers: ContainerFixture[];
let unavailable: boolean;
let beforeList: (() => Promise<void> | void) | undefined;
let beforeInspect: (() => Promise<void> | void) | undefined;
let beforeStats: (() => Promise<void> | void) | undefined;
let worldId: string;
let privateId: string;
let inspectCalls: number;
let statsCalls: number;
let mutationCalls: string[];

function requestAs(actor: SessionUser, pathname: string, method = "GET") {
  const url = `http://localhost${pathname}`;
  const session = createSession(actor, new Request(url));
  return createApp({ frontendDist: false }).fetch(new Request(url, {
    method,
    headers: { Cookie: `ludock_session=${session.token}` },
  }), { timeout: () => {}, requestIP: () => null });
}

function addOperation(
  serverId = worldId,
  status: Operation["status"] = "failed",
  timestamp = 10,
) {
  const operation = enqueueOperation({
    serverId,
    actorId: admin.id,
    kind: "backup",
    bindingRevision: 1,
    input: { privateFixture: "private-operation-input" },
  });
  getDatabase().prepare(
    "UPDATE operations SET status=?,created_at=?,updated_at=?,error=?,result_json=? WHERE id=?",
  ).run(status, timestamp, timestamp, "private-operation-error", '{"privateFixture":"private-operation-result"}', operation.id);
  return operation.id;
}

function persistOutage(serverId = worldId, startedAt = 1_000) {
  configureAvailability(serverId, { enabled: true, maintenance: false, graceSeconds: 10 });
  getDatabase().prepare(
    "UPDATE availability SET outage_started_at=?,last_state='exited' WHERE server_id=?",
  ).run(startedAt, serverId);
}

beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  beforeList = undefined;
  beforeInspect = undefined;
  beforeStats = undefined;
  unavailable = false;
  inspectCalls = 0;
  statsCalls = 0;
  mutationCalls = [];
  containers = [
    {
      id: "world-container", name: "world",
      mounts: [{ Type: "bind", Source: "/fixture/world-data", Destination: "/data", RW: true }],
    },
    { id: "private-container", name: "private-world" },
  ];
  for (const actor of [admin, operator, other, viewer]) {
    createUser({ ...actor, passwordHash: "fixture", disabled: false, createdAt: 0 });
  }
  docker.listContainers = (async () => {
    await beforeList?.();
    if (unavailable) throw new Error("private Docker connection failure");
    return containers.map((container) => ({
      Id: container.id,
      Names: [`/${container.name}`],
      Image: "itzg/minecraft-server",
      Labels: {},
    }));
  }) as unknown as typeof docker.listContainers;
  docker.getContainer = ((id: string) => ({
    inspect: async () => {
      inspectCalls += 1;
      await beforeInspect?.();
      const container = containers.find((entry) => entry.id === id)!;
      return {
        Id: id,
        Name: `/${container.name}`,
        Config: { Image: "itzg/minecraft-server", Labels: {} },
        State: { Status: "exited" },
        Mounts: container.mounts ?? [],
        NetworkSettings: { Ports: {} },
        Created: "2026-01-01T00:00:00Z",
      };
    },
    stats: async () => {
      statsCalls += 1;
      await beforeStats?.();
      return {
        cpu_stats: { cpu_usage: { total_usage: 0 }, system_cpu_usage: 0, online_cpus: 1 },
        precpu_stats: { cpu_usage: { total_usage: 0 }, system_cpu_usage: 0 },
        memory_stats: { usage: 2 * 1024 * 1024, limit: 4 * 1024 * 1024 },
      };
    },
    start: async () => { mutationCalls.push("start"); },
    stop: async () => { mutationCalls.push("stop"); },
    restart: async () => { mutationCalls.push("restart"); },
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  worldId = listLogicalServers().find((server) => server.containerId === "world-container")!.id;
  privateId = listLogicalServers().find((server) => server.containerId === "private-container")!.id;
  for (const actor of [operator, other]) {
    setServerGrant(actor.id, worldId, ["server.view", "server.start", "schedules.manage"], admin);
  }
  setServerGrant(viewer.id, worldId, ["server.view"], admin);
});

afterEach(async () => {
  await stopOperationRunner();
  Object.assign(docker, originals);
  closeDatabase();
});

describe("dashboard attention", () => {
  it("reads failure summaries without decoding unrelated operation payloads", async () => {
    const id = addOperation();
    getDatabase().prepare("UPDATE operations SET result_json='damaged private result' WHERE id=?").run(id);
    const result = await listAttention(admin);
    expect(result.items.some((item) => item.kind === "operation" && item.operationId === id)).toBeTruthy();
    expect(JSON.stringify(result)).not.toMatch(/private result/);
  });
  it("authenticates the HTTP endpoint and validates its public response", async () => {
    const handler = createApp({ frontendDist: false }).routes["/api/v1/attention"].GET!;
    const server = { timeout: () => {}, requestIP: () => null };
    const url = "http://localhost/api/v1/attention";
    const anonymous = await handler(new Request(url), server);
    expect(anonymous.status).toBe(401);
    const operationId = addOperation();
    const session = createSession(viewer, new Request(url));
    const response = await handler(new Request(url, {
      headers: { Cookie: `ludock_session=${session.token}` },
    }), server);
    expect(response.status).toBe(200);
    expect(response.headers.get("cache-control")).toBe("no-store");
    const body: unknown = await response.json();
    const attention = attentionResponseSchema.parse(body);
    expect(attention.discoveryUnavailable).toBe(false);
    expect(attention.items.length).toBe(1);
    const item = attention.items[0];
    expect(item.kind).toBe("operation");
    if (item.kind !== "operation") throw new Error("Expected operation attention");
    expect(item.operationId).toBe(operationId);
    expect(item.serverId).toBe(worldId);
    expect(JSON.stringify(body)).not.toMatch(/private-operation|world-container/);
  });

  it("limits read-only issues to granted logical servers without exposing operation details", async () => {
    const visible = addOperation();
    addOperation(privateId);
    persistOutage();
    persistOutage(privateId);
    createSchedule(operator, worldId, scheduleInput);
    setServerGrant(operator.id, worldId, ["server.view", "schedules.manage"], admin);

    const response = attentionResponseSchema.parse(await listAttention(viewer, now));

    expect(response.items.map((item) => item.kind)).toStrictEqual(["availability", "operation"]);
    expect(response.items.every((item) => item.serverId === worldId)).toBeTruthy();
    const operation = response.items.find((item) => item.kind === "operation")!;
    expect(operation.operationId).toBe(visible);
    const serialized = JSON.stringify(response);
    expect(serialized).not.toMatch(/private-world|private-container|private-operation/);
    expect(!serialized.includes(privateId)).toBeTruthy();
    expect(!serialized.includes("world-container")).toBeTruthy();
  });

  it("shows only the operator's enabled suspended schedules and respects the current role ceiling", async () => {
    const own = createSchedule(operator, worldId, scheduleInput);
    const another = createSchedule(other, worldId, scheduleInput);
    createSchedule(operator, worldId, { ...scheduleInput, enabled: false });
    for (const actor of [operator, other]) {
      setServerGrant(actor.id, worldId, ["server.view", "schedules.manage"], admin);
    }

    const response = await listAttention(operator, now);
    expect(response.items.length).toBe(1);
    const item = response.items[0];
    expect(item.kind).toBe("schedule");
    if (item.kind !== "schedule") throw new Error("Expected schedule attention");
    expect(item.scheduleId).toBe(own.id);
    expect(item.reason).toBe("action_access_removed");
    const administrator = await listAttention(admin, now);
    expect(new Set(administrator.items.map((entry) => entry.kind === "schedule" && entry.scheduleId))).toStrictEqual(new Set([own.id, another.id]));

    updateUserAccess(operator.id, "viewer", false);
    expect((await listAttention(operator, now)).items).toStrictEqual([]);
  });

  it("rechecks grants and disabled accounts after asynchronous discovery", async () => {
    addOperation();
    beforeList = () => {
      setServerGrant(viewer.id, worldId, [], admin);
      unavailable = true;
    };
    expect(await listAttention(viewer, now)).toStrictEqual({
      items: [], discoveryUnavailable: false,
    });

    beforeList = () => updateUserAccess(admin.id, "admin", true);
    expect(await listAttention(admin, now)).toStrictEqual({
      items: [], discoveryUnavailable: false,
    });
  });

  it("keeps saved failures and identities during a Docker outage", async () => {
    const operationId = addOperation();
    persistOutage();
    const original = getLogicalServer(worldId)!;
    unavailable = true;

    const response = await listAttention(viewer, now);

    expect(response.discoveryUnavailable).toBe(true);
    expect(response.items.map((item) => item.kind)).toStrictEqual(["availability", "operation"]);
    expect(response.items.find((item) => item.kind === "operation")!.operationId).toBe(operationId);
    const after = getLogicalServer(worldId)!;
    expect(after.containerId).toBe(original.containerId);
    expect(after.status).toBe("active");
    expect(after.bindingRevision).toBe(original.bindingRevision);
    expect(JSON.stringify(response)).not.toMatch(/private Docker connection failure/);
  });

  it("does not disclose discovery outages to users without visible servers", async () => {
    unavailable = true;
    setServerGrant(viewer.id, worldId, [], admin);
    expect(await listAttention(viewer, now)).toStrictEqual({
      items: [], discoveryUnavailable: false,
    });
    expect((await listAttention(admin, now)).discoveryUnavailable).toBe(true);
    updateUserAccess(admin.id, "admin", true);
    expect(await listAttention(admin, now)).toStrictEqual({
      items: [], discoveryUnavailable: false,
    });
  });

  it("refreshes material binding changes before applying permissions", async () => {
    addOperation();
    persistOutage();
    createSchedule(operator, worldId, scheduleInput);
    containers[0].mounts = [{
      Type: "bind", Source: "/fixture/replacement", Destination: "/data", RW: true,
    }];

    expect((await listAttention(operator, now)).items).toStrictEqual([]);
    const response = await listAttention(admin, now);
    expect(response.items.map((item) => item.kind)).toStrictEqual(["binding", "availability", "operation"]);
    const binding = response.items[0];
    expect(binding.kind).toBe("binding");
    if (binding.kind !== "binding") throw new Error("Expected binding attention");
    expect(binding.bindingStatus).toBe("review_required");
    expect(binding.serverId).toBe(worldId);
  });

  it("shows missing and ambiguous bindings only to administrators", async () => {
    containers = [containers[1]];
    expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    const missing = (await listAttention(admin, now)).items[0];
    expect(missing.kind).toBe("binding");
    if (missing.kind !== "binding") throw new Error("Expected binding attention");
    expect(missing.bindingStatus).toBe("missing");

    containers.push({ id: "replacement-one", name: "world" }, { id: "replacement-two", name: "world" });
    const ambiguous = (await listAttention(admin, now)).items[0];
    expect(ambiguous.kind).toBe("binding");
    if (ambiguous.kind !== "binding") throw new Error("Expected binding attention");
    expect(ambiguous.bindingStatus).toBe("ambiguous");
    expect(ambiguous.serverId).toBe(worldId);
    expect((await listAttention(operator, now)).items).toStrictEqual([]);
  });

  it("requires an enabled outage past grace and suppresses intentional or active work downtime", async () => {
    persistOutage();
    expect((await listAttention(viewer, 10_999)).items).toStrictEqual([]);
    expect((await listAttention(viewer, 11_000)).items[0].kind).toBe("availability");

    for (const policy of [
      { enabled: false, maintenance: false, graceSeconds: 10 },
      { enabled: true, maintenance: true, graceSeconds: 10 },
    ]) {
      getDatabase().prepare("UPDATE availability SET policy_json=? WHERE server_id=?")
        .run(JSON.stringify(policy), worldId);
      expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    }
    persistOutage();
    getDatabase().prepare("UPDATE availability SET intentionally_stopped=1 WHERE server_id=?").run(worldId);
    expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    getDatabase().prepare("UPDATE availability SET intentionally_stopped=0,suppressed_until=? WHERE server_id=?").run(now + 1, worldId);
    expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    getDatabase().prepare("UPDATE availability SET suppressed_until=0 WHERE server_id=?").run(worldId);

    const release = acquireLocks([`server:${worldId}`]);
    try {
      expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    } finally { release(); }
    const queued = addOperation(worldId, "queued");
    expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    getDatabase().prepare("UPDATE operations SET status='running' WHERE id=?").run(queued);
    expect((await listAttention(viewer, now)).items).toStrictEqual([]);
    getDatabase().prepare("UPDATE operations SET status='succeeded' WHERE id=?").run(queued);
    expect((await listAttention(viewer, now)).items[0].kind).toBe("availability");
  });

  it("limits failures to the most recent 100 operations and sorts them by latest update", async () => {
    const expiredFailure = addOperation(worldId, "failed", 0);
    const older = addOperation(worldId, "failed", 1);
    for (let index = 2; index <= 99; index += 1) addOperation(worldId, "succeeded", index);
    const newer = addOperation(worldId, "interrupted", 100);
    getDatabase().prepare("UPDATE operations SET updated_at=101 WHERE id=?").run(older);

    const response = await listAttention(viewer, now);
    expect(response.items.map((item) => item.kind === "operation" && item.operationId)).toStrictEqual([older, newer]);
    expect(!JSON.stringify(response).includes(expiredFailure)).toBeTruthy();
  });
});

describe("server detail and attention resolution", () => {
  it("keeps saved backup summaries permission-filtered during a Docker outage", async () => {
    getDatabase().prepare(
      `INSERT INTO backups
        (id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
        VALUES(?,?,?,?,?,?,?,?,?)`,
    ).run(crypto.randomUUID(), worldId, "fixture-fingerprint", "/private-backup-destination", "[]", 24, "private-checksum", 200, "complete");
    setServerGrant(operator.id, worldId, ["server.view", "backups.create"], admin);
    unavailable = true;

    for (const actor of [admin, operator, viewer]) {
      const response = await requestAs(actor, `/api/v1/servers/${worldId}`);
      expect(response.status).toBe(200);
      const detail = serverResponseSchema.parse(await response.json());
      expect(detail.discoveryUnavailable).toBe(true);
      expect(detail.server.latestBackup).toStrictEqual(actor.role === "viewer" ? null : { createdAt: 200, size: 24 });
      expect(JSON.stringify(detail)).not.toMatch(/private-backup-destination|private-checksum|fixture-fingerprint/);
    }

    setServerGrant(operator.id, worldId, ["server.view"], admin);
    const response = await requestAs(operator, `/api/v1/servers/${worldId}`);
    expect(serverResponseSchema.parse(await response.json()).server.latestBackup).toBe(null);
  });

  it("keeps permitted detail, operation history, and schedule history available without live Docker metadata", async () => {
    const operationId = addOperation();
    const schedule = createSchedule(operator, worldId, scheduleInput);
    setServerGrant(operator.id, worldId, ["server.view", "schedules.manage"], admin);
    unavailable = true;
    inspectCalls = 0;

    for (const actor of [viewer, operator, admin]) {
      const response = await requestAs(actor, `/api/v1/servers/${worldId}`);
      expect(response.status).toBe(200);
      const raw: unknown = await response.json();
      const detail = serverResponseSchema.parse(raw);
      expect(detail.discoveryUnavailable).toBe(true);
      expect(detail.stats).toBe(null);
      expect(detail.server.id).toBe(worldId);
      expect(detail.server.bindingStatus).toBe("active");
      expect(detail.server.state).toBe("unknown");
      expect(detail.server.status).toBe("Live status unavailable");
      expect(detail.server.shortId).toBe("");
      expect(detail.server.image).toBe("");
      expect(detail.server.gameConsole).toBe(null);
      expect(detail.server.fileRoots).toStrictEqual([]);
      expect(detail.server.ports).toStrictEqual([]);
      expect(detail.server.labels).toStrictEqual({});
      expect(detail.server.permissions.includes("server.view")).toBeTruthy();
      if (actor.role !== "viewer") expect(detail.server.permissions.includes("schedules.manage")).toBeTruthy();
      expect(JSON.stringify(raw)).not.toMatch(/world-container|itzg\/minecraft|private Docker/);

      const operations = await requestAs(actor, `/api/v1/servers/${worldId}/operations`);
      expect(operations.status).toBe(200);
      expect(operationsResponseSchema.parse(await operations.json()).operations[0].id).toBe(operationId);
    }
    for (const actor of [operator, admin]) {
      const schedules = await requestAs(actor, `/api/v1/servers/${worldId}/schedules`);
      expect(schedules.status).toBe(200);
      const saved = schedulesResponseSchema.parse(await schedules.json()).schedules;
      expect(saved[0].id).toBe(schedule.id);
      expect(saved[0].nextRunUnavailableReason).toBe("action_access_removed");
    }
    expect((await requestAs(viewer, `/api/v1/servers/${worldId}/schedules`)).status).toBe(403);
    expect(inspectCalls).toBe(0);
    expect(statsCalls).toBe(0);
    expect(mutationCalls).toStrictEqual([]);
  });

  it("preserves normal live detail and statistics when discovery succeeds", async () => {
    let lists = 0;
    beforeList = () => { lists += 1; };
    inspectCalls = 0;
    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);
    expect(response.status).toBe(200);
    const detail = serverResponseSchema.parse(await response.json());
    expect(detail.discoveryUnavailable).toBe(false);
    expect(detail.server.state).toBe("exited");
    expect(detail.server.bindingStatus).toBe("active");
    expect(detail.server.image).toBe("itzg/minecraft-server");
    expect(detail.stats).toStrictEqual({ cpuPercent: 0, memUsageMB: 2, memLimitMB: 4 });
    expect(statsCalls).toBe(1);
    expect(lists, "A detail read should discover the fleet only once").toBe(1);
    expect(inspectCalls, "Only the selected container needs a second inspection").toBe(containers.length + 1);
  });

  it("rejects unassigned, revoked, and disabled accounts after failed discovery", async () => {
    unavailable = true;
    expect((await requestAs(viewer, `/api/v1/servers/${privateId}`)).status).toBe(404);

    beforeList = () => setServerGrant(viewer.id, worldId, [], admin);
    const revoked = await requestAs(viewer, `/api/v1/servers/${worldId}`);
    expect(revoked.status).toBe(404);
    expect(await revoked.text()).not.toMatch(/world-container|Live status unavailable/);

    beforeList = () => updateUserAccess(admin.id, "admin", true);
    expect((await requestAs(admin, `/api/v1/servers/${worldId}`)).status).toBe(404);
    beforeList = undefined;
    expect((await requestAs(admin, `/api/v1/servers/${worldId}`)).status).toBe(401);
  });

  it("keeps strict server reads and mutation binding resolution unavailable until Docker recovers", async () => {
    unavailable = true;
    inspectCalls = 0;

    await expect(getServer(admin, worldId)).rejects.toThrow(/private Docker connection failure/);
    await expect(resolveAuthorizedServer(admin, worldId, "server.start")).rejects.toThrow(/private Docker connection failure/);
    const start = await requestAs(admin, `/api/v1/servers/${worldId}/start`, "POST");
    expect(start.status).toBe(500);
    expect(await start.text()).not.toMatch(/private Docker connection failure/);
    expect(mutationCalls).toStrictEqual([]);
    expect(inspectCalls).toBe(0);
    expect(statsCalls).toBe(0);
    expect(getLogicalServer(worldId)!.containerId).toBe("world-container");
    expect(getLogicalServer(worldId)!.status).toBe("active");
  });

  it("rechecks view access when the selected binding inspection revokes a grant", async () => {
    inspectCalls = 0;
    beforeInspect = () => {
      if (inspectCalls > containers.length) setServerGrant(viewer.id, worldId, [], admin);
    };

    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);

    expect(response.status).toBe(404);
    expect(inspectCalls > containers.length).toBeTruthy();
    expect(statsCalls).toBe(0);
    expect(await response.text()).not.toMatch(/world-container|itzg\/minecraft/);
  });

  it("rechecks view access after statistics finish", async () => {
    beforeStats = () => setServerGrant(viewer.id, worldId, [], admin);

    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);

    expect(response.status).toBe(404);
    expect(statsCalls).toBe(1);
    expect(await response.text()).not.toMatch(/world-container|itzg\/minecraft/);
  });

  it("discards live fields when the selected container changes before statistics", async () => {
    inspectCalls = 0;
    beforeInspect = () => {
      if (inspectCalls > containers.length) containers[0].mounts = [
        { Type: "bind", Source: "/fixture/changed-data", Destination: "/data", RW: true },
      ];
    };
    const response = await requestAs(admin, `/api/v1/servers/${worldId}`);
    const detail = serverResponseSchema.parse(await response.json());
    expect(response.status).toBe(200);
    expect(detail.discoveryUnavailable).toBe(true);
    expect(detail.server.state).toBe("unknown");
    expect(detail.server.image).toBe("");
    expect(detail.server.fileRoots).toStrictEqual([]);
    expect(detail.stats).toBe(null);
    expect(statsCalls).toBe(0);
  });

  it("retains the verified server snapshot if only its statistics read fails", async () => {
    beforeStats = () => { throw new Error("private statistics failure"); };
    const response = await requestAs(admin, `/api/v1/servers/${worldId}`);
    const detail = serverResponseSchema.parse(await response.json());
    expect(response.status).toBe(200);
    expect(detail.discoveryUnavailable).toBe(false);
    expect(detail.server.state).toBe("exited");
    expect(detail.stats).toBe(null);
  });

  it("scrubs file roots and permissions revoked while statistics are pending", async () => {
    setServerGrant(operator.id, worldId, ["server.view", "files.read"], admin);
    const original = await getServer(operator, worldId);
    expect(original.fileRoots.length).toBe(1);
    expect(original.fileRoots[0].path).toBe("/data");
    beforeStats = () => setServerGrant(operator.id, worldId, ["server.view"], admin);

    const response = await requestAs(operator, `/api/v1/servers/${worldId}`);

    expect(response.status).toBe(200);
    const detail = serverResponseSchema.parse(await response.json());
    expect(detail.server.permissions).toStrictEqual(["server.view"]);
    expect(detail.server.fileRoots).toStrictEqual([]);
    expect(detail.discoveryUnavailable).toBe(false);
    expect(detail.server.state).toBe("exited");
    expect(statsCalls).toBe(1);
  });

  it.each(["active", "review_required"] as const)(
    "discards obsolete statistics and live fields after replacement leaves the binding %s",
    async (bindingStatus) => {
      beforeStats = async () => {
        containers[0] = {
          ...containers[0],
          id: "replacement-container",
          ...(bindingStatus === "review_required" ? {
            mounts: [{ Type: "bind", Source: "/fixture/replacement-data", Destination: "/data", RW: true }],
          } : {}),
        };
        await refreshServers();
      };

      const response = await requestAs(admin, `/api/v1/servers/${worldId}`);

      expect(response.status).toBe(200);
      const detail = serverResponseSchema.parse(await response.json());
      expect(detail.server.id).toBe(worldId);
      expect(detail.server.bindingStatus).toBe(bindingStatus);
      expect(detail.server.state).toBe("unknown");
      expect(detail.server.status).toBe("Live status unavailable");
      expect(detail.server.shortId).toBe("");
      expect(detail.server.image).toBe("");
      expect(detail.server.gameConsole).toBe(null);
      expect(detail.server.fileRoots).toStrictEqual([]);
      expect(detail.server.ports).toStrictEqual([]);
      expect(detail.stats).toBe(null);
      expect(detail.discoveryUnavailable).toBe(true);
      expect(statsCalls).toBe(1);
      if (bindingStatus === "review_required") expect(detail.server.permissions).toStrictEqual(["server.view"]);
    },
  );
});
