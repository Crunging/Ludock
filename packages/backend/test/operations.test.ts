import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { listAuditHistory, listOperationHistory } from "../src/history.js";
import {
  closeDatabase,
  createUser,
  deleteUser,
  getDatabase,
  type SessionUser,
} from "../src/database.js";
import { reconcileServers, ServerBindingError } from "../src/identity.js";
import {
  enqueueOperation,
  getOperation,
  registerJobHandler,
  startOperationRunner,
  stopOperationRunner,
} from "../src/operations.js";
import {
  acquireLocks,
  withLocks,
  waitForLocksReleased,
} from "../src/operation-locks.js";
import { serverLockKeys } from "../src/servers.js";
import type { SQLQueryBindings } from "bun:sqlite";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const owner: SessionUser = { id: "owner", username: "owner", role: "admin" };
let serverId: string;
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  createUser({
    ...owner,
    passwordHash: "fake",
    disabled: false,
    createdAt: 0,
  });
  serverId = reconcileServers([
    {
      containerId: dockerId("container"),
      name: "world",
      displayName: "World",
      gameType: "minecraft",
      mounts: [],
    },
  ])[0].id;
});
afterEach(async () => {
  await stopOperationRunner();
  closeDatabase();
});
const enqueue = (
  kind: string,
  idempotencyKey?: string,
  input: Record<string, unknown> = {},
) =>
  enqueueOperation({
    serverId,
    actorId: "owner",
    bindingRevision: 1,
    kind,
    input,
    idempotencyKey,
  });
async function finished(id: string) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const operation = getOperation(id)!;
    if (!["queued", "running"].includes(operation.status)) return operation;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  expect.unreachable("Operation did not finish");
}

describe("durable operations", () => {
  for (const status of ["queued", "running"] as const) {
    for (const damaged of ["[]", "{private-fixture", "null"]) {
      it(`isolates invalid ${status} state (${damaged}) and continues valid work`, async () => {
        const runs: string[] = [];
        registerJobHandler("damaged-state", {
          run: async ({ job }) => { runs.push(job.id); },
          recover: async () => { expect.unreachable("Damaged state must never reach recovery"); },
        });
        const damagedJob = enqueue("damaged-state");
        getDatabase().prepare("UPDATE operations SET status=?,input_json=?,created_at=0 WHERE id=?")
          .run(status, damaged, damagedJob.id);
        const other = reconcileServers([{
          containerId: dockerId("other"), name: "other", displayName: "Other", gameType: "minecraft", mounts: [],
        }])[0];
        const valid = enqueueOperation({
          serverId: other.id, actorId: owner.id, kind: "damaged-state", bindingRevision: 1,
        });
        await startOperationRunner();
        expect((await finished(valid.id)).status).toBe("succeeded");
        expect(runs).toStrictEqual([valid.id]);
        const saved = getDatabase().prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT status,error,input_json FROM operations WHERE id=?")
          .get(damagedJob.id) as { status: string; error: string; input_json: string };
        expect(saved.status).toBe(status === "queued" ? "failed" : "interrupted");
        expect(saved.error).toMatch(/administrator/);
        expect(saved.error).not.toMatch(/private-fixture/);
        expect(saved.input_json, "Keep damaged state available for administrator inspection").toBe(damaged);
      });
    }
  }
  it("preserves deliberate binding diagnostics when queued work fails", async () => {
    registerJobHandler("binding-error", {
      run: async () => { throw new ServerBindingError("SERVER_BINDING_CHANGED", "The server binding changed. Review it before retrying."); },
    });
    const operation = enqueue("binding-error");
    await startOperationRunner();
    expect((await finished(operation.id)).error).toBe("The server binding changed. Review it before retrying.");
  });
  it("deduplicates retries while rejecting a reused key with different settings", () => {
    const first = enqueue("idempotency", "same", { createBackup: true });
    const retry = enqueue("idempotency", "same", { createBackup: true });
    expect(retry.id).toBe(first.id);
    expect(() => enqueue("idempotency", "same", { createBackup: false })).toThrow(/different operation settings/);
    expect(() => enqueue("idempotency", "other")).toThrow(/already queued or running/);
    expect(listOperationHistory(owner, { serverId, limit: 50 }).operations.length).toBe(1);
  });
  it("persists recovery data before the handler continues, without exposing it publicly", async () => {
    registerJobHandler("progress", {
      run: async ({ job, progress }) => {
        progress("copying", {
          initiallyRunning: true,
          privatePath: "/private/secret",
        });
        const saved = getOperation(job.id)!;
        expect(saved.phase).toBe("copying");
        expect(saved.recovery.initiallyRunning).toBe(true);
        expect(JSON.stringify(listOperationHistory(owner, { serverId, limit: 50 }).operations).includes("/private/secret")).toBe(false);
        return { copied: true };
      },
    });
    const operation = enqueue("progress");
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("succeeded");
    const audit = listAuditHistory({ limit: 20 }).entries.find(
      (event) => event.action === "server.progress.succeeded",
    );
    expect(audit?.username).toBe("owner");
  });
  it("reconciles a persisted interrupted phase without replaying its mutation", async () => {
    let runs = 0,
      recoveries = 0;
    registerJobHandler("interruption", {
      run: async () => {
        runs++;
      },
      recover: async ({ job, progress }) => {
        recoveries++;
        expect(job.phase).toBe("replacing_data");
        expect(job.recovery.initiallyRunning).toBe(true);
        progress("restoring_state");
      },
    });
    const operation = enqueue("interruption");
    getDatabase()
      .prepare(
        "UPDATE operations SET status='running',phase='replacing_data',recovery_json=? WHERE id=?",
      )
      .run(JSON.stringify({ initiallyRunning: true }), operation.id);
    await startOperationRunner();
    expect(getOperation(operation.id)?.status).toBe("interrupted");
    expect(recoveries).toBe(1);
    expect(runs).toBe(0);
    await stopOperationRunner();
    await startOperationRunner();
    expect(recoveries).toBe(1);
  });
  it("reports recovery failure without exposing exception credentials", async () => {
    registerJobHandler("recovery-failure", {
      run: async () => {},
      recover: async () => {
        throw new Error("token=secret");
      },
    });
    const operation = enqueue("recovery-failure");
    getDatabase()
      .prepare("UPDATE operations SET status='running' WHERE id=?")
      .run(operation.id);
    await startOperationRunner();
    expect(getOperation(operation.id)!.error!).toMatch(/administrator attention/);
    expect(JSON.stringify(listAuditHistory({ limit: 10 }).entries).includes("token=secret")).toBe(false);
  });
  it("waits for startup recovery on shutdown without starting queued work", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    let recoveries = 0;
    let runs = 0;
    registerJobHandler("startup-shutdown", {
      run: async () => {
        runs++;
      },
      recover: async () => {
        recoveries++;
        await gate;
      },
    });
    const interrupted = enqueue("startup-shutdown");
    getDatabase()
      .prepare("UPDATE operations SET status='running' WHERE id=?")
      .run(interrupted.id);
    const starting = startOperationRunner();
    const sameStart = startOperationRunner();
    expect(recoveries).toBe(1);
    let stopped = false;
    const stopping = stopOperationRunner().then(() => {
      stopped = true;
    });
    await Promise.resolve();
    expect(stopped).toBe(false);
    release();
    await Promise.all([starting, sameStart, stopping]);
    expect(getOperation(interrupted.id)?.status).toBe("interrupted");
    const queued = enqueue("startup-shutdown");
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(getOperation(queued.id)?.status).toBe("queued");
    expect(runs).toBe(0);
    await startOperationRunner();
    expect((await finished(queued.id)).status).toBe("succeeded");
  });
  it("completes failure auditing even if an operation's account was deleted", async () => {
    registerJobHandler("deleted-owner", {
      run: async () => {
        throw new Error("revoked");
      },
    });
    const operation = enqueue("deleted-owner");
    deleteUser("owner");
    await startOperationRunner();
    expect((await finished(operation.id)).status).toBe("failed");
    expect(listAuditHistory({ limit: 20 }).entries.some(
        (event) => event.action === "server.deleted-owner.failed",
      )).toBeTruthy();
  });
  it("serializes queued jobs and stops accepting work during shutdown", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    const started: string[] = [];
    registerJobHandler("serialized", {
      run: async ({ job }) => {
        started.push(job.serverId);
        await gate;
      },
    });
    const first = enqueue("serialized");
    const otherId = reconcileServers([
      {
        containerId: dockerId("other"),
        name: "other",
        displayName: "Other",
        gameType: "minecraft",
        mounts: [],
      },
    ]).find((server) => server.containerId === "other")!.id;
    enqueueOperation({
      serverId: otherId,
      actorId: "owner",
      kind: "serialized",
      bindingRevision: 1,
    });
    getDatabase()
      .prepare("UPDATE operations SET created_at=0 WHERE id=?")
      .run(first.id);
    await startOperationRunner();
    for (let count = 0; count < 100 && started.length === 0; count++)
      await new Promise((resolve) => setTimeout(resolve, 2));
    expect(started.length).toBe(1);
    const stopped = stopOperationRunner();
    release();
    await stopped;
    expect(getOperation(first.id)?.status).toBe("succeeded");
    expect(started.length).toBe(1);
  });
});

describe("resource locks", () => {
  it("drains only after every active operation releases its resources", async () => {
    const first = acquireLocks(["server:drain-one"]);
    const second = acquireLocks(["server:drain-two"]);
    let completed = false;
    const draining = waitForLocksReleased().then(() => {
      completed = true;
    });
    first();
    await Promise.resolve();
    expect(completed).toBe(false);
    second();
    await draining;
    expect(completed).toBe(true);
    await waitForLocksReleased();
  });
  it("conflicts on overlapping roots and normalized aliases, but not adjacent names", () => {
    const release = acquireLocks(["path:/srv/world/", "server:one"]);
    try {
      expect(() => acquireLocks(["path:/srv/world/saves"])).toThrow(/conflicting/);
      expect(() => acquireLocks(["path:/srv/other/../world"])).toThrow(/conflicting/);
      const adjacent = acquireLocks(["path:/srv/world-two"]);
      adjacent();
    } finally {
      release();
      release();
    }
    const root = acquireLocks(["path:/"]);
    try {
      expect(() => acquireLocks(["path:/srv/other"])).toThrow(/conflicting/);
    } finally {
      root();
    }
  });
  it("releases all keys after failure and never partially acquires a failed request", async () => {
    const held = acquireLocks(["server:b"]);
    expect(() => acquireLocks(["server:a", "server:b"])).toThrow(/conflicting/);
    const first = acquireLocks(["server:a"]);
    first();
    held();
    await expect(withLocks(["server:a"], async () => {
        throw new Error("copy failed");
      })).rejects.toThrow(/copy failed/);
    const next = acquireLocks(["server:a"]);
    next();
  });
  it("coordinates named volumes with bind mounts to the same physical data", () => {
    const keys = serverLockKeys("one", {
      containerId: dockerId("one"),
      name: "one",
      displayName: "one",
      gameType: "minecraft",
      mounts: [
        {
          type: "volume",
          name: "world",
          source: "/var/lib/docker/volumes/world/_data",
          destination: "/data",
          writable: true,
        },
      ],
    });
    const release = acquireLocks(keys);
    try {
      expect(() => acquireLocks(["path:/var/lib/docker/volumes/world/_data/subdir"])).toThrow(/conflicting/);
      expect(() => acquireLocks(["volume:world"])).toThrow(/conflicting/);
    } finally {
      release();
    }
  });
});
