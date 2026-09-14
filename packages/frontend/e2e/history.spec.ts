import type { Page } from "@playwright/test";
import {
  auditEntrySchema,
  auditResponseSchema,
  operationResponseSchema,
  operationSchema,
  operationsResponseSchema,
} from "@ludock/shared";
import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME } from "./fixtures";

const OPERATION_ID = "11111111-1111-4111-8111-111111111111";
const OLDER_OPERATION_ID = "22222222-2222-4222-8222-222222222222";
const CREATED_AT = Date.UTC(2026, 8, 12, 12);
const OPERATION_CURSOR = "opaque-older-operations";
const AUDIT_CURSOR = "opaque-older-audit";

function operation(id = OPERATION_ID, phase = "Restart failed after stopping") {
  return operationSchema.parse({
    id,
    serverId: RUNNING_ID,
    kind: "restart",
    status: "failed",
    phase,
    createdAt: CREATED_AT,
    updatedAt: CREATED_AT + 1_000,
    actor: { id: ADMIN.id, name: ADMIN.username },
    error: phase,
    result: null,
  });
}

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

async function mockHistory(page: Page) {
  const requests = { operations: [] as URL[], audit: [] as URL[], details: [] as string[] };
  await page.route("**/api/v1/operations**", async (route) => {
    const url = new URL(route.request().url());
    const id = url.pathname.match(/^\/api\/v1\/operations\/([^/]+)$/)?.[1];
    if (id) {
      requests.details.push(id);
      await route.fulfill({ json: operationResponseSchema.parse({ operation: operation(id) }) });
      return;
    }
    requests.operations.push(url);
    const older = url.searchParams.get("cursor") === OPERATION_CURSOR;
    await route.fulfill({ json: operationsResponseSchema.parse({
      operations: [older ? operation(OLDER_OPERATION_ID, "Older restart failed") : operation()],
      nextCursor: older ? null : OPERATION_CURSOR,
    }) });
  });
  await page.route("**/api/v1/audit**", async (route) => {
    const url = new URL(route.request().url());
    requests.audit.push(url);
    const older = url.searchParams.get("cursor") === AUDIT_CURSOR;
    await route.fulfill({ json: auditResponseSchema.parse({
      entries: [older ? auditEntry(1, "scheduled.restart.failed") : auditEntry()],
      nextCursor: older ? null : AUDIT_CURSOR,
    }) });
  });
  return requests;
}

async function enterFilters(page: Page, action: string) {
  await page.getByRole("combobox", { name: "Server", exact: true }).selectOption(RUNNING_ID);
  await page.getByRole("searchbox", { name: "Actor", exact: true }).fill(ADMIN.username);
  await page.getByRole("searchbox", { name: "Action", exact: true }).fill(action);
  await page.getByRole("combobox", { name: "Status", exact: true }).selectOption("failed");
  await page.getByLabel("From", { exact: true }).fill("2026-09-01T00:00");
  await page.getByLabel("To", { exact: true }).fill("2026-09-14T23:59");
  const search = page.getByRole("button", { name: "Search", exact: true });
  await search.focus();
  await page.keyboard.press("Enter");
  await expect(search).toBeFocused();
}

function expectedFilters(action: string) {
  return {
    serverId: RUNNING_ID,
    actor: ADMIN.username,
    action,
    status: "failed",
    from: String(Date.UTC(2026, 8, 1)),
    to: String(Date.UTC(2026, 8, 14, 23, 59)),
    limit: "50",
  };
}

test("operation history searches the backend and preserves filters through pagination and refresh", async ({ app, page }, testInfo) => {
  const requests = await mockHistory(page);
  await app.open(`/servers/${RUNNING_ID}`);
  await page.getByRole("link", { name: "Search operation history", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Operation history", exact: true })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "Server", exact: true })).toHaveValue(RUNNING_ID);
  await expect.poll(() => requests.operations.length).toBe(1);
  await enterFilters(page, "restart");
  await expect.poll(() => Object.fromEntries(requests.operations.at(-1)!.searchParams)).toEqual(expectedFilters("restart"));
  await expect(page.getByRole("table", { name: "Operation history", exact: true })).toContainText(RUNNING_NAME);
  const filteredUrl = page.url();
  const screenshot = testInfo.outputPath("operation-history-filters.png");
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Operation history filters", { path: screenshot, contentType: "image/png" });

  await page.getByRole("button", { name: "Older", exact: true }).click();
  await expect.poll(() => Object.fromEntries(requests.operations.at(-1)!.searchParams)).toEqual({ ...expectedFilters("restart"), cursor: OPERATION_CURSOR });
  await expect(page.getByRole("main")).toContainText("Older restart failed");
  await expect(page.getByRole("button", { name: "Older", exact: true })).toBeDisabled();
  await page.getByRole("searchbox", { name: "Actor", exact: true }).fill("unsubmitted actor");
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByRole("searchbox", { name: "Actor", exact: true })).toHaveValue("unsubmitted actor");
  await expect.poll(() => Object.fromEntries(requests.operations.at(-1)!.searchParams)).toEqual({ ...expectedFilters("restart"), cursor: OPERATION_CURSOR });
  await page.getByRole("button", { name: "Newer", exact: true }).click();
  await expect(page).toHaveURL(filteredUrl);
  await expect(page.getByRole("main")).toContainText("Restart failed after stopping");
  await expect(page.getByRole("main")).not.toContainText("Older restart failed");
  await expect(page.getByRole("searchbox", { name: "Actor", exact: true })).toHaveValue("unsubmitted actor");

  await page.getByRole("table", { name: "Operation history", exact: true }).getByRole("link", { name: "restart", exact: true }).click();
  await expect(page).toHaveURL(`/operations/${OPERATION_ID}`);
  await expect(page.getByRole("heading", { name: "Operation details", exact: true })).toBeVisible();
  await page.goBack();
  await expect(page).toHaveURL(filteredUrl);

  await page.getByRole("button", { name: "Clear filters", exact: true }).click();
  await expect(page.getByRole("searchbox", { name: "Actor", exact: true })).toHaveValue("");
  await expect.poll(() => Object.fromEntries(requests.operations.at(-1)!.searchParams)).toEqual({ limit: "50" });
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("audit search links to reloadable operation details and back to related events", async ({ app, page }, testInfo) => {
  const requests = await mockHistory(page);
  await app.open("/audit");
  await expect(page.getByText("server.restart.failed", { exact: true })).toBeVisible();
  await enterFilters(page, "restart");
  await expect.poll(() => Object.fromEntries(requests.audit.at(-1)!.searchParams)).toEqual(expectedFilters("restart"));
  await expect(page.getByText("server.restart.failed", { exact: true })).toBeVisible();
  const auditUrl = page.url();
  const screenshot = testInfo.outputPath("audit-history-filters.png");
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Audit history filters", { path: screenshot, contentType: "image/png" });
  await page.getByRole("button", { name: "Older", exact: true }).click();
  await expect(page.getByText("scheduled.restart.failed", { exact: true })).toBeVisible();
  await expect.poll(() => Object.fromEntries(requests.audit.at(-1)!.searchParams)).toEqual({ ...expectedFilters("restart"), cursor: AUDIT_CURSOR });
  await page.goBack();
  await expect(page).toHaveURL(auditUrl);
  await expect(page.getByText("server.restart.failed", { exact: true })).toBeVisible();
  await expect(page.getByRole("searchbox", { name: "Actor", exact: true })).toHaveValue(ADMIN.username);

  await page.getByRole("main").getByRole("link", { name: /operation/i }).click();
  await expect(page).toHaveURL(`/operations/${OPERATION_ID}`);
  await expect(page.getByRole("heading", { name: "Operation details", exact: true })).toBeVisible();
  await expect(page.getByRole("main")).toContainText(OPERATION_ID);
  await expect(page.getByRole("main")).toContainText(ADMIN.username);
  await page.reload();
  await expect(page.getByRole("heading", { name: "Operation details", exact: true })).toBeVisible();
  await expect.poll(() => requests.details.length).toBe(2);
  const detailScreenshot = testInfo.outputPath("operation-details.png");
  await page.screenshot({ path: detailScreenshot, fullPage: true });
  await testInfo.attach("Operation details", { path: detailScreenshot, contentType: "image/png" });
  await page.getByRole("link", { name: "Related audit events", exact: true }).click();
  await expect.poll(() => requests.audit.at(-1)!.searchParams.get("operationId")).toBe(OPERATION_ID);
  await expect(page.getByRole("textbox", { name: "Operation ID", exact: true })).toHaveValue(OPERATION_ID);
  await page.goBack();
  await expect(page.getByRole("heading", { name: "Operation details", exact: true })).toBeVisible();
  await page.goForward();
  await expect(page.getByRole("textbox", { name: "Operation ID", exact: true })).toHaveValue(OPERATION_ID);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("an operator can open an authorized operation without administrator audit controls", async ({ app, page }) => {
  app.user = { id: "33333333-3333-4333-8333-333333333333", username: "casey", role: "operator" };
  app.servers = [{ ...app.servers[0], permissions: ["server.view"] }];
  const requests = await mockHistory(page);
  await app.open(`/operations/${OPERATION_ID}`);
  await expect(page.getByRole("heading", { name: "Operation details", exact: true })).toBeVisible();
  await expect(page.getByRole("main")).toContainText("Restart failed after stopping");
  await expect(page.getByRole("link", { name: "Related audit events", exact: true })).toHaveCount(0);
  expect(requests.audit).toEqual([]);
  expect(app.requests.filter((request) => request.method !== "GET")).toEqual([]);
});

test("history filters and pagination fit a narrow screen with usable controls", async ({ app, page }, testInfo) => {
  await mockHistory(page);
  await page.setViewportSize({ width: 320, height: 844 });
  for (const path of ["/operations", "/audit"]) {
    await app.open(path);
    await expect(page.getByRole("button", { name: "Search", exact: true })).toBeVisible();
    const controls = page.getByRole("main").locator("input, select, button");
    const heights = await controls.evaluateAll((elements) => elements.map((element) => element.getBoundingClientRect().height));
    for (const height of heights) expect(height).toBeGreaterThanOrEqual(44);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.evaluate(() => window.scrollTo(0, 0));
    const screenshot = testInfo.outputPath(`${path.slice(1)}-history-320.png`);
    await page.screenshot({ path: screenshot, fullPage: true });
    await testInfo.attach(`${path.slice(1)} history at 320px`, { path: screenshot, contentType: "image/png" });
  }
});

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
