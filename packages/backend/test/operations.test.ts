import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import {
  closeDatabase,
  createUser,
  deleteUser,
  getDatabase,
  listAuditLog,
} from "../src/database.js";
import { reconcileServers } from "../src/identity.js";
import {
  enqueueOperation,
  getOperation,
  listOperations,
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

process.env.LUDOCK_DB_PATH = ":memory:";
let serverId: string;
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  createUser({
    id: "owner",
    username: "owner",
    role: "admin",
    passwordHash: "fake",
    disabled: false,
    createdAt: 0,
  });
  serverId = reconcileServers([
    {
      containerId: "container",
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
  assert.fail("Operation did not finish");
}

describe("durable operations", () => {
  it("deduplicates retries while rejecting a reused key with different settings", () => {
    const first = enqueue("idempotency", "same", { createBackup: true });
    const retry = enqueue("idempotency", "same", { createBackup: true });
    assert.equal(retry.id, first.id);
    assert.throws(
      () => enqueue("idempotency", "same", { createBackup: false }),
      /different operation settings/,
    );
    assert.throws(
      () => enqueue("idempotency", "other"),
      /already queued or running/,
    );
    assert.equal(listOperations(serverId).length, 1);
  });
  it("persists recovery data before the handler continues, without exposing it publicly", async () => {
    registerJobHandler("progress", {
      run: async ({ job, progress }) => {
        progress("copying", {
          initiallyRunning: true,
          privatePath: "/private/secret",
        });
        const saved = getOperation(job.id)!;
        assert.equal(saved.phase, "copying");
        assert.equal(saved.recovery.initiallyRunning, true);
        assert.equal(
          JSON.stringify(listOperations(serverId)).includes("/private/secret"),
          false,
        );
        return { copied: true };
      },
    });
    const operation = enqueue("progress");
    await startOperationRunner();
    assert.equal((await finished(operation.id)).status, "succeeded");
    const audit = listAuditLog(20).find(
      (event) => event.action === "server.progress.succeeded",
    );
    assert.equal(audit?.username, "owner");
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
        assert.equal(job.phase, "replacing_data");
        assert.equal(job.recovery.initiallyRunning, true);
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
    assert.equal(getOperation(operation.id)?.status, "interrupted");
    assert.equal(recoveries, 1);
    assert.equal(runs, 0);
    await stopOperationRunner();
    await startOperationRunner();
    assert.equal(recoveries, 1);
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
    assert.match(getOperation(operation.id)!.error!, /administrator attention/);
    assert.equal(
      JSON.stringify(listAuditLog(10)).includes("token=secret"),
      false,
    );
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
    assert.equal(recoveries, 1);
    let stopped = false;
    const stopping = stopOperationRunner().then(() => {
      stopped = true;
    });
    await Promise.resolve();
    assert.equal(stopped, false);
    release();
    await Promise.all([starting, sameStart, stopping]);
    assert.equal(getOperation(interrupted.id)?.status, "interrupted");
    const queued = enqueue("startup-shutdown");
    await new Promise((resolve) => setTimeout(resolve, 10));
    assert.equal(getOperation(queued.id)?.status, "queued");
    assert.equal(runs, 0);
    await startOperationRunner();
    assert.equal((await finished(queued.id)).status, "succeeded");
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
    assert.equal((await finished(operation.id)).status, "failed");
    assert.ok(
      listAuditLog(20).some(
        (event) => event.action === "server.deleted-owner.failed",
      ),
    );
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
        containerId: "other",
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
    assert.equal(started.length, 1);
    const stopped = stopOperationRunner();
    release();
    await stopped;
    assert.equal(getOperation(first.id)?.status, "succeeded");
    assert.equal(started.length, 1);
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
    assert.equal(completed, false);
    second();
    await draining;
    assert.equal(completed, true);
    await waitForLocksReleased();
  });
  it("conflicts on overlapping roots and normalized aliases, but not adjacent names", () => {
    const release = acquireLocks(["path:/srv/world/", "server:one"]);
    try {
      assert.throws(
        () => acquireLocks(["path:/srv/world/saves"]),
        /conflicting/,
      );
      assert.throws(
        () => acquireLocks(["path:/srv/other/../world"]),
        /conflicting/,
      );
      const adjacent = acquireLocks(["path:/srv/world-two"]);
      adjacent();
    } finally {
      release();
      release();
    }
    const root = acquireLocks(["path:/"]);
    try {
      assert.throws(() => acquireLocks(["path:/srv/other"]), /conflicting/);
    } finally {
      root();
    }
  });
  it("releases all keys after failure and never partially acquires a failed request", async () => {
    const held = acquireLocks(["server:b"]);
    assert.throws(() => acquireLocks(["server:a", "server:b"]), /conflicting/);
    const first = acquireLocks(["server:a"]);
    first();
    held();
    await assert.rejects(
      withLocks(["server:a"], async () => {
        throw new Error("copy failed");
      }),
      /copy failed/,
    );
    const next = acquireLocks(["server:a"]);
    next();
  });
  it("coordinates named volumes with bind mounts to the same physical data", () => {
    const keys = serverLockKeys("one", {
      containerId: "one",
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
      assert.throws(
        () => acquireLocks(["path:/var/lib/docker/volumes/world/_data/subdir"]),
        /conflicting/,
      );
      assert.throws(() => acquireLocks(["volume:world"]), /conflicting/);
    } finally {
      release();
    }
  });
});
