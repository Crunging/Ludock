import { test, expect, RUNNING_ID } from "./fixtures";

test("console modes keep separate drafts and keyboard navigation", { tag: "@responsive" }, async ({ app, page }) => {
  await app.open(`/console/${RUNNING_ID}`);
  const game = page.getByRole("tab", { name: "Game Console", exact: true });
  await expect(game).toHaveAttribute("aria-selected", "true");
  await page.getByRole("textbox", { name: "Game command", exact: true }).fill("list");
  await game.focus();
  await page.keyboard.press("End");
  await expect(page.getByRole("tab", { name: "Container Shell", exact: true })).toBeFocused();
  await page.getByRole("textbox", { name: "Shell command", exact: true }).fill("pwd");
  await page.getByRole("tab", { name: "Container Shell", exact: true }).focus();
  await page.keyboard.press("Home");
  await expect(page.getByRole("tab", { name: "Docker Logs", exact: true })).toBeFocused();
  await expect(page.getByRole("textbox", { name: /command/ })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Pause", exact: true })).toBeVisible();
  await page.keyboard.press("ArrowRight");
  await expect(game).toBeFocused();
  await expect(page.getByRole("textbox", { name: "Game command", exact: true })).toHaveValue("list");
  const panel = page.getByRole("tabpanel", { name: "Game Console", exact: true });
  await expect(panel).toBeVisible();
  await expect(game).toHaveAttribute("id", /\S+/);
  await expect(panel).toHaveAttribute("id", /\S+/);
  await expect(panel).toHaveAttribute("aria-labelledby", (await game.getAttribute("id"))!);
  await expect(page.getByRole("tab", { selected: true })).toHaveCount(1);
  await expect(game).toHaveAttribute("aria-selected", "true");
  const panelId = (await panel.getAttribute("id"))!;
  for (const tab of await page.getByRole("tab").all()) {
    await expect(tab).toHaveAttribute("aria-controls", panelId);
    await expect(tab).toHaveAttribute("tabindex", await tab.getAttribute("aria-selected") === "true" ? "0" : "-1");
  }
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Docker Logs", exact: true })).toBeFocused();
  await page.keyboard.press("ArrowLeft");
  await expect(page.getByRole("tab", { name: "Container Shell", exact: true })).toBeFocused();
  await expect(page.getByRole("textbox", { name: "Shell command", exact: true })).toHaveValue("pwd");
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
  await page.setViewportSize({ width: 320, height: 844 });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
});

test("console reconnect preserves its draft and never resends a command", async ({ app, page }) => {
  await page.clock.install();
  await app.open(`/console/${RUNNING_ID}`);
  const command = page.getByRole("textbox", { name: "Game command", exact: true });
  const send = page.getByRole("button", { name: "Send", exact: true });
  await command.fill("list");
  await expect(send).toBeEnabled();
  await expect.poll(() => app.sockets.length).toBe(1);
  const requestsBeforeReconnect = app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length;
  await app.sockets[0].route.close({ code: 1011, reason: "Fixture interruption" });
  await expect(page.getByRole("alert")).toContainText("Connection lost");
  await expect(send).toBeDisabled();
  await expect(command).toHaveValue("list");
  await page.clock.runFor(2_100);
  await expect.poll(() => app.sockets.length).toBe(2);
  await expect(send).toBeEnabled();
  expect(app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length)
    .toBeGreaterThan(requestsBeforeReconnect);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
  await send.click();
  await expect.poll(() => app.sockets.flatMap((socket) => socket.messages))
    .toEqual([JSON.stringify({ type: "input", data: "list" })]);
  await expect(command).toHaveValue("");
  await expect(page.getByText("Command sent. Check the output for its result.", { exact: true })).toBeVisible();
  await app.sockets[1].route.close({ code: 1011, reason: "Fixture interruption after send" });
  await page.clock.runFor(2_100);
  await expect.poll(() => app.sockets.length).toBe(3);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([JSON.stringify({ type: "input", data: "list" })]);
});

test("exhausted reconnects require Retry and fresh access before sending", async ({ app, page }) => {
  await page.clock.install();
  await app.open(`/console/${RUNNING_ID}`);
  await page.getByRole("textbox", { name: "Game command", exact: true }).fill("help");
  const send = page.getByRole("button", { name: "Send", exact: true });
  await expect(send).toBeEnabled();
  for (let attempt = 0; attempt <= 10; attempt += 1) {
    await expect.poll(() => app.sockets.length).toBe(attempt + 1);
    await app.sockets[attempt].route.close({ code: 1011, reason: "Fixture server repeatedly disconnects" });
    await expect(send).toBeDisabled();
    if (attempt < 10) await page.clock.runFor(2_100);
  }
  await expect(page.getByRole("alert")).toContainText("Automatic retries have stopped");
  const retry = page.getByRole("button", { name: "Retry connection", exact: true });
  await expect(retry).toBeVisible();
  await expect(page.getByRole("textbox", { name: "Game command", exact: true })).toHaveValue("help");
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
  const previousReads = app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length;
  await retry.click();
  await expect.poll(() => app.sockets.length).toBe(12);
  await expect(send).toBeEnabled();
  expect(app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length)
    .toBeGreaterThan(previousReads);
  await expect(retry).toHaveCount(0);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
});
