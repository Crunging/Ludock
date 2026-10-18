import { test, expect, RUNNING_ID } from "./fixtures";

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
  const retry = page.getByRole("button", { name: "Retry connection", exact: true });
  await expect(page.getByRole("textbox", { name: "Game command", exact: true })).toHaveValue("help");
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
  const previousReads = app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length;
  await retry.click();
  await expect.poll(() => app.sockets.length).toBe(12);
  await expect(send).toBeEnabled();
  expect(app.requests.filter((request) => request.path === `/servers/${RUNNING_ID}`).length)
    .toBeGreaterThan(previousReads);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
});

test("shortcuts and command history work alongside the unsent draft", async ({ app, page }) => {
  await app.open(`/console/${RUNNING_ID}`);
  const command = page.getByRole("textbox", { name: "Game command", exact: true });
  const output = page.locator(".xterm-rows");
  const sent = () => app.sockets.flatMap((socket) => socket.messages).map((message) => JSON.parse(message).data);
  await expect(page.getByRole("status").filter({ hasText: "Connected" })).toBeVisible();
  await command.fill("whitelist add Steve");
  await page.getByRole("button", { name: "List players", exact: true }).click();
  await expect.poll(sent).toEqual(["list"]);
  await expect(command).toHaveValue("whitelist add Steve");
  await app.sockets[0].route.send(JSON.stringify({ type: "stdout", data: "There are 0 of a max of 20 players online\n" }));
  await expect(output).toContainText("> list");
  await expect(output).toContainText("There are 0 of a max of 20 players online");
  // Shortcuts that need more input only prefill the command.
  await page.getByRole("button", { name: "Message players", exact: true }).click();
  await expect(command).toHaveValue("say ");
  expect(sent()).toEqual(["list"]);

  await command.fill("time set day");
  await command.press("Enter");
  await command.fill("weather clear");
  await command.press("Enter");
  await expect.poll(sent).toEqual(["list", "time set day", "weather clear"]);
  await command.fill("difficulty");
  await command.press("ArrowUp");
  await expect(command).toHaveValue("weather clear");
  await command.press("ArrowUp");
  await command.press("ArrowUp");
  await command.press("ArrowUp");
  await expect(command).toHaveValue("list");
  await command.press("ArrowDown");
  await expect(command).toHaveValue("time set day");
  await command.press("ArrowDown");
  await command.press("ArrowDown");
  await expect(command).toHaveValue("difficulty");
  expect(sent()).toEqual(["list", "time set day", "weather clear"]);
});

test("reconnecting rechecks access, falls back to logs, and fails closed when access ends", async ({ app, page }) => {
  await page.clock.install();
  app.user = { ...app.user!, role: "operator" };
  await app.open(`/console/${RUNNING_ID}`);
  const command = page.getByRole("textbox", { name: "Game command", exact: true });
  await command.fill("list");
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeEnabled();
  await expect.poll(() => app.sockets.map((socket) => socket.path)).toEqual([`/ws/v1/game-console/${RUNNING_ID}`]);

  app.servers[0].permissions = ["server.view", "logs.read"];
  await app.sockets[0].route.close({ code: 1011, reason: "Fixture interruption" });
  await page.clock.runFor(2_100);
  await expect(command).toHaveCount(0);
  await expect(page.getByRole("tab")).toHaveText(["Docker Logs"]);
  await expect.poll(() => app.sockets.at(-1)?.path).toBe(`/ws/v1/logs/${RUNNING_ID}`);
  await page.clock.runFor(100);
  await expect(page.locator(".xterm-rows")).toContainText("Switched to Docker Logs");

  app.servers = app.servers.filter((server) => server.id !== RUNNING_ID);
  const socketsBeforeRevocation = app.sockets.length;
  await app.sockets.at(-1)!.route.close({ code: 1011, reason: "Fixture interruption after revocation" });
  await page.clock.runFor(2_100);
  await expect(page.getByRole("alert")).toContainText("Reload details to check your access");
  await expect(page.getByRole("tablist")).toHaveCount(0);
  const socketsAfterDenial = app.sockets.length;
  expect(socketsAfterDenial).toBeGreaterThan(socketsBeforeRevocation);
  await page.clock.runFor(25_000);
  expect(app.sockets).toHaveLength(socketsAfterDenial);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
});

test("a late access check cannot restore the console after the connection denies access", async ({ app, page }) => {
  let reads = 0;
  let releaseCheck = () => {};
  const checkHeld = new Promise<void>((resolve) => { releaseCheck = resolve; });
  await page.route(`**/api/v1/servers/${RUNNING_ID}`, async (route) => {
    reads += 1;
    // The second read is the access check that runs once the socket opens.
    if (reads === 2) await checkHeld;
    await route.fallback();
  });
  await app.open(`/console/${RUNNING_ID}`);
  const command = page.getByRole("textbox", { name: "Game command", exact: true });
  const output = page.locator(".xterm-rows");
  await command.fill("list");
  await expect.poll(() => reads).toBe(2);
  await expect(page.getByRole("status").filter({ hasText: "Checking access…" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Send", exact: true })).toBeDisabled();
  await app.sockets[0].route.send(JSON.stringify({ type: "stdout", data: "private game output\n" }));
  await expect(output).toContainText("private game output");

  await app.sockets[0].route.close({ code: 4403, reason: "Fixture access revoked" });
  await expect(page.getByRole("alert")).toContainText("Connection access is unavailable");
  await expect(output).not.toContainText("private game output");
  // Grants changed meanwhile, so a late answer that was trusted would switch modes and reconnect.
  app.servers[0].permissions = ["server.view", "logs.read"];
  const lateCheck = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/v1/servers/${RUNNING_ID}`);
  releaseCheck();
  await (await lateCheck).finished();
  await page.waitForTimeout(250);
  await expect(page.getByRole("alert")).toContainText("Connection access is unavailable");
  await expect(page.getByRole("tablist")).toHaveCount(0);
  await expect(command).toHaveCount(0);
  await expect(output).not.toContainText("private game output");
  await expect(output).not.toContainText("Switched to");
  expect(app.sockets).toHaveLength(1);
  expect(app.sockets.flatMap((socket) => socket.messages)).toEqual([]);
});
