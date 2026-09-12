import assert from "node:assert/strict";
import { test } from "bun:test";
import {
  credentialsRequestSchema,
  setupRequestSchema,
  createUserRequestSchema,
  userAccessRequestSchema,
  resetPasswordRequestSchema,
  changePasswordRequestSchema,
  fileLocationSchema,
  createDirectoryRequestSchema,
  renameFileRequestSchema,
  uploadFileQuerySchema,
  availabilitySchema,
  availabilityResponseSchema,
  composeProjectResponseSchema,
  composeRegistrationSchema,
  dockerContainerIdSchema,
  integrationsResponseSchema,
  logicalServerIdSchema,
  operationSchema,
  savedScheduleSchema,
  scheduleSchema,
  serverEventSchema,
  serverGrantsResponseSchema,
} from "@ludock/shared";
import { getGameCapabilityMatrix } from "../src/server-presets.js";

const serverId = "5b9dfbf4-59b0-4ae8-ad5f-4c755f7c02de";
const ownerId = "eaa9292a-d218-4696-8c86-f3ebd176dbd4";

test("game capability response projects the actual integration registry without adapter configuration", () => {
  const raw = getGameCapabilityMatrix();
  const { integrations } = integrationsResponseSchema.parse({
    integrations: raw,
  });
  assert.equal(integrations.length, raw.length);
  assert.deepEqual(
    integrations.map((value) => value.gameType),
    raw.map((value) => value.gameType),
  );
  assert.deepEqual(integrations[0]?.capabilities, raw[0]?.capabilities);
  assert(
    integrations.every(
      (value) =>
        !Object.hasOwn(value, "console") && !Object.hasOwn(value, "aliases"),
    ),
  );
});

test("request defaults do not hide missing persisted schedule, availability, or Compose response fields", () => {
  const input = {
    action: "start",
    time: "08:00",
    days: [1],
    timezone: "UTC",
  };
  const saved = {
    ...scheduleSchema.parse(input),
    id: serverId,
    serverId,
    ownerId,
    lastResult: null,
    lastOperation: null,
    lastRunAt: null,
    lastSlot: null,
    revision: 1,
    nextRunAt: null,
    nextRunUnavailableReason: null,
  };
  assert(savedScheduleSchema.safeParse(saved).success);
  assert(
    !savedScheduleSchema.safeParse({ ...saved, ownerId: undefined }).success,
  );
  assert(
    !savedScheduleSchema.safeParse({ ...saved, enabled: undefined }).success,
  );
  const policy = availabilitySchema.parse({ enabled: false });
  const state = {
    outageStartedAt: null,
    notified: false,
    suppressedUntil: 0,
    intentionallyStopped: false,
    lastState: null,
  };
  assert(availabilityResponseSchema.safeParse({ policy, state }).success);
  assert(
    !availabilityResponseSchema.safeParse({
      policy: { enabled: false },
      state,
    }).success,
  );
  const project = {
    ...composeRegistrationSchema.parse({
      projectName: "games",
      projectDirectory: "/compose/games",
      composeFiles: ["/compose/games/compose.yaml"],
    }),
    id: serverId,
    disabled: false,
  };
  assert(composeProjectResponseSchema.safeParse({ project }).success);
  assert(
    !composeProjectResponseSchema.safeParse({
      project: { ...project, envFiles: undefined },
    }).success,
  );
});

test("persisted operation response rejects unknown states and Docker references in the logical server field", () => {
  const operation = {
    id: ownerId,
    serverId,
    kind: "backup",
    status: "running",
    phase: "copy",
    createdAt: 1,
    updatedAt: 2,
    error: null,
    result: null,
  };
  assert(operationSchema.safeParse(operation).success);
  for (const changes of [
    { status: "done" },
    { serverId: "minecraft" },
    { phase: undefined },
    { updatedAt: "2" },
  ]) {
    assert(!operationSchema.safeParse({ ...operation, ...changes }).success);
  }
  assert(
    !serverGrantsResponseSchema.safeParse({
      grants: [{ serverId, capabilities: ["server.everything"], updatedAt: 1 }],
    }).success,
  );
});

test("events distinguish logical server updates from content-free invalidation", () => {
  const update = {
    type: "container_event",
    action: "start",
    serverId,
    time: 1,
  };
  assert.deepEqual(serverEventSchema.parse(update), update);
  assert(
    !serverEventSchema.safeParse({
      ...update,
      serverId: undefined,
      containerId: "minecraft",
    }).success,
  );
  assert(
    !serverEventSchema.safeParse({ ...update, serverId: "minecraft" }).success,
  );
  assert(
    !serverEventSchema.safeParse({ ...update, action: "exec_start" }).success,
  );
  assert.deepEqual(
    serverEventSchema.parse({
      type: "container_event",
      action: "refresh",
      time: 1,
      containerId: "private",
    }),
    { type: "container_event", action: "refresh", time: 1 },
  );
});

test("identifier boundary parsers preserve supported Docker references and reject paths", () => {
  assert.equal(logicalServerIdSchema.parse(serverId), serverId);
  assert(!logicalServerIdSchema.safeParse("minecraft").success);
  for (const reference of [
    "a".repeat(64),
    "123456789abc",
    "minecraft-server.1",
  ]) {
    assert.equal(dockerContainerIdSchema.parse(reference), reference);
  }
  for (const reference of [
    "../server",
    "/containers/server",
    "server/name",
    "",
    "a".repeat(129),
  ]) {
    assert(!dockerContainerIdSchema.safeParse(reference).success);
  }
});

test("shared account requests normalize input, validate password changes, and strip unknown fields", () => {
  const password = "long-enough-password";
  const bootstrapCode = "fixture-setup-code-0123456789abcdef";
  assert.deepEqual(
    credentialsRequestSchema.parse({
      username: "  admin  ",
      password,
      ignored: true,
    }),
    { username: "admin", password },
  );
  assert.deepEqual(
    setupRequestSchema.parse({
      username: "  admin  ",
      password,
      bootstrapCode,
    }),
    { username: "admin", password, bootstrapCode },
  );
  assert(!setupRequestSchema.safeParse({ username: "admin", password }).success);
  assert(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode: "x".repeat(31),
  }).success);
  assert(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode: "x".repeat(129),
  }).success);
  assert(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode,
    ignored: true,
  }).success);
  assert.deepEqual(
    createUserRequestSchema.parse({
      username: "friend",
      password,
      role: "operator",
      ignored: true,
    }),
    { username: "friend", password, role: "operator" },
  );
  assert.deepEqual(
    userAccessRequestSchema.parse({
      role: "viewer",
      disabled: false,
      ignored: true,
    }),
    { role: "viewer", disabled: false },
  );
  assert(
    changePasswordRequestSchema.safeParse({
      currentPassword: "a",
      newPassword: password,
    }).success,
  );
  assert(
    !resetPasswordRequestSchema.safeParse({ password: "too-short" }).success,
  );
  assert(
    !resetPasswordRequestSchema.safeParse({ password: "a".repeat(129) })
      .success,
  );
});

test("shared file requests retain root-relative defaults and reject oversized names before storage access", () => {
  assert.deepEqual(fileLocationSchema.parse({ root: "data", ignored: true }), {
    root: "data",
    path: "",
  });
  assert.deepEqual(
    createDirectoryRequestSchema.parse({
      root: "data",
      name: "worlds",
      ignored: true,
    }),
    { root: "data", path: "", name: "worlds" },
  );
  assert.deepEqual(
    renameFileRequestSchema.parse({
      root: "data",
      path: "worlds/old",
      newName: "new",
    }),
    { root: "data", path: "worlds/old", newName: "new" },
  );
  assert.deepEqual(
    uploadFileQuerySchema.parse({ root: "data", name: "world.zip" }),
    { root: "data", path: "", name: "world.zip" },
  );
  assert(
    !createDirectoryRequestSchema.safeParse({
      root: "data",
      name: "a".repeat(256),
    }).success,
  );
  assert(
    !fileLocationSchema.safeParse({ root: "data", path: "a".repeat(2049) })
      .success,
  );
});
