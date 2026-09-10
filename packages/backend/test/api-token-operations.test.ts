import assert from "node:assert/strict";
import { afterEach, beforeEach, describe, it } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";

const { operationActorId } = await import("../src/auth.js");
const { closeDatabase, getDatabase, listAuditLog } = await import("../src/database.js");
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
  assert.fail("Operation did not finish");
}

beforeEach(async () => {
  await stopOperationRunner();
  closeDatabase();
  process.env.LUDOCK_API_TOKEN = firstToken;
  serverId = reconcileServers([
    {
      containerId: "api-token-container",
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
    assert.match(firstActorId, /^api-token:[0-9a-f]{64}$/);
    const oldOperation = enqueueOperation({
      serverId,
      actorId: firstActorId,
      kind: "api-token-operation",
      bindingRevision: 1,
    });

    process.env.LUDOCK_API_TOKEN = secondToken;
    assert.throws(
      () => jobActor({ job: getOperation(oldOperation.id)!, progress: () => {} }),
      /no longer has access/,
    );
    assert.throws(
      () => jobActor({
        job: { ...getOperation(oldOperation.id)!, actorId: "api-token" },
        progress: () => {},
      }),
      /no longer has access/,
    );

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
    assert.equal(
      jobActor({ job: getOperation(currentOperation.id)!, progress: () => {} }).id,
      "api-token",
    );

    registerJobHandler("api-token-operation", { run: async () => ({ ok: true }) });
    await startOperationRunner();
    assert.equal((await finished(currentOperation.id)).status, "succeeded");
    const audit = JSON.stringify(listAuditLog(20));
    assert.equal(audit.includes(firstActorId), false);
    assert.equal(audit.includes(currentActorId), false);
    assert.ok(
      listAuditLog(20).some((entry) =>
        entry.action === "server.api-token-operation.succeeded" &&
        (entry.details as { actorId?: string } | null)?.actorId === "api-token",
      ),
    );
  });
});
