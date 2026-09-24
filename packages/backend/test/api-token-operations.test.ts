import { expect, afterEach, beforeEach, describe, it } from "bun:test";
import { dockerId } from "./fixtures/ids.js";

process.env.LUDOCK_DB_PATH = ":memory:";

const { operationActorId } = await import("../src/auth.js");
const { closeDatabase, getDatabase } = await import("../src/database.js");
const { listAuditHistory } = await import("../src/history.js");
const { reconcileServers } = await import("../src/identity.js");
const {
  enqueueOperation,
  getOperation,
  registerJobHandler,
  startOperationRunner,
  stopOperationRunner,
} = await import("../src/operations.js");
const { jobActor } = await import("../src/jobs.js");

const originalApiToken = process.env.LUDOCK_API_TOKEN;
const firstToken = "api-token-first-generation-0123456789";
const secondToken = "api-token-second-generation-012345678";
const apiTokenUser = { id: "api-token", username: "api-token", role: "admin" as const };
let serverId: string;

function boundActorId(token: string): string {
  return operationActorId(
    new Request("http://localhost/api/v1/servers", {
      headers: { authorization: `Bearer ${token}` },
    }),
    apiTokenUser,
  );
}

async function finished(id: string) {
  for (let attempt = 0; attempt < 100; attempt++) {
    const operation = getOperation(id)!;
    if (!['queued', 'running'].includes(operation.status)) return operation;
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  expect.unreachable("Operation did not finish");
}

beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  process.env.LUDOCK_API_TOKEN = firstToken;
  serverId = reconcileServers([
    {
      containerId: dockerId("api-token-container"),
      name: "api-token-world",
      displayName: "API token world",
      gameType: "minecraft",
      mounts: [],
    },
  ])[0].id;
});

afterEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  if (originalApiToken === undefined) delete process.env.LUDOCK_API_TOKEN;
  else process.env.LUDOCK_API_TOKEN = originalApiToken;
});

describe("API token operation actors", () => {
  it("binds queued work to the current token generation and redacts its fingerprint", async () => {
    const firstActorId = boundActorId(firstToken);
    expect(firstActorId).toMatch(/^api-token:[0-9a-f]{64}$/);
    const oldOperation = enqueueOperation({
      serverId,
      actorId: firstActorId,
      kind: "api-token-operation",
      bindingRevision: 1,
    });

    process.env.LUDOCK_API_TOKEN = secondToken;
    expect(() => jobActor({ job: getOperation(oldOperation.id)!, progress: () => {} })).toThrow(/no longer has access/);
    expect(() => jobActor({
        job: { ...getOperation(oldOperation.id)!, actorId: "api-token" },
        progress: () => {},
      })).toThrow(/no longer has access/);

    getDatabase()
      .prepare("UPDATE operations SET status='failed' WHERE id=?")
      .run(oldOperation.id);
    const currentActorId = boundActorId(secondToken);
    const currentOperation = enqueueOperation({
      serverId,
      actorId: currentActorId,
      kind: "api-token-operation",
      bindingRevision: 1,
    });
    expect(jobActor({ job: getOperation(currentOperation.id)!, progress: () => {} }).id).toBe("api-token");

    registerJobHandler("api-token-operation", { run: async () => ({ ok: true }) });
    await startOperationRunner();
    expect((await finished(currentOperation.id)).status).toBe("succeeded");
    const audit = JSON.stringify(listAuditHistory({ limit: 20 }).entries);
    expect(audit.includes(firstActorId)).toBe(false);
    expect(audit.includes(currentActorId)).toBe(false);
    expect(listAuditHistory({ limit: 20 }).entries.some((entry) =>
        entry.action === "server.api-token-operation.succeeded" &&
        (entry.details as { actorId?: string } | null)?.actorId === "api-token",
      )).toBeTruthy();
  });
});
