import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import { getDockerInstance } from "../src/docker.js";
import { listLogicalServers } from "../src/identity.js";
import { refreshServers } from "../src/servers.js";
import {
  checkAvailability,
  configureAvailability,
  getAvailability,
  setIntentionalStop,
  suppressMonitoring,
} from "../src/monitoring.js";
import { configureNotifications } from "../src/notifications.js";
import { enqueueOperation, stopOperationRunner } from "../src/operations.js";
import { acquireLocks } from "../src/operation-locks.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const docker = getDockerInstance();
const originals = {
  listContainers: docker.listContainers,
  getContainer: docker.getContainer,
};
let serverId: string,
  state: string,
  health: string | undefined,
  unavailable: boolean;
function deliveries() {
  return getDatabase()
    .prepare("SELECT * FROM notification_deliveries ORDER BY created_at")
    .all();
}
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  state = "running";
  health = undefined;
  unavailable = false;
  docker.listContainers = (async () => {
    if (unavailable) throw new Error("Docker socket unreachable");
    return [
      {
        Id: "container",
        Names: ["/world"],
        Image: "itzg/minecraft-server",
        Labels: {},
      },
    ];
  }) as unknown as typeof docker.listContainers;
  docker.getContainer = (() => ({
    inspect: async () => ({
      Id: "container",
      Name: "/world",
      Config: { Image: "itzg/minecraft-server", Labels: {} },
      State: {
        Status: state,
        ...(health ? { Health: { Status: health } } : {}),
      },
      Mounts: [],
      NetworkSettings: { Ports: {} },
      Created: "2026-01-01T00:00:00Z",
    }),
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  configureNotifications(
    true,
    "https://discord.com/api/webhooks/123456/fake-test-secret",
  );
  configureAvailability(serverId, {
    enabled: true,
    maintenance: false,
    graceSeconds: 10,
  });
});
afterEach(async () => {
  await stopOperationRunner();
  Object.assign(docker, originals);
  closeDatabase();
});

describe("availability monitoring", () => {
  it("emits one outage after grace and one recovery", async () => {
    state = "exited";
    await checkAvailability(1000);
    await checkAvailability(10_999);
    assert.equal(deliveries().length, 0);
    await checkAvailability(11_000);
    await checkAvailability(30_000);
    assert.equal(deliveries().length, 1);
    state = "running";
    await checkAvailability(31_000);
    await checkAvailability(32_000);
    assert.equal(deliveries().length, 2);
    assert.equal(getAvailability(serverId).state.outageStartedAt, null);
  });
  it("suppresses intentional stops until the server has actually been observed running", async () => {
    setIntentionalStop(serverId, true);
    state = "exited";
    await checkAvailability(1000);
    await checkAvailability(30_000);
    assert.equal(deliveries().length, 0);
    state = "running";
    await checkAvailability(40_000);
    assert.equal(getAvailability(serverId).state.intentionallyStopped, false);
    state = "exited";
    await checkAvailability(50_000);
    await checkAvailability(60_000);
    assert.equal(deliveries().length, 1);
  });
  it("suppresses operation downtime plus the configured completion grace", async () => {
    const operation = enqueueOperation({
      serverId,
      actorId: "api-token",
      kind: "backup",
      bindingRevision: 1,
    });
    state = "exited";
    const now = Date.now();
    await checkAvailability(now);
    await checkAvailability(now + 100_000);
    assert.equal(deliveries().length, 0);
    getDatabase()
      .prepare("UPDATE operations SET status='succeeded' WHERE id=?")
      .run(operation.id);
    suppressMonitoring(serverId);
    const end = getAvailability(serverId).state.suppressedUntil;
    await checkAvailability(end - 1);
    assert.equal(deliveries().length, 0);
    await checkAvailability(end);
    await checkAvailability(end + 10_000);
    assert.equal(deliveries().length, 1);
  });
  it("suppresses a direct lifecycle action throughout its held lock", async () => {
    const release = acquireLocks([`server:${serverId}`]);
    state = "exited";
    try {
      await checkAvailability(1000);
      await checkAvailability(121_000);
      assert.equal(deliveries().length, 0);
      assert.equal(getAvailability(serverId).state.outageStartedAt, null);
    } finally {
      release();
    }
    await checkAvailability(122_000);
    await checkAvailability(132_000);
    assert.equal(deliveries().length, 1);
  });
  it("does not treat Docker health starting as ready, and respects maintenance", async () => {
    health = "starting";
    await checkAvailability(1000);
    await checkAvailability(11_000);
    assert.equal(deliveries().length, 1);
    health = "healthy";
    await checkAvailability(12_000);
    assert.equal(deliveries().length, 2);
    configureAvailability(serverId, {
      enabled: true,
      maintenance: true,
      graceSeconds: 10,
    });
    health = "unhealthy";
    await checkAvailability(20_000);
    await checkAvailability(40_000);
    assert.equal(deliveries().length, 2);
  });
  it("reports lost Docker connectivity without deleting or rebinding servers", async () => {
    unavailable = true;
    await checkAvailability(1000);
    await checkAvailability(11_000);
    assert.equal(deliveries().length, 1);
    assert.match(deliveries()[0].payload_json as string, /cannot reach Docker/);
    assert.equal(listLogicalServers()[0].status, "active");
    assert.equal(
      getAvailability(serverId).state.lastState,
      "docker_unavailable",
    );
    unavailable = false;
    await checkAvailability(12_000);
    assert.equal(deliveries().length, 2);
  });
});
