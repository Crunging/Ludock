import { createServer } from "node:http";
import { createServer as createTCPServer } from "node:net";
import { test, expect, asAdmin } from "./fixtures.mjs";

asAdmin();

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
  if (!(await card.evaluate((details) => details.open))) await card.locator("summary").click();
  return card;
}

async function save(card, label) {
  await card.getByRole("button", { name: `Save ${label}` }).click();
  await expect(card.locator("form .alert--success")).toHaveText(`${label} settings saved.`);
}

async function sendTest(page, card, label) {
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
  await expect(card.locator("summary")).toContainText("Off");
}

test("sending test alerts to ntfy, Gotify, email, and a webhook", async ({ page }) => {
  const http = await receiver();
  const mail = await mailServer();
  try {
    await page.goto("/settings");
    await expect(page.locator("#notification-deliveries")).toContainText("Recent deliveries");

    const ntfy = await openDestination(page, "ntfy");
    await expect(ntfy.getByRole("button", { name: "Send test notification" })).toBeDisabled();
    await ntfy.getByLabel("Server URL").fill(http.url + "/ntfy");
    await ntfy.getByLabel("Topic").fill("e2e-secret-topic");
    await ntfy.getByLabel("Access token (optional)").fill("tk_e2esecret");
    await ntfy.getByLabel("Send alerts to ntfy").check();
    await save(ntfy, "ntfy");
    await expect(ntfy.locator("summary")).toContainText("On");
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
    await expect(page.locator("#notification-email-card summary")).toContainText("On");

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
  await expect(discord.getByLabel("Send alerts to Discord")).not.toBeChecked();
  await discord.getByLabel("Webhook URL").fill("https://example.com/not-discord");
  await discord.getByRole("button", { name: "Save Discord" }).click();
  await expect(discord.locator(".alert--error")).toHaveText("Use a Discord HTTPS webhook URL");
  await discord.getByLabel("Webhook URL").fill(webhook);
  await save(discord, "Discord");
  await expect(discord.getByLabel("Replace webhook URL")).toHaveValue("");
  await expect(discord.locator("summary")).toContainText("Off");
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
  await expect((await openDestination(page, "telegram")).getByLabel("Chat ID")).toHaveValue("-100123");
  const content = await page.content();
  expect(content).not.toContain("e2e-secret-token");
  expect(content).not.toContain("e2e-secret-bot-token");
});
