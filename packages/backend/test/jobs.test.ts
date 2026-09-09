import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "node:test";
import {
  closeDatabase,
  createUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant } from "../src/authorization.js";
import {
  createSchedule,
  deleteSchedule,
  runSchedules,
} from "../src/schedules.js";
import {
  getOperation,
  listOperations,
  startOperationRunner,
  stopOperationRunner,
  type JobContext,
} from "../src/operations.js";
import {
  jobActor,
  registerBackgroundJobs,
  recoverUpdate,
} from "../src/jobs.js";
import { getDockerInstance } from "../src/docker.js";
import { refreshServers } from "../src/servers.js";
import { listLogicalServers } from "../src/identity.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "owner", username: "owner", role: "admin" };
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
const docker = getDockerInstance();
const originalList = docker.listContainers.bind(docker),
  originalGet = docker.getContainer.bind(docker);
let serverId: string,
  mutations = 0;
const input = {
  action: "start" as const,
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: "UTC",
};
beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  mutations = 0;
  for (const actor of [admin, friend])
    createUser({
      ...actor,
      passwordHash: "fake",
      disabled: false,
      createdAt: 0,
    });
  docker.listContainers = (async () => [
    {
      Id: "fixture",
      Names: ["/fixture"],
      Image: "alpine:latest",
      State: "exited",
      Status: "Exited",
      Ports: [],
      Created: 0,
      Labels: { "ludock.enable": "true" },
    },
  ]) as unknown as typeof docker.listContainers;
  docker.getContainer = ((id: string) => ({
    inspect: async () => ({
      Id: id,
      Name: "/fixture",
      Config: { Image: "alpine:latest", Labels: { "ludock.enable": "true" } },
      State: { Status: "exited", Running: false },
      Mounts: [],
      NetworkSettings: { Ports: {} },
      Created: "2026-01-01T00:00:00Z",
    }),
    start: async () => {
      mutations++;
    },
  })) as unknown as typeof docker.getContainer;
  await refreshServers();
  serverId = listLogicalServers()[0].id;
  setServerGrant(
    friend.id,
    serverId,
    ["server.view", "server.start", "schedules.manage"],
    admin,
  );
});
afterEach(async () => {
  await stopOperationRunner();
  docker.listContainers = originalList;
  docker.getContainer = originalGet;
  closeDatabase();
});
function scheduled(): { context: JobContext; scheduleId: string } {
  const schedule = createSchedule(friend, serverId, input);
  runSchedules(Date.parse("2026-09-09T08:00:00Z"));
  const job = getOperation(listOperations(serverId)[0].id)!;
  return { context: { job, progress: () => {} }, scheduleId: schedule.id };
}
describe("queued operation authority", () => {
  it("does not invoke data recovery for an update interrupted while pulling", async () => {
    const { context } = scheduled();
    context.job.recovery = { containerId: "fixture", initiallyRunning: true };
    // No data-operation binding was persisted and the game was never stopped.
    await recoverUpdate(context);
    assert.equal(mutations, 0);
  });
  it("rejects a schedule deleted after its operation was queued", () => {
    const { context, scheduleId } = scheduled();
    assert.equal(jobActor(context).id, friend.id);
    deleteSchedule(friend, serverId, scheduleId);
    assert.throws(() => jobActor(context), /deleted, disabled, or changed/);
  });
  it("rejects a disabled schedule or disabled owner at execution", () => {
    const { context, scheduleId } = scheduled();
    getDatabase()
      .prepare("UPDATE schedules SET input_json=? WHERE id=?")
      .run(JSON.stringify({ ...input, enabled: false }), scheduleId);
    assert.throws(() => jobActor(context), /deleted, disabled, or changed/);
    updateUserAccess(friend.id, "operator", true);
    assert.throws(() => jobActor(context), /no longer has access/);
  });
  it("rejects a schedule whose action changed after it was queued", () => {
    const { context, scheduleId } = scheduled();
    getDatabase()
      .prepare("UPDATE schedules SET input_json=? WHERE id=?")
      .run(JSON.stringify({ ...input, action: "stop" }), scheduleId);
    assert.throws(() => jobActor(context), /deleted, disabled, or changed/);
  });
  it("rechecks action grants before a scheduled job touches Docker", async () => {
    const { context } = scheduled();
    setServerGrant(
      friend.id,
      serverId,
      ["server.view", "schedules.manage"],
      admin,
    );
    registerBackgroundJobs();
    await startOperationRunner();
    for (let attempt = 0; attempt < 100; attempt++) {
      if (getOperation(context.job.id)?.status === "failed") break;
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    assert.equal(getOperation(context.job.id)?.status, "failed");
    assert.equal(mutations, 0);
  });
  for (const change of ["grant revocation", "container rename"] as const)
  it(`rechecks a scheduled action after ${change} during the final lifecycle inspection`, async () => {
    const { context } = scheduled();
    const getContainer = docker.getContainer;
    let dispatchInspected = false;
    docker.getContainer = ((id: string) => {
      const container = getContainer(id);
      const inspect = container.inspect;
      container.inspect = (async () => {
        const info = await inspect.call(container);
        if (getOperation(context.job.id)?.phase === "start") {
          dispatchInspected = true;
          if (change === "grant revocation")
            setServerGrant(
              friend.id, serverId, ["server.view", "schedules.manage"], admin,
            );
          else info.Name = "/different-server";
        }
        return info;
      }) as typeof container.inspect;
      return container;
    }) as typeof docker.getContainer;
    registerBackgroundJobs();
    await startOperationRunner();
    for (let attempt = 0; attempt < 100; attempt++) {
      if (getOperation(context.job.id)?.status === "failed") break;
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    assert.equal(dispatchInspected, true);
    assert.equal(getOperation(context.job.id)?.status, "failed");
    assert.equal(mutations, 0);
  });
});
