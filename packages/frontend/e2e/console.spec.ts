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
