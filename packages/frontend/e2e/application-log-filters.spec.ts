import type { Page } from "@playwright/test";
import { applicationLogsResponseSchema, type ApplicationLogEntry } from "@ludock/shared";
import { test, expect } from "./fixtures";

const GENERATION = "11111111-1111-4111-8111-111111111111";
const RESTARTED_GENERATION = "22222222-2222-4222-8222-222222222222";
const CREATED_AT = Date.UTC(2026, 8, 12, 12);

function logEntry(id: number, overrides: Partial<ApplicationLogEntry> = {}): ApplicationLogEntry {
  return {
    id,
    timestamp: CREATED_AT + id * 1000,
    level: "info",
    component: "docker",
    message: `Container event ${id}`,
    ...overrides,
  };
}

async function mockLogs(page: Page, initialEntries: ApplicationLogEntry[]) {
  const fixture = { generation: GENERATION, entries: initialEntries, requests: [] as URL[] };
  await page.route("**/api/v1/application-logs?**", async (route) => {
    if (route.request().method() !== "GET") return route.fallback();
    const url = new URL(route.request().url());
    fixture.requests.push(url);
    const after = url.searchParams.get("generation") === fixture.generation
      ? Number(url.searchParams.get("after") || 0)
      : 0;
    const limit = Number(url.searchParams.get("limit") || 250);
    await route.fulfill({ json: applicationLogsResponseSchema.parse({
      generation: fixture.generation,
      entries: fixture.entries.filter((entry) => entry.id > after).slice(-limit),
    }) });
  });
  return fixture;
}

test("application log filters combine text, severity, and component and can be cleared", async ({ app, page }, testInfo) => {
  await mockLogs(page, [
    logEntry(1, { level: "error", message: "Image pull failed", context: { server: "survival", token: "[REDACTED]" } }),
    logEntry(2, { message: "Container ready", context: { server: "creative" } }),
    logEntry(3, { level: "warn", component: "scheduler", message: "Restart skipped", context: { server: "survival" } }),
    logEntry(4, { level: "debug", component: "http", message: "Request completed" }),
  ]);
  await app.open("/logs");
  const output = page.getByRole("log", { name: "Ludock application logs" });
  const search = page.getByRole("searchbox", { name: "Search logs", exact: true });
  const severity = page.getByRole("combobox", { name: "Severity", exact: true });
  const component = page.getByRole("combobox", { name: "Component", exact: true });
  await expect(page.getByText("4 of 4 recent entries", { exact: true })).toBeVisible();
  await expect(page.getByText(/Searches up to .* recent entries buffered in this page\./)).toBeVisible();
  await search.fill("DOCKER");
  await expect(page.getByText("2 of 4 recent entries", { exact: true })).toBeVisible();
  await search.fill("pull FAILED");
  await expect(output).toContainText("Image pull failed");
  await expect(output).not.toContainText("Container ready");
  await search.fill("ERROR");
  await expect(page.getByText("1 of 4 recent entries", { exact: true })).toBeVisible();
  await search.fill("SURVIVAL");
  await expect(page.getByText("2 of 4 recent entries", { exact: true })).toBeVisible();
  await severity.selectOption("error");
  await component.selectOption("docker");
  await expect(page.getByText("1 of 4 recent entries", { exact: true })).toBeVisible();
  await expect(output).toContainText("[REDACTED]");
  await expect(output).not.toContainText("Restart skipped");

  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  if (testInfo.project.name === "mobile") {
    const controls = [search, severity, component,
      page.getByRole("button", { name: "Clear filters", exact: true }),
      page.getByRole("button", { name: "Pause", exact: true }),
      page.getByRole("button", { name: "Refresh", exact: true }),
      page.locator("label").filter({ has: page.getByRole("checkbox", { name: "Follow latest", exact: true }) }),
    ];
    for (const control of controls) {
      await expect(control).toBeVisible();
      const bounds = await control.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.height).toBeGreaterThanOrEqual(44);
    }
  }
  const screenshot = testInfo.outputPath("application-log-filters.png");
  await page.evaluate(() => window.scrollTo(0, 0));
  await page.screenshot({ path: screenshot, fullPage: true });
  await testInfo.attach("Application log filters", { path: screenshot, contentType: "image/png" });

  await component.selectOption("scheduler");
  await expect(output).toContainText("No recent log entries match these filters.");
  await expect(page.getByText("0 of 4 recent entries", { exact: true })).toBeVisible();
  const clear = page.getByRole("button", { name: "Clear filters", exact: true });
  await clear.focus();
  await page.keyboard.press("Enter");
  await expect(search).toHaveValue("");
  await expect(severity).toHaveValue("");
  await expect(component).toHaveValue("");
  await expect(page.getByText("4 of 4 recent entries", { exact: true })).toBeVisible();
});

test("turning off follow preserves the reading position while polling and pause stops polling", async ({ app, page }) => {
  await page.clock.install();
  const fixture = await mockLogs(page, Array.from({ length: 90 }, (_, index) => logEntry(index + 1)));
  await app.open("/logs");
  const output = page.getByRole("log", { name: "Ludock application logs" });
  const follow = page.getByRole("checkbox", { name: "Follow latest", exact: true });
  await expect(page.getByText("90 of 90 recent entries", { exact: true })).toBeVisible();
  await expect(follow).toBeChecked();
  await expect.poll(() => output.evaluate((element) => element.scrollHeight - element.clientHeight - element.scrollTop)).toBeLessThanOrEqual(1);
  await follow.uncheck();
  await output.evaluate((element) => { element.scrollTop = 80; });
  await expect.poll(() => output.evaluate((element) => element.scrollTop)).toBe(80);
  fixture.entries.push(logEntry(91));
  await page.clock.runFor(2_100);
  await expect(page.getByText("91 of 91 recent entries", { exact: true })).toBeVisible();
  expect(fixture.requests.length).toBeGreaterThan(1);
  await expect.poll(() => output.evaluate((element) => element.scrollTop)).toBe(80);
  await follow.check();
  await expect.poll(() => output.evaluate((element) => element.scrollHeight - element.clientHeight - element.scrollTop)).toBeLessThanOrEqual(1);

  await page.getByRole("button", { name: "Pause", exact: true }).click();
  await expect(page.getByRole("button", { name: "Resume", exact: true })).toBeVisible();
  const pausedReads = fixture.requests.length;
  fixture.entries.push(logEntry(92));
  await page.clock.runFor(6_100);
  expect(fixture.requests).toHaveLength(pausedReads);
  await expect(page.getByText("91 of 91 recent entries", { exact: true })).toBeVisible();
  await expect(follow).toBeChecked();
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  await expect(page.getByText("92 of 92 recent entries", { exact: true })).toBeVisible();
  expect(fixture.requests.length).toBeGreaterThan(pausedReads);
});

test("component selection survives buffered entry eviction and a process restart", async ({ app, page }) => {
  await page.clock.install();
  const fixture = await mockLogs(page, [logEntry(1), logEntry(2, { component: "scheduler" })]);
  await app.open("/logs");
  const component = page.getByRole("combobox", { name: "Component", exact: true });
  await expect(page.getByText("2 of 2 recent entries", { exact: true })).toBeVisible();
  await component.selectOption("docker");
  await page.getByRole("searchbox", { name: "Search logs", exact: true }).fill("container");
  await expect(page.getByText("1 of 2 recent entries", { exact: true })).toBeVisible();
  fixture.entries.push(...Array.from({ length: 1000 }, (_, index) => logEntry(index + 3, { component: "scheduler" })));
  await page.clock.runFor(2_100);
  await expect(page.getByText("0 of 1000 recent entries", { exact: true })).toBeVisible();
  await expect(component).toHaveValue("docker");
  await expect(page.getByRole("log")).toContainText("No recent log entries match these filters.");

  fixture.generation = RESTARTED_GENERATION;
  fixture.entries = [logEntry(1, { message: "Container rebound after restart" })];
  await page.clock.runFor(2_100);
  await expect(page.getByText("1 of 1 recent entries", { exact: true })).toBeVisible();
  await expect(component).toHaveValue("docker");
  await expect(page.getByRole("searchbox", { name: "Search logs", exact: true })).toHaveValue("container");
  await expect(page.getByRole("log")).toContainText("Container rebound after restart");
  await expect(page.getByRole("log")).not.toContainText("Container event");
});

test("reading position follows a retained row when the full log buffer evicts older entries", async ({ app, page }) => {
  await page.clock.install();
  const variedEntry = (id: number) => logEntry(id, {
    message: [`Container event ${id}`, ...Array.from({ length: id % 4 + 1 }, (_, index) => `Detail line ${index + 1}`)].join("\n"),
  });
  const fixture = await mockLogs(page, Array.from({ length: 250 }, (_, index) => variedEntry(index + 1)));
  await app.open("/logs");
  const output = page.getByRole("log", { name: "Ludock application logs" });
  await expect(page.getByText("250 of 250 recent entries", { exact: true })).toBeVisible();
  fixture.entries.push(...Array.from({ length: 750 }, (_, index) => variedEntry(index + 251)));
  await page.clock.runFor(2_100);
  await expect(page.getByText("1000 of 1000 recent entries", { exact: true })).toBeVisible();
  await page.getByRole("checkbox", { name: "Follow latest", exact: true }).uncheck();
  const anchor = output.locator(".application-log").filter({ has: page.getByText(variedEntry(600).message, { exact: true }) });
  await anchor.evaluate((row) => {
    const container = row.parentElement;
    if (!container) throw new Error("Log entry has no scroll container");
    container.scrollTop += row.getBoundingClientRect().top - container.getBoundingClientRect().top + 8;
  });
  const relativeTop = () => anchor.evaluate((row) => {
    const container = row.parentElement;
    if (!container) throw new Error("Log entry has no scroll container");
    return row.getBoundingClientRect().top - container.getBoundingClientRect().top;
  });
  await expect.poll(async () => Math.abs(await relativeTop() + 8)).toBeLessThanOrEqual(1);
  const initialTop = await relativeTop();

  for (const batch of [{ first: 1001, count: 12 }, { first: 1013, count: 5 }]) {
    fixture.entries.push(...Array.from({ length: batch.count }, (_, index) => variedEntry(batch.first + index)));
    await page.clock.runFor(2_100);
    await expect(output.getByText(variedEntry(batch.first + batch.count - 1).message, { exact: true })).toHaveCount(1);
    await expect(page.getByText("1000 of 1000 recent entries", { exact: true })).toBeVisible();
    await expect(output.getByText(variedEntry(batch.first - 1000).message, { exact: true })).toHaveCount(0);
    await expect.poll(async () => Math.abs(await relativeTop() - initialTop)).toBeLessThanOrEqual(1);
  }
});
