import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { afterEach, beforeEach, describe, it } from "node:test";
import { mountsOverlap, stopForDataOperation } from "../src/backups.js";
import type { ServerObservation } from "../src/identity.js";
import { listLogicalServers } from "../src/identity.js";
import { setServerGrant } from "../src/authorization.js";
import {
  closeDatabase,
  createUser,
  updateUserAccess,
} from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import type { JobContext } from "../src/operations.js";
import {
  refreshServers,
  resolveAuthorizedServer,
  type ServerContext,
} from "../src/servers.js";

const bind = (source: string): ServerObservation["mounts"][number] => ({
  type: "bind",
  source,
  destination: "/data",
  writable: true,
});

describe("backup shared-writer boundaries", () => {
  it("recognizes host root and normalized parent mounts in both directions", () => {
    for (const [left, right] of [
      ["/", "/srv/games/world"],
      ["/srv/games/", "/srv/games/world"],
      ["/srv//games/./", "/srv/games/world"],
      ["/srv/games/world", "/srv/games/world"],
    ]) {
      assert.equal(
        mountsOverlap(bind(left), bind(right)), true,
        `${left} contains ${right}`,
      );
      assert.equal(
        mountsOverlap(bind(right), bind(left)), true,
        `${right} overlaps ${left}`,
      );
    }
  });

  it("keeps sibling paths and distinct named volumes separate", () => {
    assert.equal(mountsOverlap(bind("/srv/game"), bind("/srv/games")), false);
    assert.equal(mountsOverlap(bind(""), bind("/srv/games")), false);
    const volume = {
      ...bind("/var/lib/docker/volumes/world/_data"),
      type: "volume", name: "world",
    };
    assert.equal(
      mountsOverlap(volume, { ...volume, destination: "/other-data" }), true,
    );
    assert.equal(
      mountsOverlap(volume, { ...volume, name: "other-world" }), false,
    );
    assert.equal(mountsOverlap(volume, bind("/var/lib/docker/volumes")), true);
  });
});

describe("backup execution authority", () => {
  const admin = { id: "admin", username: "admin", role: "admin" as const };
  const operator = {
    id: "operator", username: "operator", role: "operator" as const,
  };
  const docker = getDockerInstance();
  const originalList = docker.listContainers;
  const originalGet = docker.getContainer;
  let context: ServerContext;
  let job: JobContext;
  let stopped: number;
  let running: boolean;
  let onInspect: () => void;
  beforeEach(async () => {
    process.env.LUDOCK_DB_PATH = ":memory:";
    closeDatabase();
    stopped = 0;
    running = true;
    onInspect = () => {};
    for (const actor of [admin, operator])
      createUser({ ...actor, passwordHash: "fake", disabled: false, createdAt: 0 });
    const labels = { "ludock.enable": "true" };
    docker.listContainers = (async () => [{
      Id: "fixture", Names: ["/fixture"], Image: "alpine:latest",
      State: "running", Status: "Up", Ports: [], Created: 0, Labels: labels,
    }]) as unknown as typeof docker.listContainers;
    docker.getContainer = ((id: string) => ({
      inspect: async () => {
        onInspect();
        return {
          Id: id, Name: "/fixture",
          Config: { Image: "alpine:latest", Labels: labels },
          State: { Status: running ? "running" : "exited", Running: running },
          Mounts: [], NetworkSettings: { Ports: {} },
          Created: "2026-01-01T00:00:00Z",
        };
      },
      stop: async () => { stopped++; running = false; },
    })) as unknown as typeof docker.getContainer;
    await refreshServers();
    const serverId = listLogicalServers()[0].id;
    setServerGrant(operator.id, serverId, ["server.view", "backups.create"], admin);
    context = await resolveAuthorizedServer(operator, serverId, "backups.create");
    const saved: JobContext["job"] = {
      id: randomUUID(), serverId, actorId: operator.id, kind: "backup",
      status: "running", phase: "validating", input: {}, recovery: {},
      bindingRevision: context.logical.bindingRevision,
      createdAt: 0, updatedAt: 0, error: null, result: null,
    };
    job = {
      job: saved,
      progress: (phase, patch) => {
        saved.phase = phase;
        Object.assign(saved.recovery, patch);
      },
    };
  });
  afterEach(() => {
    docker.listContainers = originalList;
    docker.getContainer = originalGet;
    closeDatabase();
  });
  it("rechecks the backup grant after Docker inspection before stopping", async () => {
    onInspect = () => setServerGrant(
      operator.id, context.logical.id, ["server.view"], admin,
    );
    await assert.rejects(stopForDataOperation(context, job), /permission/);
    assert.equal(stopped, 0);
    assert.deepEqual(job.job.recovery, {});
  });
  it("rejects an owner disabled during Docker inspection", async () => {
    onInspect = () => updateUserAccess(operator.id, "operator", true);
    await assert.rejects(stopForDataOperation(context, job), /no longer has access/);
    assert.equal(stopped, 0);
  });
  it("records and stops for an authorized backup without requiring a lifecycle grant", async () => {
    await stopForDataOperation(context, job);
    assert.equal(stopped, 1);
    assert.equal(job.job.recovery.initialRunning, true);
  });
});
