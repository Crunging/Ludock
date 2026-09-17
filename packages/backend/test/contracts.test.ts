import { expect, test } from "bun:test";
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
  composeSourceProjectSchema,
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
  expect(integrations.length).toBe(raw.length);
  expect(integrations.map((value) => value.gameType)).toStrictEqual(raw.map((value) => value.gameType));
  expect(integrations[0]?.capabilities).toStrictEqual(raw[0]?.capabilities);
  expect(integrations.every(
      (value) =>
        !Object.hasOwn(value, "console") && !Object.hasOwn(value, "aliases"),
    )).toBeTruthy();
});

test("request defaults and required persisted response fields remain distinct", () => {
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
  expect(savedScheduleSchema.safeParse(saved).success).toBeTruthy();
  expect(!savedScheduleSchema.safeParse({ ...saved, ownerId: undefined }).success).toBeTruthy();
  expect(!savedScheduleSchema.safeParse({ ...saved, enabled: undefined }).success).toBeTruthy();
  const policy = availabilitySchema.parse({ enabled: false });
  const state = {
    outageStartedAt: null,
    notified: false,
    suppressedUntil: 0,
    intentionallyStopped: false,
    lastState: null,
  };
  expect(availabilityResponseSchema.safeParse({ policy, state }).success).toBeTruthy();
  expect(!availabilityResponseSchema.safeParse({
      policy: { enabled: false },
      state,
    }).success).toBeTruthy();
  const project = composeSourceProjectSchema.parse({
    projectName: "games", projectDirectory: "/compose/games",
    composeFiles: ["/compose/games/compose.yaml"],
  });
  expect(project.envFiles).toStrictEqual([]);
  expect(!composeSourceProjectSchema.safeParse({ ...project, composeFiles: [] }).success).toBeTruthy();
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
  expect(operationSchema.safeParse(operation).success).toBeTruthy();
  for (const changes of [
    { status: "done" },
    { serverId: "minecraft" },
    { phase: undefined },
    { updatedAt: "2" },
  ]) {
    expect(!operationSchema.safeParse({ ...operation, ...changes }).success).toBeTruthy();
  }
  expect(!serverGrantsResponseSchema.safeParse({
      grants: [{ serverId, capabilities: ["server.everything"], updatedAt: 1 }],
    }).success).toBeTruthy();
});

test("events distinguish logical server updates from content-free invalidation", () => {
  const update = {
    type: "container_event",
    action: "start",
    serverId,
    time: 1,
  };
  expect(serverEventSchema.parse(update)).toStrictEqual(update);
  expect(!serverEventSchema.safeParse({
      ...update,
      serverId: undefined,
      containerId: "minecraft",
    }).success).toBeTruthy();
  expect(!serverEventSchema.safeParse({ ...update, serverId: "minecraft" }).success).toBeTruthy();
  expect(!serverEventSchema.safeParse({ ...update, action: "exec_start" }).success).toBeTruthy();
  expect(serverEventSchema.parse({
      type: "container_event",
      action: "refresh",
      time: 1,
      containerId: "private",
    })).toStrictEqual({ type: "container_event", action: "refresh", time: 1 });
});

test("identifier boundary parsers preserve supported Docker references and reject paths", () => {
  expect(logicalServerIdSchema.parse(serverId)).toBe(serverId);
  expect(!logicalServerIdSchema.safeParse("minecraft").success).toBeTruthy();
  for (const reference of [
    "a".repeat(64),
    "123456789abc",
    "minecraft-server.1",
  ]) {
    expect(dockerContainerIdSchema.parse(reference)).toBe(reference);
  }
  for (const reference of [
    "../server",
    "/containers/server",
    "server/name",
    "",
    "a".repeat(129),
  ]) {
    expect(!dockerContainerIdSchema.safeParse(reference).success).toBeTruthy();
  }
});

test("shared account requests normalize input, validate password changes, and strip unknown fields", () => {
  const password = "long-enough-password";
  const bootstrapCode = "fixture-setup-code-0123456789abcdef";
  expect(credentialsRequestSchema.parse({
      username: "  admin  ",
      password,
      ignored: true,
    })).toStrictEqual({ username: "admin", password });
  expect(setupRequestSchema.parse({
      username: "  admin  ",
      password,
      bootstrapCode,
    })).toStrictEqual({ username: "admin", password, bootstrapCode });
  expect(!setupRequestSchema.safeParse({ username: "admin", password }).success).toBeTruthy();
  expect(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode: "x".repeat(31),
  }).success).toBeTruthy();
  expect(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode: "x".repeat(129),
  }).success).toBeTruthy();
  expect(!setupRequestSchema.safeParse({
    username: "admin",
    password,
    bootstrapCode,
    ignored: true,
  }).success).toBeTruthy();
  expect(createUserRequestSchema.parse({
      username: "friend",
      password,
      role: "operator",
      ignored: true,
    })).toStrictEqual({ username: "friend", password, role: "operator" });
  expect(userAccessRequestSchema.parse({
      role: "viewer",
      disabled: false,
      ignored: true,
    })).toStrictEqual({ role: "viewer", disabled: false });
  expect(changePasswordRequestSchema.safeParse({
      currentPassword: "a",
      newPassword: password,
    }).success).toBeTruthy();
  expect(!resetPasswordRequestSchema.safeParse({ password: "too-short" }).success).toBeTruthy();
  expect(!resetPasswordRequestSchema.safeParse({ password: "a".repeat(129) })
      .success).toBeTruthy();
});

test("shared file requests retain root-relative defaults and reject oversized names before storage access", () => {
  expect(fileLocationSchema.parse({ root: "data", ignored: true })).toStrictEqual({
    root: "data",
    path: "",
  });
  expect(createDirectoryRequestSchema.parse({
      root: "data",
      name: "worlds",
      ignored: true,
    })).toStrictEqual({ root: "data", path: "", name: "worlds" });
  expect(renameFileRequestSchema.parse({
      root: "data",
      path: "worlds/old",
      newName: "new",
    })).toStrictEqual({ root: "data", path: "worlds/old", newName: "new" });
  expect(uploadFileQuerySchema.parse({ root: "data", name: "world.zip" })).toStrictEqual({ root: "data", path: "", name: "world.zip" });
  expect(!createDirectoryRequestSchema.safeParse({
      root: "data",
      name: "a".repeat(256),
    }).success).toBeTruthy();
  expect(!fileLocationSchema.safeParse({ root: "data", path: "a".repeat(2049) })
      .success).toBeTruthy();
});
