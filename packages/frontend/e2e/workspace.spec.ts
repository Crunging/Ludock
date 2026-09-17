import type { Page } from "@playwright/test";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

async function expectNoPageOverflow(page: Page) {
  await expect.poll(() => page.evaluate(() =>
    document.documentElement.scrollWidth <= window.innerWidth,
  )).toBe(true);
}

test("server states and ports stay aligned and usable across viewport widths", { tag: "@responsive" }, async ({ app, page }, testInfo) => {
  await app.open();
  await expect(page.getByRole("article")).toHaveCount(2);
  const widths = testInfo.project.name === "mobile"
    ? [390, 320]
    : [1440, 1200, 1024, 900, 769];
  for (const width of widths) {
    await page.setViewportSize({ width, height: 900 });
    await expectNoPageOverflow(page);
    await expect(page.getByText("25565/tcp", { exact: true })).toBeVisible();
    await expect(page.getByText("34197/udp", { exact: true })).toBeVisible();
    const layout = await page.locator(".server-list").evaluate((list) => {
      const header = list.querySelector(".server-list__header");
      const headerCells = Array.from(header?.children || []);
      const rows = Array.from(list.querySelectorAll(".server-row"));
      return {
        columnsVisible: getComputedStyle(list).display === "grid",
        headings: headerCells.map((cell) => cell.getBoundingClientRect().x),
        rows: rows.map((row) => {
          const state = row.querySelector(".server-state")!.getBoundingClientRect();
          const ports = row.querySelector(".server-row__ports")!.getBoundingClientRect();
          const actions = row.querySelector(".server-row__actions")!.getBoundingClientRect();
          return { state: state.x, ports: ports.x, left: actions.left, right: actions.right };
        }),
      };
    });
    for (const row of layout.rows) {
      expect(row.left).toBeGreaterThanOrEqual(0);
      expect(row.right).toBeLessThanOrEqual(width);
      if (layout.columnsVisible) {
        expect(Math.abs(row.state - layout.headings[1])).toBeLessThan(1);
        expect(Math.abs(row.ports - layout.headings[2])).toBeLessThan(1);
      }
    }
    if (testInfo.project.name === "mobile") {
      await expect(page.locator(".server-row__ports-label").first()).toBeVisible();
      const heights = await page.locator(".server-row__actions .secondary-btn").evaluateAll(
        (buttons) => buttons.map((button) => button.getBoundingClientRect().height),
      );
      expect(heights.every((height) => height >= 44)).toBe(true);
      await expect(page.getByRole("searchbox", { name: "Find a server" })).toHaveCSS("font-size", "16px");
      await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveCSS("font-size", "16px");
    }
  }
});

test("search and state filters combine, survive refresh, and clear together", async ({ app, page }) => {
  await app.open();
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("exited");
  await expect(page.getByRole("article")).toHaveCount(1);
  await expect(page.getByRole("link", { name: "Factorio weekend", exact: true })).toBeVisible();
  await page.getByRole("searchbox", { name: "Find a server" }).fill("factorio");
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("exited");
  await expect(page.getByRole("searchbox", { name: "Find a server" })).toHaveValue("factorio");
  await page.getByRole("searchbox", { name: "Find a server" }).fill("missing-world");
  await expect(page.getByText("No servers match your filters.")).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page.getByRole("article")).toHaveCount(2);
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("all");
  await expect(page.getByRole("searchbox", { name: "Find a server" })).toHaveValue("");
});

test("More supports keyboard traversal, Escape, and outside dismissal", { tag: "@responsive" }, async ({ app, page }) => {
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  const trigger = row.getByRole("button", { name: `More actions for ${RUNNING_NAME}` });
  await trigger.focus();
  await page.keyboard.press("Enter");
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await page.keyboard.press("Tab");
  await expect(row.getByRole("button", { name: "Files", exact: true })).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
  await expect(trigger).toBeFocused();
  await trigger.click();
  await page.getByRole("heading", { name: "Servers", exact: true }).click();
  await expect(trigger).toHaveAttribute("aria-expanded", "false");
});

test("lifecycle confirmation traps page focus, restores its trigger, and sends only after confirmation", { tag: "@responsive" }, async ({ app, page }) => {
  await app.open();
  const row = page.getByRole("article", { name: RUNNING_NAME });
  const stop = row.getByRole("button", { name: "Stop", exact: true });
  await stop.click();
  const dialog = page.getByRole("dialog", { name: `Stop ${RUNNING_NAME}?` });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await page.keyboard.press("Tab");
  await expect(dialog.getByRole("button", { name: "Stop server", exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  // Chromium can briefly focus its own chrome while cycling a native dialog.
  // It must never focus an application control behind the modal.
  expect(await page.evaluate(() =>
    !document.hasFocus() || Boolean(document.querySelector("dialog")?.contains(document.activeElement)),
  )).toBe(true);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).focus();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(stop).toBeFocused();
  expect(app.requests.filter((request) => request.method === "POST")).toEqual([]);
  await stop.click();
  await page.getByRole("dialog").getByRole("button", { name: "Stop server", exact: true }).click();
  await expect.poll(() => app.requests.filter((request) => request.method === "POST")).toEqual([
    { method: "POST", path: `/servers/${RUNNING_ID}/stop`, query: {}, body: undefined },
  ]);
  await expect(row.getByRole("button", { name: "Start", exact: true })).toBeVisible();
});

test("detail tabs support arrow keys, Home and End without losing drafts", { tag: "@responsive" }, async ({ app, page }) => {
  await app.open(`/servers/${RUNNING_ID}`);
  const activity = page.getByRole("tab", { name: "Activity", exact: true });
  await activity.focus();
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("tab", { name: "Backups", exact: true })).toBeFocused();
  await expect(page.getByRole("tab", { name: "Backups", selected: true })).toHaveCount(1);
  await page.keyboard.press("End");
  await expect(page.getByRole("tab", { name: "Availability", exact: true })).toBeFocused();
  await page.keyboard.press("Home");
  await expect(activity).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Availability", exact: true })).toBeFocused();
  expect(await page.getByRole("tab").evaluateAll((tabs) => tabs.every((tab) => {
    const panel = document.getElementById(tab.getAttribute("aria-controls") || "");
    return panel?.getAttribute("aria-labelledby") === tab.id &&
      tab.getAttribute("tabindex") === (tab.getAttribute("aria-selected") === "true" ? "0" : "-1");
  }))).toBe(true);
  const selected = page.getByRole("tab", { selected: true });
  await expect(selected).toHaveAttribute("aria-controls", await page.getByRole("tabpanel").getAttribute("id") || "");
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await page.keyboard.press("Tab");
  await expect(page.getByRole("tabpanel", { name: "Schedules", exact: true })).toBeFocused();
  await page.getByRole("combobox", { name: "Time zone", exact: true }).fill("America/New_York");
  await page.getByRole("tab", { name: "Activity", exact: true }).click();
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await expect(page.getByRole("combobox", { name: "Time zone", exact: true })).toHaveValue("America/New_York");
  await expectNoPageOverflow(page);
});

test("empty assignment and sign-in remain usable on narrow screens", { tag: "@mobile" }, async ({ app, page }) => {
  app.user = { id: "22222222-2222-4222-8222-222222222222", username: "casey", role: "operator" };
  app.servers = [];
  await app.open();
  await expect(page.getByRole("heading", { name: "No servers assigned" })).toBeVisible();
  await expectNoPageOverflow(page);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  await page.setViewportSize({ width: 320, height: 844 });
  await expectNoPageOverflow(page);
});
