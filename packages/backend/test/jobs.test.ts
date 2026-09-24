import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { listOperationHistory } from "../src/history.js";
import {
  closeDatabase,
  createUser,
  getDatabase,
  updateUserAccess,
  type SessionUser,
} from "../src/database.js";
import { setServerGrant } from "./fixtures/grants.js";
import {
  createSchedule,
  deleteSchedule,
  runSchedules,
  setScheduleEnabled,
  updateSchedule,
} from "../src/schedules.js";
import {
  getOperation,
  startOperationRunner,
  stopOperationRunner,
  type JobContext,
} from "../src/operations.js";
import {
  jobActor,
  registerBackgroundJobs,
  recoverUpdate,
} from "../src/jobs.js";
import { docker } from "../src/docker-client.js";
import { DockerApiError } from "../src/docker-transport.js";
import { getAvailability } from "../src/monitoring.js";
import { refreshServers } from "../src/servers.js";
import { listLogicalServers } from "../src/identity.js";

process.env.LUDOCK_DB_PATH = ":memory:";
const admin: SessionUser = { id: "owner", username: "owner", role: "admin" };
const friend: SessionUser = {
  id: "friend",
  username: "friend",
  role: "operator",
};
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
  const job = getOperation(listOperationHistory(admin, { serverId, limit: 50 }).operations[0].id)!;
  return { context: { job, progress: () => {} }, scheduleId: schedule.id };
}
describe("queued operation authority", () => {
  for (const action of ["start", "stop"] as const) {
    for (const statusCode of [304, 500]) {
      it(`records scheduled ${action} correctly after Docker returns ${statusCode}`, async () => {
        setServerGrant(friend.id, serverId,
          ["server.view", `server.${action}`, "schedules.manage"], admin);
        createSchedule(friend, serverId, { ...input, action });
        runSchedules(Date.parse("2026-09-09T08:00:00Z"));
        const operation = listOperationHistory(admin, { serverId, limit: 50 }).operations[0];
        const getContainer = docker.getContainer;
        docker.getContainer = ((id: string) => {
          const container = getContainer(id);
          container[action] = async () => {
            mutations++;
            throw new DockerApiError(statusCode);
          };
          return container;
        }) as typeof docker.getContainer;
        registerBackgroundJobs();
        await startOperationRunner();
        for (let attempt = 0; attempt < 100; attempt++) {
          if (["succeeded", "failed"].includes(getOperation(operation.id)!.status)) break;
          await new Promise((resolve) => setTimeout(resolve, 5));
        }
        expect(getOperation(operation.id)?.status).toBe(statusCode === 304 ? "succeeded" : "failed");
        expect(mutations).toBe(1);
        expect(getAvailability(serverId).state.intentionallyStopped).toBe(action === "stop" && statusCode === 304);
      });
    }
  }

  it("does not invoke data recovery for an update interrupted while pulling", async () => {
    const { context } = scheduled();
    context.job.recovery = { containerId: "fixture", initiallyRunning: true };
    // No data-operation binding was persisted and the game was never stopped.
    await recoverUpdate(context);
    expect(mutations).toBe(0);
  });
  it("rejects a schedule deleted after its operation was queued", () => {
    const { context, scheduleId } = scheduled();
    expect(jobActor(context).id).toBe(friend.id);
    deleteSchedule(friend, serverId, scheduleId);
    expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
  });
  it("rejects a disabled schedule or disabled owner at execution", () => {
    const { context, scheduleId } = scheduled();
    getDatabase()
      .prepare("UPDATE schedules SET input_json=? WHERE id=?")
      .run(JSON.stringify({ ...input, enabled: false }), scheduleId);
    expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
    updateUserAccess(friend.id, "operator", true);
    expect(() => jobActor(context)).toThrow(/no longer has access/);
  });
  it("rejects a schedule whose action changed after it was queued", () => {
    const { context, scheduleId } = scheduled();
    getDatabase()
      .prepare("UPDATE schedules SET input_json=? WHERE id=?")
      .run(JSON.stringify({ ...input, action: "stop" }), scheduleId);
    expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
  });
  it("rejects queued work after editing the schedule time without changing its action", () => {
    const { context, scheduleId } = scheduled();
    updateSchedule(friend, serverId, scheduleId, { ...input, time: "09:00", revision: 1 });
    expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
  });
  it("does not revive old queued work after pausing and resuming a schedule", () => {
    const { context, scheduleId } = scheduled();
    setScheduleEnabled(friend, serverId, scheduleId, { enabled: false, revision: 1 });
    setScheduleEnabled(friend, serverId, scheduleId, { enabled: true, revision: 2 });
    expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
  });
  it("rejects invalid or future schedule generations", () => {
    const { context } = scheduled();
    for (const revision of [undefined, null, "1", 0, 2]) {
      context.job.input.scheduleRevision = revision;
      expect(() => jobActor(context)).toThrow(/deleted, disabled, or changed/);
    }
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
    expect(getOperation(context.job.id)?.status).toBe("failed");
    expect(mutations).toBe(0);
  });
  for (const change of ["grant revocation", "container rename", "schedule pause and resume"] as const)
  it(`rechecks a scheduled action after ${change} during the final lifecycle inspection`, async () => {
    const { context, scheduleId } = scheduled();
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
          else if (change === "container rename") info.Name = "/different-server";
          else {
            setScheduleEnabled(friend, serverId, scheduleId, { enabled: false, revision: 1 });
            setScheduleEnabled(friend, serverId, scheduleId, { enabled: true, revision: 2 });
          }
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
    expect(dispatchInspected).toBe(true);
    expect(getOperation(context.job.id)?.status).toBe("failed");
    expect(mutations).toBe(0);
  });
});
