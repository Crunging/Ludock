import type {
  AvailabilityPolicy,
  AvailabilityState,
  Backup,
  Operation,
  Schedule,
  Server,
  UpdateCapability,
} from "@ludock/shared";

export function serverFixture(overrides: Partial<Server> = {}): Server {
  return {
    id: "53bfe195-b78c-4c14-aebb-1bd09384f33b",
    shortId: "docker123",
    name: "world",
    displayName: "Friends world",
    image: "itzg/minecraft-server",
    state: "running",
    status: "Up",
    gameType: "minecraft",
    gameConsole: null,
    fileRoots: [],
    ports: [],
    created: 0,
    labels: {},
    permissions: ["server.view"],
    bindingStatus: "active",
    ...overrides,
  };
}

export function operationFixture(serverId: string, overrides: Partial<Operation> = {}): Operation {
  return {
    id: "c15cbd1f-dbb6-444d-8b8f-c5d728b94df0",
    serverId,
    kind: "update",
    status: "already_current",
    phase: "finished",
    createdAt: 0,
    updatedAt: 0,
    error: null,
    result: null,
    ...overrides,
  };
}

export function scheduleFixture(serverId: string, overrides: Partial<Schedule> = {}): Schedule {
  return {
    id: "77777777-7777-4777-8777-777777777777",
    serverId,
    ownerId: "88888888-8888-4888-8888-888888888888",
    action: "restart",
    enabled: true,
    time: "09:00",
    days: [0, 1, 2, 3, 4, 5, 6],
    timezone: "UTC",
    revision: 1,
    lastSlot: null,
    lastResult: "Queued operation",
    lastOperation: operationFixture(serverId, {
      kind: "restart", status: "succeeded", createdAt: Date.UTC(2026, 8, 11, 9), updatedAt: Date.UTC(2026, 8, 11, 9, 1),
    }),
    lastRunAt: Date.UTC(2026, 8, 11, 9),
    nextRunAt: Date.UTC(2026, 8, 12, 9),
    nextRunUnavailableReason: null,
    ...overrides,
  };
}

export interface ServerDetailData {
  operations?: Operation[];
  backups?: Backup[];
  schedules?: Schedule[];
}

// Defaults for detail-page reads; each scenario owns its mutations and failures.
export function serverDetailResponse(path: string, server: Server, data: ServerDetailData = {}) {
  const base = `/servers/${server.id}`;
  switch (path) {
    case base: return { server, stats: null };
    case `${base}/operations`: return { operations: data.operations ?? [] };
    case `${base}/backups`: return { backups: data.backups ?? [] };
    case `${base}/schedules`: return { schedules: data.schedules ?? [] };
    case `${base}/update-capability`: return {
      capability: {
        available: true,
        actionLabel: "Update server",
        projectName: "games",
        serviceName: "minecraft",
        image: server.image,
        manager: "compose",
      } satisfies UpdateCapability,
    };
    case `${base}/availability`: return {
      policy: { enabled: false, maintenance: false, graceSeconds: 120 } satisfies AvailabilityPolicy,
      state: {
        outageStartedAt: null,
        notified: false,
        suppressedUntil: 0,
        intentionallyStopped: false,
        lastState: null,
      } satisfies AvailabilityState,
    };
    default: throw new Error(`Unexpected server detail request: ${path}`);
  }
}
