import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { closeDatabase, getDatabase } from "../src/database.js";
import { docker } from "../src/docker-client.js";
import { listLogicalServers } from "../src/identity.js";
import { refreshServers } from "../src/servers.js";
import {
  checkAvailability as evaluateAvailability,
  configureAvailability,
  getAvailability,
  setIntentionalStop,
} from "../src/monitoring.js";
import { configureNotifications } from "../src/notifications.js";
import { stopOperationRunner } from "../src/operations.js";
import { acquireLocks } from "../src/operation-locks.js";
import type { SQLQueryBindings } from "bun:sqlite";

process.env.LUDOCK_DB_PATH = ":memory:";
const originals = {
  listContainers: docker.listContainers,
  getContainer: docker.getContainer,
};
let serverId: string,
  state: string,
  health: string | null,
  unavailable: boolean;
function deliveries() {
  return getDatabase()
    .prepare<Record<string, unknown>, SQLQueryBindings[]>("SELECT * FROM notification_deliveries ORDER BY created_at")
    .all();
}
async function checkAvailability(now: number) {
  evaluateAvailability(await refreshServers().catch(() => null), now);
}
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  state = "running";
  health = null;
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
    expect(deliveries().length).toBe(0);
    await checkAvailability(11_000);
    await checkAvailability(30_000);
    expect(deliveries().length).toBe(1);
    state = "running";
    await checkAvailability(31_000);
    await checkAvailability(32_000);
    expect(deliveries().length).toBe(2);
    expect(getAvailability(serverId).state.outageStartedAt).toBe(null);
  });

  it("treats a running server with a failing or starting health check as unavailable", async () => {
    health = "unhealthy";
    await checkAvailability(1000);
    await checkAvailability(11_000);
    expect(deliveries().length).toBe(1);
    health = "starting";
    await checkAvailability(12_000);
    expect(deliveries().length).toBe(1);
    health = "healthy";
    await checkAvailability(13_000);
    expect(deliveries().length).toBe(2);
  });

  it("suppresses intentional stops until the server has actually been observed running", async () => {
    setIntentionalStop(serverId, true);
    state = "exited";
    await checkAvailability(1000);
    await checkAvailability(30_000);
    expect(deliveries().length).toBe(0);
    state = "running";
    await checkAvailability(40_000);
    expect(getAvailability(serverId).state.intentionallyStopped).toBe(false);
    state = "exited";
    await checkAvailability(50_000);
    await checkAvailability(60_000);
    expect(deliveries().length).toBe(1);
  });

  it("suppresses a direct lifecycle action throughout its held lock", async () => {
    const release = acquireLocks([`server:${serverId}`]);
    state = "exited";
    try {
      await checkAvailability(1000);
      await checkAvailability(121_000);
      expect(deliveries().length).toBe(0);
      expect(getAvailability(serverId).state.outageStartedAt).toBe(null);
    } finally {
      release();
    }
    await checkAvailability(122_000);
    await checkAvailability(132_000);
    expect(deliveries().length).toBe(1);
  });

  it("reports lost Docker connectivity without deleting or rebinding servers", async () => {
    unavailable = true;
    await checkAvailability(1000);
    await checkAvailability(11_000);
    expect(deliveries().length).toBe(1);
    expect(deliveries()[0].payload_json as string).toMatch(/cannot reach Docker/);
    expect(listLogicalServers()[0].status).toBe("active");
    expect(getAvailability(serverId).state.lastState).toBe("docker_unavailable");
    unavailable = false;
    await checkAvailability(12_000);
    expect(deliveries().length).toBe(2);
  });
});
