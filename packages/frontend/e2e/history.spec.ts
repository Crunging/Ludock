import { auditEntrySchema, auditResponseSchema } from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID } from "./fixtures";

const OPERATION_ID = "11111111-1111-4111-8111-111111111111";
const CREATED_AT = Date.UTC(2026, 8, 12, 12);

function auditEntry(id = 2, action = "server.restart.failed") {
  return auditEntrySchema.parse({
    id,
    username: ADMIN.username,
    actor: { id: ADMIN.id, name: ADMIN.username },
    action,
    targetType: "server",
    targetId: RUNNING_ID,
    operationId: OPERATION_ID,
    status: "failed",
    details: { operationId: OPERATION_ID },
    ipAddress: null,
    createdAt: CREATED_AT,
  });
}

test("an obsolete audit response cannot replace a newer search", async ({ app, page }) => {
  let releaseOld = () => {};
  const oldResponse = new Promise<void>((resolve) => { releaseOld = resolve; });
  let oldRequestStarted = false;
  let oldRequestFinished = false;
  await page.route("**/api/v1/audit**", async (route) => {
    const actor = new URL(route.request().url()).searchParams.get("actor");
    if (actor === "older") {
      oldRequestStarted = true;
      await oldResponse;
    }
    await route.fulfill({ json: auditResponseSchema.parse({
      entries: [auditEntry(1, actor === "older" ? "obsolete-search-result" : actor === "newer" ? "newest-search-result" : "initial-result")],
      nextCursor: null,
    }) });
    if (actor === "older") oldRequestFinished = true;
  });
  await app.open("/audit");
  await expect(page.getByText("initial-result", { exact: true })).toBeVisible();
  const actor = page.getByRole("searchbox", { name: "Actor", exact: true });
  await actor.fill("older");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect.poll(() => oldRequestStarted).toBe(true);
  await actor.fill("newer");
  await page.getByRole("button", { name: "Search", exact: true }).click();
  await expect(page.getByText("newest-search-result", { exact: true })).toBeVisible();
  releaseOld();
  await expect.poll(() => oldRequestFinished).toBe(true);
  await expect(actor).toHaveValue("newer");
  await expect(page.getByText("newest-search-result", { exact: true })).toBeVisible();
  await expect(page.getByText("obsolete-search-result", { exact: true })).toHaveCount(0);
});
