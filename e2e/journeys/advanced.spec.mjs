import { test, expect, asAdmin, admin, signIn } from "./fixtures.mjs";

asAdmin();

async function openAdvanced(page) {
  await page.goto("/settings");
  const advanced = page.locator("#advanced-settings");
  if ((await advanced.getAttribute("open")) === null) {
    await advanced.locator("summary").click();
  }
  return advanced;
}

test("turning verbose logging on and off from Settings", async ({ page }) => {
  await page.goto("/logs");
  await page.getByLabel("Search logs").fill("HTTP request completed");
  await expect(page.locator(".application-log")).toHaveCount(0);

  const advanced = await openAdvanced(page);
  const form = advanced.locator("#logging-form");
  await form.getByLabel("Log extra detail").check();
  await form.getByRole("button", { name: "Save logging" }).click();
  await expect(form.locator(".alert--success")).toHaveText("Verbose logging is on for the next 24 hours.");
  await page.reload();
  await expect(page.locator("#logging-form")).toContainText("On until");

  // Requests are now logged in detail, without restarting Ludock.
  await page.goto("/logs");
  await page.getByLabel("Search logs").fill("HTTP request completed");
  await expect(page.locator(".application-log").first()).toBeVisible();

  const again = await openAdvanced(page);
  await again.locator("#logging-form").getByLabel("Log extra detail").uncheck();
  await again.locator("#logging-form").getByRole("button", { name: "Save logging" }).click();
  await expect(again.locator("#logging-form .alert--success")).toHaveText("Verbose logging is off.");
});

test("trusting a reverse proxy that Ludock noticed", async ({ page, session }) => {
  // The browser stands in for a proxy on the same host, which adds the
  // visitor's address to each request.
  await page.setExtraHTTPHeaders({ "X-Forwarded-For": "198.51.100.7" });
  const advanced = await openAdvanced(page);
  await expect(advanced).toHaveAttribute("open", "");
  const form = advanced.locator("#proxy-form");
  await expect(form.locator(".proxy-hint")).toContainText("You’re reaching Ludock through a proxy at 127.0.0.1 that it doesn’t trust");

  // A value LUDOCK_TRUSTED_PROXIES would refuse is refused here too.
  await form.getByLabel("Trusted proxy addresses").fill("proxy.example:80");
  await form.getByRole("button", { name: "Save proxies" }).click();
  await expect(form.locator(".alert--error")).toContainText("Enter IP addresses or ranges separated by commas");

  await form.getByRole("button", { name: "Trust 127.0.0.1" }).click();
  await expect(form.locator(".alert--success")).toHaveText("Proxy trusted. New requests through it use each visitor’s own address.");
  await expect(form.locator(".proxy-hint")).toHaveCount(0);
  await expect(form.getByLabel("Trusted proxy addresses")).toHaveValue("127.0.0.1");
  await page.reload();
  await expect(page.locator("#proxy-form .proxy-hint")).toHaveCount(0);

  // A visitor signing in through the proxy is now seen at their own address.
  const visitor = await session({ storageState: { cookies: [], origins: [] }, extraHTTPHeaders: { "X-Forwarded-For": "203.0.113.9" } });
  await signIn(visitor, admin.username, admin.password);
  await expect(visitor).toHaveURL("/");
  await page.goto("/account");
  const visitorSession = page.locator(".session-row", { hasText: "203.0.113.9" });
  await expect(visitorSession).toBeVisible();
  await visitorSession.getByRole("button", { name: "Revoke" }).click();
  await expect(visitorSession).toHaveCount(0);

  // Trusting no proxy again puts the hint back for the next forwarded request.
  const reset = await openAdvanced(page);
  await reset.locator("#proxy-form").getByLabel("Trusted proxy addresses").fill("");
  await reset.locator("#proxy-form").getByRole("button", { name: "Save proxies" }).click();
  await expect(reset.locator("#proxy-form .alert--success")).toContainText("No proxy is trusted.");
  await page.reload();
  await expect(page.locator("#proxy-form .proxy-hint")).toContainText("127.0.0.1");
});
