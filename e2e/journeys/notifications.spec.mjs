import { createServer, request } from "node:http";
import { createServer as createTCPServer } from "node:net";
import { join } from "node:path";
import { test, expect, asAdmin, dockerAction, expectOperationSucceeded, openServer } from "./fixtures.mjs";

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

async function sendTest(page, card, label) {
  await openDeliveries(page);
  await card.getByRole("button", { name: "Send test notification" }).click();
  await expect(card.locator(".notification-destination__test .alert--success")).toContainText("Test notification queued.");
  const row = page.locator(".notification-deliveries-table tbody tr", { hasText: "Test notification" })
    .filter({ has: page.getByRole("cell", { name: label, exact: true }) }).first();
  await expect(row).toContainText("Delivered", { timeout: 30_000 });
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
    await sendTest(page, ntfy, "ntfy");
    const published = http.requests.find((request) => request.url === "/ntfy");
    expect(published.headers.authorization).toBe("Bearer tk_e2esecret");
    expect(published.body).toEqual({ topic: "e2e-secret-topic", title: "Ludock", message: "Ludock test notification: ntfy delivery is working." });

    const gotify = await openDestination(page, "gotify");
    await gotify.getByLabel("Server URL").fill(http.url + "/gotify/");
    await gotify.getByLabel("Application token").fill("Ae2esecret");
    await gotify.getByLabel("Send alerts to Gotify").check();
    await save(gotify, "Gotify");
    await sendTest(page, gotify, "Gotify");
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
    await sendTest(page, email, "Email");
    expect(mail.messages).toHaveLength(1);
    expect(mail.messages[0]).toContain("Subject: Ludock: Ludock test notification: Email delivery is working.");
    expect(mail.messages[0]).toContain("To: <admin@example.com>, <ops@example.com>");

    const webhook = await openDestination(page, "webhook");
    await webhook.getByLabel("Webhook URL").fill(http.url + "/hook?token=e2e-secret-query");
    await webhook.getByLabel("Authorization header (optional)").fill("Bearer e2e-secret-header");
    await webhook.getByLabel("Send alerts to the webhook").check();
    await save(webhook, "Webhook");
    await sendTest(page, webhook, "Webhook");
    const hook = http.requests.find((request) => request.url.startsWith("/hook"));
    expect(hook.headers.authorization).toBe("Bearer e2e-secret-header");
    expect(hook.body).toMatchObject({ source: "ludock", kind: "test", message: "Ludock test notification: Webhook delivery is working." });

    // Removing the optional header keeps the URL.
    await webhook.getByLabel("Remove the saved Authorization header").check();
    await save(webhook, "Webhook");
    await expect(webhook.getByLabel("Remove the saved Authorization header")).toHaveCount(0);
    await expect(webhook.getByLabel("Replace webhook URL")).toBeVisible();

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

test("retrying a failed webhook delivery", async ({ page }) => {
  const http = await receiver();
  http.statuses["/status/503"] = 503;
  try {
    await page.goto("/settings");
    const webhook = await openDestination(page, "webhook");
    await webhook.locator("input[name=url]").fill(http.url + "/status/503");
    await webhook.getByLabel("Send alerts to the webhook").check();
    await save(webhook, "Webhook");
    await webhook.getByRole("button", { name: "Send test notification" }).click();

    await openDeliveries(page);
    const row = page.locator(".notification-deliveries-table tbody tr", { hasText: "Test notification" })
      .filter({ has: page.getByRole("cell", { name: "Webhook", exact: true }) }).first();
    await expect(row).toContainText("Pending retry", { timeout: 30_000 });
    await expect(row).toContainText("Delivery to the webhook failed because it is temporarily unavailable.");

    http.statuses["/status/503"] = 204;
    await row.getByRole("button", { name: "Retry Webhook notification" }).click();
    await expect(page.locator("#notification-deliveries .alert--success")).toContainText("Notification queued for retry.");
    await expect(row).toContainText("Delivered", { timeout: 30_000 });
    expect(http.requests.filter((request) => request.url === "/status/503")).toHaveLength(2);

    await turnOff(page, "webhook", "Webhook");
  } finally {
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

async function openAlertTypes(card) {
  const types = card.locator(".notification-alerts");
  if (!(await types.evaluate((details) => details.open))) await types.locator("summary").click();
  return types;
}

/** Publishes a new version of Survival's image tag in the fake daemon's registry. */
function publishSurvivalImage(digit) {
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: "/fake/registry?" + new URLSearchParams({ image: "itzg/minecraft-server:java21", id: "sha256:" + digit.repeat(64) }) }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

/**
 * Checks Survival's image until a check newer than the last one finds a newer
 * image; checks run at most every 10 seconds.
 */
async function findNewerSurvivalImage(page) {
  await openServer(page, "Survival");
  await page.getByRole("tab", { name: "Update" }).click();
  const check = page.locator("#image-check");
  const checked = check.locator(".image-check__checked time");
  await expect(check.getByRole("button", { name: "Check now" })).toBeVisible();
  const before = await checked.count() ? await checked.getAttribute("datetime") : null;
  await expect(async () => {
    await check.getByRole("button", { name: "Check now" }).click();
    await expect(check).toContainText("A newer image is available.", { timeout: 2_000 });
    expect(await checked.getAttribute("datetime", { timeout: 2_000 })).not.toBe(before);
  }).toPass({ timeout: 30_000, intervals: [1_000, 3_000] });
}

// A destination gets server problems but not newer images until that type
// is switched on; the panel shows newer images either way.
test("choosing which types of alert reach a webhook", async ({ page }) => {
  test.setTimeout(240_000);
  const http = await receiver();
  const messages = () => http.requests.filter((r) => r.url === "/alerts").map((r) => r.body.message);
  const newerImage = /^Survival: a newer image is available/;
  try {
    await page.goto("/settings");
    let webhook = await openDestination(page, "webhook");
    await webhook.locator("input[name=url]").fill(http.url + "/alerts");
    await webhook.getByLabel("Send alerts to the webhook").check();
    const types = webhook.locator(".notification-alerts");
    await expect(types.locator("summary")).toHaveText("Alert types Everything except newer images");
    await expect(types.getByLabel("Server problems")).toBeHidden();
    await save(webhook, "Webhook");

    await publishSurvivalImage("5");
    await findNewerSurvivalImage(page);

    // Enshrouded goes down behind Ludock's back after a start through Ludock.
    await openServer(page, "enshrouded");
    // Watched by default; a short wait makes the alert quick.
    await expect(page.getByLabel("Monitor this server")).toBeChecked();
    await page.getByText("Alert levels").click();
    const saved = page.waitForResponse((response) => response.request().method() === "POST" && response.url().endsWith("/availability"));
    await page.getByLabel("Wait before alerting (seconds)").fill("11");
    await page.getByLabel("Wait before alerting (seconds)").press("Tab");
    expect((await saved).ok()).toBe(true);
    await page.locator(".detail-actions").getByRole("button", { name: "Start" }).click();
    await expect(page.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
    await dockerAction("enshrouded", "stop");
    await expect.poll(messages, { timeout: 90_000 }).toContain("enshrouded: the server is down.");
    // Recent deliveries names each alert by its type.
    await page.goto("/settings");
    await expect(page.locator(".notification-deliveries-table tbody tr").first()).toContainText("Server problems");
    await expect(page.locator(".notification-deliveries-table")).not.toContainText("Server notification");
    // Alerts are delivered in the order they were queued, so a newer-image
    // notice would have arrived first.
    expect(messages().filter((m) => newerImage.test(m))).toEqual([]);

    await page.goto("/settings");
    webhook = await openDestination(page, "webhook");
    await openAlertTypes(webhook);
    await webhook.getByLabel("Newer images available").check();
    await webhook.getByLabel("Server problems").uncheck();
    await save(webhook, "Webhook");
    await expect(webhook.locator(".notification-alerts summary")).toHaveText(
      "Alert types Failed backups and schedules, Restores and updates, Newer images available");

    await publishSurvivalImage("6");
    await findNewerSurvivalImage(page);
    await expect.poll(() => messages().filter((m) => newerImage.test(m)).length, { timeout: 30_000 }).toBe(1);
    // A test still reaches it, whatever types are chosen.
    await page.goto("/settings");
    webhook = await openDestination(page, "webhook");
    await sendTest(page, webhook, "Webhook");

    // Saving with no alert type is refused.
    await openAlertTypes(webhook);
    for (const label of ["Failed backups and schedules", "Restores and updates", "Newer images available"]) {
      await webhook.getByLabel(label).uncheck();
    }
    await webhook.getByRole("button", { name: "Save Webhook" }).click();
    await expect(webhook.locator(".alert--error")).toHaveText("Choose at least one alert type");
    await expect(webhook.getByLabel("Server problems")).not.toBeChecked();
  } finally {
    // A stop through Ludock leaves it watched but quiet for later journeys.
    await dockerAction("enshrouded", "start");
    await openServer(page, "enshrouded");
    await page.locator(".detail-actions").getByRole("button", { name: "Stop" }).click();
    await page.getByRole("dialog").getByRole("button", { name: "Stop server" }).click();
    await expectOperationSucceeded(page);
    await page.goto("/settings");
    const webhook = await openDestination(page, "webhook");
    await openAlertTypes(webhook);
    await webhook.getByLabel("Server problems").check();
    await webhook.getByLabel("Newer images available").uncheck();
    await webhook.locator("input[name=enabled]").uncheck();
    await save(webhook, "Webhook");
    http.close();
  }
});
