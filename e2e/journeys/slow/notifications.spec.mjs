import { createServer } from "node:http";
import { createServer as createTCPServer } from "node:net";
import { test, expect, asAdmin, createUser, dockerAction, expectOperationSucceeded, monitoringSettings, openServer, signIn } from "../fixtures.mjs";

asAdmin();
// Each journey here passes on its own, so they spread across workers.
test.describe.configure({ mode: "parallel" });

// receiver records the HTTP requests ntfy, Gotify, and webhook deliveries
// make. Paths under /status/<code> answer with that status.
async function receiver() {
  const requests = [];
  const statuses = {};
  const server = createServer((request, response) => {
    let body = "";
    request.on("data", (chunk) => { body += chunk; });
    request.on("end", () => {
      requests.push({ method: request.method, url: request.url, headers: request.headers, body: body ? JSON.parse(body) : null });
      response.writeHead(statuses[request.url] ?? 200, { "Content-Type": "application/json" });
      response.end("{}");
    });
  });
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  return { requests, statuses, url: `http://127.0.0.1:${server.address().port}`, close: () => server.close() };
}

// mailServer accepts plain SMTP and records each message's text.
async function mailServer() {
  const messages = [];
  const server = createTCPServer((socket) => {
    socket.setEncoding("utf8");
    let buffer = "";
    let message = null;
    socket.write("220 fake ESMTP\r\n");
    socket.on("data", (chunk) => {
      buffer += chunk;
      for (let end = buffer.indexOf("\r\n"); end >= 0; end = buffer.indexOf("\r\n")) {
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (message !== null) {
          if (line === ".") {
            messages.push(message);
            message = null;
            socket.write("250 queued\r\n");
          } else {
            message += line.replace(/^\./, "") + "\n";
          }
          continue;
        }
        const verb = line.split(" ")[0].toUpperCase();
        if (verb === "EHLO" || verb === "HELO") socket.write("250-fake\r\n250 8BITMIME\r\n");
        else if (verb === "DATA") { message = ""; socket.write("354 go ahead\r\n"); }
        else if (verb === "QUIT") socket.end("221 bye\r\n");
        else socket.write("250 ok\r\n");
      }
    });
  });
  await new Promise((done) => server.listen(0, "127.0.0.1", done));
  return { messages, port: server.address().port, close: () => server.close() };
}

async function openDestination(page, name) {
  const card = page.locator(`#notification-${name}-card`);
  if (!(await card.evaluate((details) => details.open))) await card.locator(":scope > summary").click();
  return card;
}

async function save(card, label) {
  await card.getByRole("button", { name: `Save ${label}` }).click();
  await expect(card.locator("form .alert--success")).toHaveText(`${label} settings saved.`);
}

/** Opens Advanced, which holds Recent deliveries, unless it is open. */
async function openDeliveries(page) {
  const advanced = page.locator("#advanced-settings");
  if (!(await advanced.evaluate((details) => details.open))) await advanced.locator(":scope > summary").click();
}

/** Sends a test and waits for its result beside the button. */
async function sendTest(card, label) {
  await card.getByRole("button", { name: "Send test notification" }).click();
  await expect(card.locator(".notification-test-result")).toHaveText(`Delivered. The test message should be in ${label} now.`, { timeout: 30_000 });
}

async function turnOff(page, name, label) {
  const card = await openDestination(page, name);
  await card.locator("input[name=enabled]").uncheck();
  await save(card, label);
  await expect(card.locator(":scope > summary")).toContainText("Off");
}

test("sending test alerts to ntfy, Gotify, email, and a webhook", async ({ page }) => {
  const http = await receiver();
  const mail = await mailServer();
  try {
    await page.goto("/settings");
    // Delivery history waits under Advanced.
    await expect(page.locator("#notification-deliveries")).toBeHidden();
    await openDeliveries(page);
    await expect(page.locator("#notification-deliveries-title")).toHaveText("Recent deliveries");

    const ntfy = await openDestination(page, "ntfy");
    await expect(ntfy.getByRole("button", { name: "Send test notification" })).toBeDisabled();
    await ntfy.getByLabel("Server URL").fill(http.url + "/ntfy");
    await ntfy.getByLabel("Topic").fill("e2e-secret-topic");
    await ntfy.getByLabel("Access token (optional)").fill("tk_e2esecret");
    // A destination set up for the first time is on once saved.
    await expect(ntfy.getByLabel("Send alerts to ntfy")).toBeChecked();
    await save(ntfy, "ntfy");
    await expect(ntfy.locator(":scope > summary")).toContainText("On");
    await expect(ntfy.getByLabel("Replace topic")).toHaveValue("");
    await sendTest(ntfy, "ntfy");
    const published = http.requests.find((request) => request.url === "/ntfy");
    expect(published.headers.authorization).toBe("Bearer tk_e2esecret");
    expect(published.body).toEqual({ topic: "e2e-secret-topic", title: "Ludock", message: "Ludock test notification: ntfy delivery is working." });

    const gotify = await openDestination(page, "gotify");
    await gotify.getByLabel("Server URL").fill(http.url + "/gotify/");
    await gotify.getByLabel("Application token").fill("Ae2esecret");
    await gotify.getByLabel("Send alerts to Gotify").check();
    await save(gotify, "Gotify");
    await sendTest(gotify, "Gotify");
    const pushed = http.requests.find((request) => request.url === "/gotify/message");
    expect(pushed.headers["x-gotify-key"]).toBe("Ae2esecret");
    expect(pushed.body.message).toBe("Ludock test notification: Gotify delivery is working.");

    const email = await openDestination(page, "email");
    await email.getByLabel("Mail server").fill("127.0.0.1");
    await email.getByLabel("Security").selectOption("none");
    await email.getByLabel("Port", { exact: true }).fill(String(mail.port));
    await email.getByLabel("From").fill("Ludock <ludock@example.com>");
    await email.getByLabel("To", { exact: true }).fill("admin@example.com, ops@example.com");
    await email.getByLabel("Send alerts by email").check();
    await save(email, "Email");
    await expect(email.getByLabel("To", { exact: true })).toHaveValue("<admin@example.com>, <ops@example.com>");
    await sendTest(email, "Email");
    expect(mail.messages).toHaveLength(1);
    expect(mail.messages[0]).toContain("Subject: Ludock: Ludock test notification: Email delivery is working.");
    expect(mail.messages[0]).toContain("To: <admin@example.com>, <ops@example.com>");

    const webhook = await openDestination(page, "webhook");
    await webhook.getByLabel("Webhook URL").fill(http.url + "/hook?token=e2e-secret-query");
    await webhook.getByLabel("Authorization header (optional)").fill("Bearer e2e-secret-header");
    await webhook.getByLabel("Send alerts to the webhook").check();
    await save(webhook, "Webhook");
    await sendTest(webhook, "Webhook");
    const hook = http.requests.find((request) => request.url.startsWith("/hook"));
    expect(hook.headers.authorization).toBe("Bearer e2e-secret-header");
    expect(hook.body).toMatchObject({ source: "ludock", kind: "test", message: "Ludock test notification: Webhook delivery is working." });

    // Removing the optional header keeps the URL.
    await webhook.getByLabel("Remove the saved Authorization header").check();
    await save(webhook, "Webhook");
    await expect(webhook.getByLabel("Remove the saved Authorization header")).toHaveCount(0);
    await expect(webhook.getByLabel("Replace webhook URL")).toBeVisible();

    // Alert types default to everything except newer images, a choice is
    // saved, and choosing none is refused.
    const types = webhook.locator(".notification-alerts");
    await expect(types.locator("summary")).toHaveText("Alert types Everything except newer images");
    await types.locator("summary").click();
    await webhook.getByLabel("Newer images available").check();
    await webhook.getByLabel("Server problems").uncheck();
    await save(webhook, "Webhook");
    await expect(types.locator("summary")).toHaveText("Alert types Failed backups and schedules, Restores and updates, Newer images available");
    if (!(await types.evaluate((details) => details.open))) await types.locator("summary").click();
    for (const label of ["Failed backups and schedules", "Restores and updates", "Newer images available"]) {
      await webhook.getByLabel(label).uncheck();
    }
    await webhook.getByRole("button", { name: "Save Webhook" }).click();
    await expect(webhook.locator(".alert--error")).toHaveText("Choose at least one alert type");
    for (const label of ["Server problems", "Failed backups and schedules", "Restores and updates"]) {
      await webhook.getByLabel(label).check();
    }
    await save(webhook, "Webhook");
    await expect(types.locator("summary")).toHaveText("Alert types Everything except newer images");

    await page.reload();
    const content = await page.content();
    for (const secret of ["e2e-secret-topic", "tk_e2esecret", "Ae2esecret", "e2e-secret-query", "e2e-secret-header"]) {
      expect(content).not.toContain(secret);
    }
    await expect(page.locator("#notification-email-card > summary")).toContainText("On");

    // Turned off, so later journeys' alerts go nowhere.
    await turnOff(page, "ntfy", "ntfy");
    await turnOff(page, "gotify", "Gotify");
    await turnOff(page, "email", "Email");
    await turnOff(page, "webhook", "Webhook");
  } finally {
    http.close();
    mail.close();
  }
});

// A destination whose alerts don't arrive says so on its card and in Needs
// attention, for administrators only, until a delivery gets through.
test("a failing webhook is called out until a delivery gets through, and then an outage reaches it", async ({ page, session }) => {
  test.setTimeout(180_000);
  const http = await receiver();
  http.statuses["/status/503"] = 503;
  const unavailable = "Delivery to the webhook failed because it is temporarily unavailable. Try again later.";
  try {
    await page.goto("/settings");
    let webhook = await openDestination(page, "webhook");
    await webhook.locator("input[name=url]").fill(http.url + "/status/503");
    await webhook.getByLabel("Send alerts to the webhook").check();
    await save(webhook, "Webhook");
    await expect(webhook.locator(":scope > summary")).toContainText("On");

    // The test's result shows beside its button, and the card turns failing.
    await webhook.getByRole("button", { name: "Send test notification" }).click();
    await expect(webhook.locator(".notification-test-result")).toHaveText(`The test didn’t arrive. ${unavailable}`, { timeout: 30_000 });
    await expect(webhook.locator(":scope > summary")).toContainText("Failing");
    await expect(webhook.locator(".notification-destination__health")).toContainText("Alerts aren’t reaching Webhook. Last tried");
    await expect(webhook.locator(".notification-destination__health")).toContainText(unavailable);

    await page.goto("/");
    const attention = page.getByRole("region", { name: "Needs attention" });
    await expect(attention).toContainText("Alerts aren’t reaching Webhook");
    await expect(attention).toContainText(unavailable);
    expect(await page.content()).not.toContain(http.url);

    // Members never see notification settings, so they aren't told.
    await createUser(page, "alerted-member", "alerted member password");
    // A fresh profile, not the administrator this file signs in as.
    const member = await session({ storageState: { cookies: [], origins: [] } });
    await signIn(member, "alerted-member", "alerted member password");
    await expect(member.getByRole("heading", { level: 1, name: "Servers" })).toBeVisible();
    const fragment = await member.request.get("/fragments/attention");
    expect(fragment.ok()).toBe(true);
    expect(await fragment.text()).not.toContain("Alerts aren");

    // The link opens the destination's card.
    await page.goto("/");
    await page.getByRole("link", { name: "Check Webhook: Notifications — Alerts aren’t reaching Webhook" }).click();
    webhook = page.locator("#notification-webhook-card");
    await expect(webhook).toHaveAttribute("open", "");
    await expect(webhook.locator(".notification-destination__health")).toContainText("Alerts aren’t reaching Webhook.");

    // Recent deliveries reads each one back as sent.
    await openDeliveries(page);
    const row = page.locator(".notification-deliveries-table tbody tr", { hasText: "Ludock test notification: Webhook delivery is working." }).first();
    await expect(row).toContainText("Test notification · Queued");
    await expect(row).toContainText("Failed");
    await expect(row).toContainText(unavailable);

    // A retry that gets through puts it back to On and clears the alert.
    http.statuses["/status/503"] = 204;
    await row.getByRole("button", { name: "Retry Webhook notification" }).click();
    await expect(page.locator("#notification-deliveries .alert--success")).toContainText("Notification queued for retry.");
    await expect(row).toContainText("Delivered", { timeout: 30_000 });
    expect(http.requests.filter((request) => request.url === "/status/503")).toHaveLength(2);
    await expect(page.locator("#notification-webhook-card > summary")).toContainText("On");
    await expect(page.locator("#notification-webhook-card .notification-destination__health")).toBeEmpty();
    await page.goto("/");
    await expect(page.locator("#attention")).not.toContainText("Alerts aren’t reaching");

    // A real alert reaches it too: Enshrouded, started through Ludock with a
    // short wait before alerting, goes down behind Ludock's back.
    await openServer(page, "enshrouded");
    await monitoringSettings(page);
    await expect(page.getByLabel("Monitor this server")).toBeChecked();
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
    await page.getByLabel("Wait before alerting (seconds)").fill("11");
    await page.getByLabel("Wait before alerting (seconds)").press("Tab");
    expect((await saved).ok()).toBe(true);
    await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
    await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
    await dockerAction("enshrouded", "stop");
    await expect.poll(() => http.requests.filter((r) => r.url === "/status/503").map((r) => r.body?.message), { timeout: 90_000 })
      .toContain("enshrouded: the server is down.");
    // Recent deliveries shows the alert as sent, with its type.
    await page.goto("/settings");
    await openDeliveries(page);
    const alertRow = page.locator(".notification-deliveries-table tbody tr").first();
    await expect(alertRow).toContainText("enshrouded: the server is down.");
    await expect(alertRow).toContainText("Server problems · Queued");
  } finally {
    // A stop through Ludock leaves it watched but quiet for later journeys.
    await dockerAction("enshrouded", "start");
    await openServer(page, "enshrouded");
    await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
    await expectOperationSucceeded(page);
    await page.goto("/settings");
    await turnOff(page, "webhook", "Webhook");
    http.close();
  }
});

test("saving Discord and Telegram settings without showing secrets again", async ({ page }) => {
  // Delivery stays off, so nothing is sent to the made-up webhook or bot.
  const webhook = "https://discord.com/api/webhooks/123/e2e-secret-token";
  await page.goto("/settings");
  const discord = await openDestination(page, "discord");
  await expect(discord.getByLabel("Send alerts to Discord")).toBeChecked();
  await discord.getByLabel("Send alerts to Discord").uncheck();
  await discord.getByLabel("Webhook URL").fill("https://example.com/not-discord");
  await discord.getByRole("button", { name: "Save Discord" }).click();
  await expect(discord.locator(".alert--error")).toHaveText("Use a Discord HTTPS webhook URL");
  await discord.getByLabel("Webhook URL").fill(webhook);
  await save(discord, "Discord");
  await expect(discord.getByLabel("Replace webhook URL")).toHaveValue("");
  await expect(discord.locator(":scope > summary")).toContainText("Off");
  expect(await page.content()).not.toContain("e2e-secret-token");

  const telegram = await openDestination(page, "telegram");
  await telegram.getByLabel("Bot token").fill("not-a-token");
  await telegram.getByLabel("Chat ID").fill("-100123");
  await telegram.getByLabel("Send alerts to Telegram").check();
  await telegram.getByRole("button", { name: "Save Telegram" }).click();
  await expect(telegram.locator(".alert--error")).toContainText("Enter the bot token from BotFather");
  // The visible field keeps what was entered; the token is not echoed.
  await expect(telegram.getByLabel("Chat ID")).toHaveValue("-100123");
  await expect(telegram.getByLabel("Bot token")).toHaveValue("");
  await telegram.getByLabel("Bot token").fill("123456:e2e-secret-bot-token-abcdefghijklmnop");
  await telegram.getByLabel("Send alerts to Telegram").uncheck();
  await save(telegram, "Telegram");

  await page.reload();
  await expect((await openDestination(page, "discord")).getByLabel("Replace webhook URL")).toHaveValue("");
  // Switching it off was a choice, and it sticks.
  await expect((await openDestination(page, "discord")).getByLabel("Send alerts to Discord")).not.toBeChecked();
  await expect(page.locator("#notification-discord-card > summary")).toContainText("Off");
  await expect((await openDestination(page, "telegram")).getByLabel("Chat ID")).toHaveValue("-100123");
  const content = await page.content();
  expect(content).not.toContain("e2e-secret-token");
  expect(content).not.toContain("e2e-secret-bot-token");
});
