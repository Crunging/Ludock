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
import { serversRoutes } from "../src/routes/servers.js";
import type { RequestContext } from "../src/routes/request.js";
import { isServerBusy } from "../src/operation-locks.js";
import * as notifications from "../src/notifications.js";
import { getAvailability } from "../src/monitoring.js";
import type { JobContext } from "../src/operations.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin = { id: "admin", username: "admin", role: "admin" as const };
const operator = { id: "operator", username: "operator", role: "operator" as const };
const containerId = dockerId("restart_backup");
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

function routeContext(path: string, body: unknown, actor = operator): RequestContext {
  const url = new URL(`http://localhost/api/v1/servers/${serverId}/${path}`);
  const { token } = createSession(actor, new Request(url.href));
  return { request: new Request(url.href, { headers: { Cookie: `ludock_session=${token}` } }),
    user: actor, params: { id: serverId }, url, body, headers: new Headers() };
}
/** Mirrors the real backup's completion record, which allows the server to start. */
function completedBackup(job: JobContext) {
  job.progress("backup_complete", { backupId: "backup-fixture", backupComplete: true });
  return { id: "backup-fixture" } as Awaited<ReturnType<typeof backups.createStoppedBackup>>;
}
function enqueue() {
  return enqueueOperation({ serverId, actorId: operator.id, kind: "restart", bindingRevision: 1,
    input: { createBackup: true } });
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
const restartRoute = serversRoutes["/api/v1/servers/:id/restart"].POST!;

describe("startup update capability", () => {
  for (const compose of [false, true])
    it(`reports startup updates without source mounts (${compose ? "Compose" : "standalone"})`, async () => {
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
    const response = await capabilityRoute(routeContext("update-capability", undefined));
    expect(response.status).toBe(200);
    expect(updateCapabilityResponseSchema.parse(await response.json()).capability).toMatchObject({ updatesOnStart: true, available: false });
    expect(spawn).not.toHaveBeenCalled();
  });
});

describe("restart requests", () => {
  it("requires backup access only when a backup is selected", async () => {
    grants(false);
    await expect(restartRoute(routeContext("restart", { createBackup: true }))).rejects.toMatchObject({ code: "FORBIDDEN" });
    for (const body of [undefined, { createBackup: false }]) {
      const response = await restartRoute(routeContext("restart", body));
      expect(response.status).toBe(202);
      const { id } = operationResponseSchema.parse(await response.json()).operation;
      expect(getOperation(id)?.input).toEqual({});
      getDatabase().query("UPDATE operations SET status='succeeded' WHERE id=?").run(id);
    }
    grants();
    const response = await restartRoute(routeContext("restart", { createBackup: true }));
    const operation = operationResponseSchema.parse(await response.json()).operation;
    expect(getOperation(operation.id)?.input).toEqual({ createBackup: true });
    expect(listOperationHistory(operator, { serverId, limit: 10 }).operations[0].kind).toBe("restart");
  });
  it("rejects malformed options for any image", async () => {
    image = "factoriotools/factorio";
    await expect(restartRoute(routeContext("restart", { createBackup: "yes" }))).rejects.toThrow();
    await expect(restartRoute(routeContext("restart", { createBackup: true, forceRecreate: true }))).rejects.toThrow();
    const response = await restartRoute(routeContext("restart", { createBackup: true }));
    expect(response.status).toBe(202);
    expect(calls).toEqual([]);
  });
  it("rechecks request authentication after asynchronous discovery", async () => {
    const ctx = routeContext("restart", { createBackup: true });
    onInspect = () => deleteRequestSession(ctx.request);
    await expect(restartRoute(ctx)).rejects.toMatchObject({ code: "AUTHENTICATION_REQUIRED" });
    expect(listOperationHistory(admin, { serverId, limit: 10 }).operations).toEqual([]);
  });
});

describe("restarts with a backup", () => {
  it("holds the locks, keeps the server stopped during the backup, and starts it once", async () => {
    spyOn(backups, "createStoppedBackup").mockImplementation(async (_server, job) => {
      expect(running).toBe(false);
      expect(isServerBusy(serverId)).toBe(true);
      calls.push("backup");
      return completedBackup(job);
    });
    const operation = enqueue();
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("succeeded");
    expect(calls).toEqual(["save", "stop", "backup", "start"]);
    expect(getOperation(operation.id)?.result).toMatchObject({ action: "restart", backupId: "backup-fixture", saveStatus: "sent" });
    expect(isServerBusy(serverId)).toBe(false);
  });
  it("doesn't report a server that another manager started during the backup as stopped", async () => {
    const notify = spyOn(notifications, "notifyEvent");
    spyOn(backups, "createStoppedBackup").mockImplementation(async () => {
      running = true;
      throw new Error("started externally");
    });
    const operation = enqueue();
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(calls).toEqual(["save", "stop"]);
    expect(getAvailability(serverId).state.intentionallyStopped).toBe(false);
    expect(notify.mock.calls.some(([, message]) => message.includes("left stopped"))).toBe(false);
  });
  it("leaves the server stopped and says so when the backup fails", async () => {
    const notify = spyOn(notifications, "notifyEvent");
    spyOn(backups, "createStoppedBackup").mockRejectedValue(new Error("private-fixture"));
    const operation = enqueue();
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(calls).toEqual(["save", "stop"]);
    expect(running).toBe(false);
    expect(getOperation(operation.id)?.error).not.toContain("private-fixture");
    expect(getAvailability(serverId).state.intentionallyStopped).toBe(true);
    expect(notify).toHaveBeenCalledWith(`operation:${operation.id}`,
      "world: the backup failed, so the server was left stopped. Review the operation, then start it.");
  });
  for (const revoked of ["server.restart", "backups.create"] as const)
    it(`rechecks ${revoked} after the backup`, async () => {
      spyOn(backups, "createStoppedBackup").mockImplementation(async (_server, job) => {
        setServerGrant(operator.id, serverId,
          ["server.view", revoked === "server.restart" ? "backups.create" : "server.restart"], admin);
        return completedBackup(job);
      });
      const operation = enqueue();
      await startOperationRunner();
      expect((await finished(operation.id)).status).toBe("failed");
      // With a completed backup, the only start returns the server to its initial running state.
      expect(calls).toEqual(["save", "stop", "start"]);
      expect(getOperation(operation.id)?.phase).not.toBe("started");
    });
  it("rechecks the binding at the final Docker inspection", async () => {
    spyOn(backups, "createStoppedBackup").mockImplementation(async (_server, job) => completedBackup(job));
    const operation = enqueue();
    onInspect = () => {
      if (getOperation(operation.id)?.phase === "starting") name = "/different";
    };
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(calls).toEqual(["save", "stop"]);
  });
  for (const [recovery, backupComplete, docker, expected] of [
    [true, true, "stopped", ["start"]],
    [true, true, "running", []],
    [true, false, "stopped", []],
    [false, false, "running", []],
  ] as const)
    it(`reconciles interruption, starting only after a completed backup (stop recorded: ${recovery}, backup complete: ${backupComplete}, Docker: ${docker})`, async () => {
      const notify = spyOn(notifications, "notifyEvent");
      const server = await resolveAuthorizedServer(operator, serverId, "server.restart");
      const operation = enqueue();
      running = docker === "running";
      getDatabase().query("UPDATE operations SET status='running',recovery_json=? WHERE id=?").run(JSON.stringify(recovery ? {
        initialRunning: true, dataSafe: true, containerId, fingerprint: server.logical.bindingFingerprint,
        bindingRevision: 1, stopSettled: true, stoppedStartedAt: startedAt, backupComplete,
      } : {}), operation.id);
      await startOperationRunner();
      expect(getOperation(operation.id)?.status).toBe("interrupted");
      expect(calls).toEqual([...expected]);
      // A server deliberately left stopped is not an outage.
      const leftStopped = recovery && !backupComplete;
      expect(getAvailability(serverId).state.intentionallyStopped).toBe(leftStopped);
      expect(notify.mock.calls.some(([, message]) => message.includes("left stopped"))).toBe(leftStopped);
    });
  for (const startedSinceStop of [false, true])
    it(`never repeats a start that reached Docker (started since the backup's stop: ${startedSinceStop})`, async () => {
      const server = await resolveAuthorizedServer(operator, serverId, "server.restart");
      const operation = enqueue();
      running = false;
      const stoppedStartedAt = startedAt;
      // The start reached Docker and the game has exited since.
      if (startedSinceStop) startedAt = "2026-01-02T00:00:00Z";
      getDatabase().query("UPDATE operations SET status='running',recovery_json=? WHERE id=?").run(JSON.stringify({
        initialRunning: true, dataSafe: true, containerId, fingerprint: server.logical.bindingFingerprint,
        bindingRevision: 1, stopSettled: true, stoppedStartedAt, backupComplete: true, startRequested: true,
      }), operation.id);
      await startOperationRunner();
      expect(getOperation(operation.id)?.status).toBe("interrupted");
      expect(calls).toEqual(startedSinceStop ? [] : ["start"]);
    });
});
