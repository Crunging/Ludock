import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "bun:test";
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
    assert.ok(result.items.some((item) => item.kind === "operation" && item.operationId === id));
    assert.doesNotMatch(JSON.stringify(result), /private result/);
  });
  it("authenticates the HTTP endpoint and validates its public response", async () => {
    const handler = createApp({ frontendDist: false }).routes["/api/v1/attention"].GET!;
    const server = { timeout: () => {}, requestIP: () => null };
    const url = "http://localhost/api/v1/attention";
    const anonymous = await handler(new Request(url), server);
    assert.equal(anonymous.status, 401);
    const operationId = addOperation();
    const session = createSession(viewer, new Request(url));
    const response = await handler(new Request(url, {
      headers: { Cookie: `ludock_session=${session.token}` },
    }), server);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("cache-control"), "no-store");
    const body: unknown = await response.json();
    const attention = attentionResponseSchema.parse(body);
    assert.equal(attention.discoveryUnavailable, false);
    assert.equal(attention.items.length, 1);
    const item = attention.items[0];
    assert.equal(item.kind, "operation");
    if (item.kind !== "operation") throw new Error("Expected operation attention");
    assert.equal(item.operationId, operationId);
    assert.equal(item.serverId, worldId);
    assert.doesNotMatch(JSON.stringify(body), /private-operation|world-container/);
  });

  it("limits read-only issues to granted logical servers without exposing operation details", async () => {
    const visible = addOperation();
    addOperation(privateId);
    persistOutage();
    persistOutage(privateId);
    createSchedule(operator, worldId, scheduleInput);
    setServerGrant(operator.id, worldId, ["server.view", "schedules.manage"], admin);

    const response = attentionResponseSchema.parse(await listAttention(viewer, now));

    assert.deepEqual(response.items.map((item) => item.kind), ["availability", "operation"]);
    assert.ok(response.items.every((item) => item.serverId === worldId));
    const operation = response.items.find((item) => item.kind === "operation")!;
    assert.equal(operation.operationId, visible);
    const serialized = JSON.stringify(response);
    assert.doesNotMatch(serialized, /private-world|private-container|private-operation/);
    assert.ok(!serialized.includes(privateId));
    assert.ok(!serialized.includes("world-container"));
  });

  it("shows only the operator's enabled suspended schedules and respects the current role ceiling", async () => {
    const own = createSchedule(operator, worldId, scheduleInput);
    const another = createSchedule(other, worldId, scheduleInput);
    createSchedule(operator, worldId, { ...scheduleInput, enabled: false });
    for (const actor of [operator, other]) {
      setServerGrant(actor.id, worldId, ["server.view", "schedules.manage"], admin);
    }

    const response = await listAttention(operator, now);
    assert.equal(response.items.length, 1);
    const item = response.items[0];
    assert.equal(item.kind, "schedule");
    if (item.kind !== "schedule") throw new Error("Expected schedule attention");
    assert.equal(item.scheduleId, own.id);
    assert.equal(item.reason, "action_access_removed");
    const administrator = await listAttention(admin, now);
    assert.deepEqual(new Set(administrator.items.map((entry) => entry.kind === "schedule" && entry.scheduleId)), new Set([own.id, another.id]));

    updateUserAccess(operator.id, "viewer", false);
    assert.deepEqual((await listAttention(operator, now)).items, []);
  });

  it("rechecks grants and disabled accounts after asynchronous discovery", async () => {
    addOperation();
    beforeList = () => {
      setServerGrant(viewer.id, worldId, [], admin);
      unavailable = true;
    };
    assert.deepEqual(await listAttention(viewer, now), {
      items: [], discoveryUnavailable: false,
    });

    beforeList = () => updateUserAccess(admin.id, "admin", true);
    assert.deepEqual(await listAttention(admin, now), {
      items: [], discoveryUnavailable: false,
    });
  });

  it("keeps saved failures and identities during a Docker outage", async () => {
    const operationId = addOperation();
    persistOutage();
    const original = getLogicalServer(worldId)!;
    unavailable = true;

    const response = await listAttention(viewer, now);

    assert.equal(response.discoveryUnavailable, true);
    assert.deepEqual(response.items.map((item) => item.kind), ["availability", "operation"]);
    assert.equal(response.items.find((item) => item.kind === "operation")!.operationId, operationId);
    const after = getLogicalServer(worldId)!;
    assert.equal(after.containerId, original.containerId);
    assert.equal(after.status, "active");
    assert.equal(after.bindingRevision, original.bindingRevision);
    assert.doesNotMatch(JSON.stringify(response), /private Docker connection failure/);
  });

  it("does not disclose discovery outages to users without visible servers", async () => {
    unavailable = true;
    setServerGrant(viewer.id, worldId, [], admin);
    assert.deepEqual(await listAttention(viewer, now), {
      items: [], discoveryUnavailable: false,
    });
    assert.equal((await listAttention(admin, now)).discoveryUnavailable, true);
    updateUserAccess(admin.id, "admin", true);
    assert.deepEqual(await listAttention(admin, now), {
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

    assert.deepEqual((await listAttention(operator, now)).items, []);
    const response = await listAttention(admin, now);
    assert.deepEqual(response.items.map((item) => item.kind), ["binding", "availability", "operation"]);
    const binding = response.items[0];
    assert.equal(binding.kind, "binding");
    if (binding.kind !== "binding") throw new Error("Expected binding attention");
    assert.equal(binding.bindingStatus, "review_required");
    assert.equal(binding.serverId, worldId);
  });

  it("shows missing and ambiguous bindings only to administrators", async () => {
    containers = [containers[1]];
    assert.deepEqual((await listAttention(viewer, now)).items, []);
    const missing = (await listAttention(admin, now)).items[0];
    assert.equal(missing.kind, "binding");
    if (missing.kind !== "binding") throw new Error("Expected binding attention");
    assert.equal(missing.bindingStatus, "missing");

    containers.push({ id: "replacement-one", name: "world" }, { id: "replacement-two", name: "world" });
    const ambiguous = (await listAttention(admin, now)).items[0];
    assert.equal(ambiguous.kind, "binding");
    if (ambiguous.kind !== "binding") throw new Error("Expected binding attention");
    assert.equal(ambiguous.bindingStatus, "ambiguous");
    assert.equal(ambiguous.serverId, worldId);
    assert.deepEqual((await listAttention(operator, now)).items, []);
  });

  it("requires an enabled outage past grace and suppresses intentional or active work downtime", async () => {
    persistOutage();
    assert.deepEqual((await listAttention(viewer, 10_999)).items, []);
    assert.equal((await listAttention(viewer, 11_000)).items[0].kind, "availability");

    for (const policy of [
      { enabled: false, maintenance: false, graceSeconds: 10 },
      { enabled: true, maintenance: true, graceSeconds: 10 },
    ]) {
      getDatabase().prepare("UPDATE availability SET policy_json=? WHERE server_id=?")
        .run(JSON.stringify(policy), worldId);
      assert.deepEqual((await listAttention(viewer, now)).items, []);
    }
    persistOutage();
    getDatabase().prepare("UPDATE availability SET intentionally_stopped=1 WHERE server_id=?").run(worldId);
    assert.deepEqual((await listAttention(viewer, now)).items, []);
    getDatabase().prepare("UPDATE availability SET intentionally_stopped=0,suppressed_until=? WHERE server_id=?").run(now + 1, worldId);
    assert.deepEqual((await listAttention(viewer, now)).items, []);
    getDatabase().prepare("UPDATE availability SET suppressed_until=0 WHERE server_id=?").run(worldId);

    const release = acquireLocks([`server:${worldId}`]);
    try {
      assert.deepEqual((await listAttention(viewer, now)).items, []);
    } finally { release(); }
    const queued = addOperation(worldId, "queued");
    assert.deepEqual((await listAttention(viewer, now)).items, []);
    getDatabase().prepare("UPDATE operations SET status='running' WHERE id=?").run(queued);
    assert.deepEqual((await listAttention(viewer, now)).items, []);
    getDatabase().prepare("UPDATE operations SET status='succeeded' WHERE id=?").run(queued);
    assert.equal((await listAttention(viewer, now)).items[0].kind, "availability");
  });

  it("limits failures to the most recent 100 operations and sorts them by latest update", async () => {
    const expiredFailure = addOperation(worldId, "failed", 0);
    const older = addOperation(worldId, "failed", 1);
    for (let index = 2; index <= 99; index += 1) addOperation(worldId, "succeeded", index);
    const newer = addOperation(worldId, "interrupted", 100);
    getDatabase().prepare("UPDATE operations SET updated_at=101 WHERE id=?").run(older);

    const response = await listAttention(viewer, now);
    assert.deepEqual(response.items.map((item) => item.kind === "operation" && item.operationId), [older, newer]);
    assert.ok(!JSON.stringify(response).includes(expiredFailure));
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
      assert.equal(response.status, 200);
      const detail = serverResponseSchema.parse(await response.json());
      assert.equal(detail.discoveryUnavailable, true);
      assert.deepEqual(detail.server.latestBackup, actor.role === "viewer" ? null : { createdAt: 200, size: 24 });
      assert.doesNotMatch(JSON.stringify(detail), /private-backup-destination|private-checksum|fixture-fingerprint/);
    }

    setServerGrant(operator.id, worldId, ["server.view"], admin);
    const response = await requestAs(operator, `/api/v1/servers/${worldId}`);
    assert.equal(serverResponseSchema.parse(await response.json()).server.latestBackup, null);
  });

  it("keeps permitted detail, operation history, and schedule history available without live Docker metadata", async () => {
    const operationId = addOperation();
    const schedule = createSchedule(operator, worldId, scheduleInput);
    setServerGrant(operator.id, worldId, ["server.view", "schedules.manage"], admin);
    unavailable = true;
    inspectCalls = 0;

    for (const actor of [viewer, operator, admin]) {
      const response = await requestAs(actor, `/api/v1/servers/${worldId}`);
      assert.equal(response.status, 200);
      const raw: unknown = await response.json();
      const detail = serverResponseSchema.parse(raw);
      assert.equal(detail.discoveryUnavailable, true);
      assert.equal(detail.stats, null);
      assert.equal(detail.server.id, worldId);
      assert.equal(detail.server.bindingStatus, "active");
      assert.equal(detail.server.state, "unknown");
      assert.equal(detail.server.status, "Live status unavailable");
      assert.equal(detail.server.shortId, "");
      assert.equal(detail.server.image, "");
      assert.equal(detail.server.gameConsole, null);
      assert.deepEqual(detail.server.fileRoots, []);
      assert.deepEqual(detail.server.ports, []);
      assert.deepEqual(detail.server.labels, {});
      assert.ok(detail.server.permissions.includes("server.view"));
      if (actor.role !== "viewer") assert.ok(detail.server.permissions.includes("schedules.manage"));
      assert.doesNotMatch(JSON.stringify(raw), /world-container|itzg\/minecraft|private Docker/);

      const operations = await requestAs(actor, `/api/v1/servers/${worldId}/operations`);
      assert.equal(operations.status, 200);
      assert.equal(operationsResponseSchema.parse(await operations.json()).operations[0].id, operationId);
    }
    for (const actor of [operator, admin]) {
      const schedules = await requestAs(actor, `/api/v1/servers/${worldId}/schedules`);
      assert.equal(schedules.status, 200);
      const saved = schedulesResponseSchema.parse(await schedules.json()).schedules;
      assert.equal(saved[0].id, schedule.id);
      assert.equal(saved[0].nextRunUnavailableReason, "action_access_removed");
    }
    assert.equal((await requestAs(viewer, `/api/v1/servers/${worldId}/schedules`)).status, 403);
    assert.equal(inspectCalls, 0);
    assert.equal(statsCalls, 0);
    assert.deepEqual(mutationCalls, []);
  });

  it("preserves normal live detail and statistics when discovery succeeds", async () => {
    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);
    assert.equal(response.status, 200);
    const detail = serverResponseSchema.parse(await response.json());
    assert.equal(detail.discoveryUnavailable, false);
    assert.equal(detail.server.state, "exited");
    assert.equal(detail.server.bindingStatus, "active");
    assert.equal(detail.server.image, "itzg/minecraft-server");
    assert.deepEqual(detail.stats, { cpuPercent: 0, memUsageMB: 2, memLimitMB: 4 });
    assert.equal(statsCalls, 1);
  });

  it("rejects unassigned, revoked, and disabled accounts after failed discovery", async () => {
    unavailable = true;
    assert.equal((await requestAs(viewer, `/api/v1/servers/${privateId}`)).status, 404);

    beforeList = () => setServerGrant(viewer.id, worldId, [], admin);
    const revoked = await requestAs(viewer, `/api/v1/servers/${worldId}`);
    assert.equal(revoked.status, 404);
    assert.doesNotMatch(await revoked.text(), /world-container|Live status unavailable/);

    beforeList = () => updateUserAccess(admin.id, "admin", true);
    assert.equal((await requestAs(admin, `/api/v1/servers/${worldId}`)).status, 404);
    beforeList = undefined;
    assert.equal((await requestAs(admin, `/api/v1/servers/${worldId}`)).status, 401);
  });

  it("keeps strict server reads and mutation binding resolution unavailable until Docker recovers", async () => {
    unavailable = true;
    inspectCalls = 0;

    await assert.rejects(getServer(admin, worldId), /private Docker connection failure/);
    await assert.rejects(resolveAuthorizedServer(admin, worldId, "server.start"), /private Docker connection failure/);
    const start = await requestAs(admin, `/api/v1/servers/${worldId}/start`, "POST");
    assert.equal(start.status, 500);
    assert.doesNotMatch(await start.text(), /private Docker connection failure/);
    assert.deepEqual(mutationCalls, []);
    assert.equal(inspectCalls, 0);
    assert.equal(statsCalls, 0);
    assert.equal(getLogicalServer(worldId)!.containerId, "world-container");
    assert.equal(getLogicalServer(worldId)!.status, "active");
  });

  it("rechecks view access when the statistics binding refresh revokes a grant", async () => {
    let lists = 0;
    beforeList = () => {
      lists += 1;
      if (lists === 2) setServerGrant(viewer.id, worldId, [], admin);
    };

    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);

    assert.equal(response.status, 404);
    assert.equal(lists, 2);
    assert.equal(statsCalls, 0);
    assert.doesNotMatch(await response.text(), /world-container|itzg\/minecraft/);
  });

  it("rechecks view access after statistics finish", async () => {
    beforeStats = () => setServerGrant(viewer.id, worldId, [], admin);

    const response = await requestAs(viewer, `/api/v1/servers/${worldId}`);

    assert.equal(response.status, 404);
    assert.equal(statsCalls, 1);
    assert.doesNotMatch(await response.text(), /world-container|itzg\/minecraft/);
  });

  it("scrubs file roots and permissions revoked while statistics are pending", async () => {
    setServerGrant(operator.id, worldId, ["server.view", "files.read"], admin);
    const original = await getServer(operator, worldId);
    assert.equal(original.fileRoots.length, 1);
    assert.equal(original.fileRoots[0].path, "/data");
    beforeStats = () => setServerGrant(operator.id, worldId, ["server.view"], admin);

    const response = await requestAs(operator, `/api/v1/servers/${worldId}`);

    assert.equal(response.status, 200);
    const detail = serverResponseSchema.parse(await response.json());
    assert.deepEqual(detail.server.permissions, ["server.view"]);
    assert.deepEqual(detail.server.fileRoots, []);
    assert.equal(detail.discoveryUnavailable, false);
    assert.equal(detail.server.state, "exited");
    assert.equal(statsCalls, 1);
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

      assert.equal(response.status, 200);
      const detail = serverResponseSchema.parse(await response.json());
      assert.equal(detail.server.id, worldId);
      assert.equal(detail.server.bindingStatus, bindingStatus);
      assert.equal(detail.server.state, "unknown");
      assert.equal(detail.server.status, "Live status unavailable");
      assert.equal(detail.server.shortId, "");
      assert.equal(detail.server.image, "");
      assert.equal(detail.server.gameConsole, null);
      assert.deepEqual(detail.server.fileRoots, []);
      assert.deepEqual(detail.server.ports, []);
      assert.equal(detail.stats, null);
      assert.equal(detail.discoveryUnavailable, true);
      assert.equal(statsCalls, 1);
      if (bindingStatus === "review_required") assert.deepEqual(detail.server.permissions, ["server.view"]);
    },
  );
});
