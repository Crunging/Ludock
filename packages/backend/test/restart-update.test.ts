import { operationResponseSchema, updateCapabilityResponseSchema } from "@ludock/shared";
import { afterEach, beforeEach, describe, expect, it, mock, spyOn } from "bun:test";
import { closeDatabase, createUser, getDatabase } from "../src/database.js";
import { setServerGrant } from "./fixtures/grants.js";
import { docker } from "../src/docker-client.js";
import { dockerId } from "./fixtures/ids.js";
import { listLogicalServers } from "../src/identity.js";
import { refreshServers, resolveAuthorizedServer } from "../src/servers.js";
import { createSession, deleteRequestSession } from "../src/auth.js";
import { updateCapability } from "../src/compose.js";
import * as backups from "../src/backups.js";
import * as lifecycle from "../src/lifecycle.js";
import { registerBackgroundJobs } from "../src/jobs.js";
import { enqueueOperation, getOperation, startOperationRunner, stopOperationRunner } from "../src/operations.js";
import { listOperationHistory } from "../src/history.js";
import { composeRoutes } from "../src/routes/compose.js";
import type { RequestContext } from "../src/routes/request.js";
import { isServerBusy } from "../src/operation-locks.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin = { id: "admin", username: "admin", role: "admin" as const };
const operator = { id: "operator", username: "operator", role: "operator" as const };
const containerId = dockerId("restart_to_update");
const originalRoots = process.env.LUDOCK_COMPOSE_ROOTS;
let serverId: string;
let image: string;
let running: boolean;
let startedAt: string;
let labels: Record<string, string>;
let name: string;
let calls: string[];
let onInspect: () => void;
const grants = (backup = true) => setServerGrant(operator.id, serverId,
  ["server.view", "server.restart", ...(backup ? ["backups.create" as const] : [])], admin);

beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  delete process.env.LUDOCK_COMPOSE_ROOTS;
  image = "itzg/minecraft-server:latest";
  running = true;
  startedAt = "2026-01-01T00:00:00Z";
  name = "/world";
  labels = { "ludock.enable": "true", "ludock.game": "minecraft" };
  calls = [];
  onInspect = () => {};
  for (const actor of [admin, operator])
    createUser({ ...actor, passwordHash: "unused", disabled: false, createdAt: 0 });
  spyOn(docker, "listContainers").mockImplementation(async () => [{
    Id: containerId, Names: [name], Image: image, State: running ? "running" : "exited",
    Status: running ? "Up" : "Exited", Ports: [], Created: 0, Labels: labels,
  }]);
  spyOn(docker, "getContainer").mockImplementation((id) => {
    expect(id).toBe(containerId);
    expect(id).not.toBe(serverId);
    return {
      inspect: async () => {
        onInspect();
        return {
          Id: id, Name: name, Image: "sha256:fixture",
          Config: { Image: image, Labels: labels },
          State: { Status: running ? "running" : "exited", Running: running, StartedAt: startedAt, ExitCode: 0 },
          Mounts: [], NetworkSettings: { Ports: {} }, Created: "2026-01-01T00:00:00Z",
        };
      },
      stop: async () => { calls.push("stop"); running = false; },
      start: async () => { calls.push("start"); running = true; startedAt = "2026-01-02T00:00:00Z"; },
      restart: async () => { calls.push("restart"); running = true; startedAt = "2026-01-02T00:00:00Z"; },
    } as unknown as ReturnType<typeof docker.getContainer>;
  });
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  grants();
  registerBackgroundJobs();
  spyOn(lifecycle, "saveWorld").mockImplementation(async (context) => {
    calls.push("save");
    context.progress("waiting_for_save", { saveStatus: "sent", saveReason: null });
  });
});
afterEach(async () => {
  await stopOperationRunner();
  mock.restore();
  closeDatabase();
  if (originalRoots === undefined) delete process.env.LUDOCK_COMPOSE_ROOTS;
  else process.env.LUDOCK_COMPOSE_ROOTS = originalRoots;
});

const request = (createBackup: boolean) => ({ createBackup, ...(createBackup ? {} : { skipBackupConfirmation: "world" }) });
function routeContext(body: unknown, actor = operator): RequestContext {
  const url = new URL(`http://localhost/api/v1/servers/${serverId}/restart-update`);
  const { token } = createSession(actor, new Request(url.href));
  return { request: new Request(url.href, { headers: { Cookie: `ludock_session=${token}` } }),
    user: actor, params: { id: serverId }, url, body, headers: new Headers() };
}
function enqueue(createBackup = false) {
  return enqueueOperation({ serverId, actorId: operator.id, kind: "restart_to_update", bindingRevision: 1,
    input: { request: request(createBackup) } });
}
async function finished(id: string) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const operation = getOperation(id)!;
    if (!["queued", "running"].includes(operation.status)) return operation;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("Operation did not finish");
}

const capabilityRoute = composeRoutes["/api/v1/servers/:id/update-capability"].GET!;
const restartRoute = composeRoutes["/api/v1/servers/:id/restart-update"].POST!;

describe("restart update capability", () => {
  for (const compose of [false, true])
    it(`offers startup updates without source mounts (${compose ? "Compose" : "standalone"})`, async () => {
      const server = await resolveAuthorizedServer(admin, serverId, "server.update");
      if (compose) server.observation.compose = { project: "games", service: "world", containerNumber: "1" };
      const capability = await updateCapability(server, true);
      expect(capability.updatesOnStart).toBe(true);
      expect(capability.available).toBe(false);
      expect(capability.manager).toBe(compose ? "compose" : "external");
    });
  it("does not infer startup updates from an overridden game label", async () => {
    image = "example/custom-game:latest";
    labels["ludock.game"] = "minecraft";
    const server = await resolveAuthorizedServer(admin, serverId, "server.update");
    expect((await updateCapability(server, true)).updatesOnStart).toBe(false);
  });
  it("allows restart operators without exposing Compose sources or launching Compose", async () => {
    const spawn = spyOn(Bun, "spawn");
    const response = await capabilityRoute(routeContext(undefined));
    expect(response.status).toBe(200);
    expect(updateCapabilityResponseSchema.parse(await response.json()).capability).toMatchObject({ updatesOnStart: true, available: false });
    expect(spawn).not.toHaveBeenCalled();
  });
});

describe("restart update requests", () => {
  it("requires restart access and backup access only when selected", async () => {
    grants(false);
    await expect(restartRoute(routeContext(request(true)))).rejects.toMatchObject({ code: "FORBIDDEN" });
    const response = await restartRoute(routeContext(request(false)));
    expect(response.status).toBe(202);
    const operation = operationResponseSchema.parse(await response.json()).operation;
    expect(getOperation(operation.id)?.input).toEqual({ request: request(false) });
    expect(listOperationHistory(operator, { serverId, limit: 10 }).operations[0].kind).toBe("restart_to_update");
  });
  it("rejects unsupported images, missing confirmation, and malformed options", async () => {
    await expect(restartRoute(routeContext({ createBackup: false }))).rejects.toMatchObject({ code: "CONFIRMATION_REQUIRED" });
    await expect(restartRoute(routeContext({ createBackup: true, forceRecreate: true }))).rejects.toThrow();
    image = "factoriotools/factorio";
    await expect(restartRoute(routeContext(request(true)))).rejects.toMatchObject({ code: "RESTART_UPDATE_UNAVAILABLE" });
    expect(calls).toEqual([]);
  });
  it("rechecks request authentication after asynchronous discovery", async () => {
    const ctx = routeContext(request(false));
    onInspect = () => deleteRequestSession(ctx.request);
    await expect(restartRoute(ctx)).rejects.toMatchObject({ code: "AUTHENTICATION_REQUIRED" });
    expect(listOperationHistory(admin, { serverId, limit: 10 }).operations).toEqual([]);
  });
});

describe("tracked restart updates", () => {
  for (const initiallyRunning of [false, true])
    it(`restarts the existing container once (initially running: ${initiallyRunning})`, async () => {
      running = initiallyRunning;
      const spawn = spyOn(Bun, "spawn");
      const operation = enqueue();
      await startOperationRunner();
      expect((await finished(operation.id)).status).toBe("succeeded");
      expect(calls).toEqual(["save", "restart"]);
      expect(spawn).not.toHaveBeenCalled();
      expect(getOperation(operation.id)?.recovery.restartRequested).toBe(true);
    });
  it("holds the locks and keeps the server stopped between backup and restart", async () => {
    spyOn(backups, "createStoppedBackup").mockImplementation(async () => {
      expect(running).toBe(false);
      expect(isServerBusy(serverId)).toBe(true);
      calls.push("backup");
      return { id: "backup-fixture" } as Awaited<ReturnType<typeof backups.createStoppedBackup>>;
    });
    const operation = enqueue(true);
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("succeeded");
    expect(calls).toEqual(["save", "stop", "backup", "restart"]);
    expect(getOperation(operation.id)?.result).toMatchObject({ backupId: "backup-fixture", saveStatus: "sent" });
    expect(isServerBusy(serverId)).toBe(false);
  });
  it("restores the initial running state after a backup failure without restarting to update", async () => {
    spyOn(backups, "createStoppedBackup").mockRejectedValue(new Error("private-fixture"));
    const operation = enqueue(true);
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(calls).toEqual(["save", "stop", "start"]);
    expect(getOperation(operation.id)?.error).not.toContain("private-fixture");
  });
  it("rechecks backup and restart access after the backup", async () => {
    spyOn(backups, "createStoppedBackup").mockImplementation(async () => {
      grants(false);
      return { id: "backup-fixture" } as Awaited<ReturnType<typeof backups.createStoppedBackup>>;
    });
    const operation = enqueue(true);
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(calls).toEqual(["save", "stop", "start"]);
  });
  for (const change of ["revoke", "rename"])
    it(`rechecks authority at the final Docker inspection (${change})`, async () => {
      const operation = enqueue();
      onInspect = () => {
        if (getOperation(operation.id)?.phase !== "restarting") return;
        if (change === "revoke") setServerGrant(operator.id, serverId, ["server.view"], admin);
        else name = "/different";
      };
      await startOperationRunner();
      expect((await finished(operation.id)).status).toBe("failed");
      expect(calls).toEqual(["save"]);
    });
  for (const [restartRequested, docker, expected] of [
    [false, "stopped", ["start"]],
    [true, "stopped", ["start"]],
    [true, "started", []],
    [true, "running", []],
  ] as const)
    it(`reconciles interruption without replaying a restart (dispatched: ${restartRequested}, Docker: ${docker})`, async () => {
      const server = await resolveAuthorizedServer(operator, serverId, "server.restart");
      const operation = enqueue(true);
      const stoppedStartedAt = startedAt;
      // "started" means Docker ran the restart and the game has exited since.
      running = docker === "running";
      if (docker !== "stopped") startedAt = "2026-01-02T00:00:00Z";
      getDatabase().query("UPDATE operations SET status='running',recovery_json=? WHERE id=?").run(JSON.stringify({
        initialRunning: true, dataSafe: true, containerId, fingerprint: server.logical.bindingFingerprint,
        bindingRevision: 1, stopSettled: true, stoppedStartedAt, restartRequested,
      }), operation.id);
      await startOperationRunner();
      expect(getOperation(operation.id)?.status).toBe("interrupted");
      expect(calls).toEqual([...expected]);
    });
});
