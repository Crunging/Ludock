import { test, expect, ADMIN, RUNNING_ID, RUNNING_NAME } from "./fixtures";

test("server tools return to the remembered detail tab and filtered list", async ({ app, page }) => {
  await app.open();
  const search = page.getByRole("searchbox", { name: "Find a server" });
  await search.fill("survival");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("running");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await page.getByRole("tab", { name: "Schedules", exact: true }).click();
  await page.getByRole("link", { name: "Files", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();

  await page.goBack();
  await expect(page.getByRole("tab", { name: "Schedules", selected: true })).toBeVisible();
  await page.goBack();
  await expect(search).toHaveValue("survival");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("running");
  await expect(page.getByRole("article")).toHaveCount(1);
  await page.goForward();
  await expect(page.getByRole("tab", { name: "Schedules", selected: true })).toBeVisible();
  await page.goForward();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();

  await page.getByRole("link", { name: /Back to server/ }).click();
  await expect(page.getByRole("tab", { name: "Schedules", selected: true })).toBeVisible();
  await page.getByRole("link", { name: "All servers", exact: true }).click();
  await expect(search).toHaveValue("survival");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("running");
  await expect(page.getByRole("article")).toHaveCount(1);
});

test("signing in continues to the requested server page", { tag: "@responsive" }, async ({ app, page }) => {
  app.user = null;
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  await expect(page).toHaveURL(`/files/${RUNNING_ID}`);
  expect(app.requests.filter((request) => request.path !== "/auth/status")).toEqual([]);

  await page.getByRole("textbox", { name: "Username", exact: true }).fill(ADMIN.username);
  await page.getByLabel("Password", { exact: true }).fill("fixture-password-123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(`/files/${RUNNING_ID}`);
  expect(app.requests.filter((request) => request.method !== "GET").map((request) => request.path))
    .toEqual(["/auth/login"]);
});

for (const role of ["operator", "viewer"] as const) {
  test(`${role} direct links to administration return to servers without fetching administrator data`, async ({ app, page }) => {
    app.user = { id: "22222222-2222-4222-8222-222222222222", username: "casey", role };
    app.servers = [{ ...app.servers[0], permissions: ["server.view"] }];
    for (const path of ["/users", "/audit", "/logs", "/diagnostics", "/settings"]) {
      await test.step(path, async () => {
        const requestCount = app.requests.length;
        await app.open(path);
        await expect(page).toHaveURL("/");
        await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
        await expect(page.getByRole("article", { name: RUNNING_NAME })).toBeVisible();
        expect(app.requests.slice(requestCount).filter((request) =>
          request.method !== "GET" || !["/auth/status", "/servers", "/attention"].includes(request.path),
        )).toEqual([]);
      });
    }
  });
}

test("unknown direct links return safely to servers", async ({ app, page }) => {
  for (const path of ["/unknown", `/servers/${RUNNING_ID}/unknown`]) {
    await test.step(path, async () => {
      const requestCount = app.requests.length;
      await app.open(path);
      await expect(page).toHaveURL("/");
      await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
      await expect(page.getByRole("article")).toHaveCount(2);
      expect(app.requests.slice(requestCount).filter((request) =>
        request.method !== "GET" || !["/auth/status", "/servers", "/attention"].includes(request.path),
      )).toEqual([]);
    });
  }
});

test("malformed encoded server history entries return safely to servers", async ({ app, page }) => {
  await app.open();
  await expect(page.getByRole("article")).toHaveCount(2);
  for (const path of ["/servers/%", "/files/%E0%A4%A", "/console/%FF"]) {
    await test.step(path, async () => {
      const requestCount = app.requests.length;
      // The document server rejects malformed escapes before loading the app.
      // Exercise the client route decoder through the browser history instead.
      await page.evaluate((pathname) => {
        window.history.pushState({}, "", pathname);
        window.dispatchEvent(new PopStateEvent("popstate"));
      }, path);
      await expect(page).toHaveURL("/");
      await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
      await expect(page.getByRole("article")).toHaveCount(2);
      expect(app.requests.slice(requestCount).filter((request) =>
        request.method !== "GET" || !["/servers", "/attention"].includes(request.path),
      )).toEqual([]);
    });
  }
});

test("signing out clears the previous account’s remembered list filters", async ({ app, page }) => {
  await app.open();
  await page.getByRole("searchbox", { name: "Find a server" }).fill("factorio");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("exited");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Ludock" })).toBeVisible();
  await page.getByRole("textbox", { name: "Username", exact: true }).fill("another-admin");
  await page.getByLabel("Password", { exact: true }).fill("fixture-password-123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("searchbox", { name: "Find a server" })).toHaveValue("");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("all");
  await expect(page.getByRole("article")).toHaveCount(2);
});

test("an unavailable page download keeps navigation and filters available until explicit reload", async ({ app, page }) => {
  await app.open();
  const search = page.getByRole("searchbox", { name: "Find a server" });
  await search.fill("survival");
  await page.getByRole("combobox", { name: "State", exact: true }).selectOption("running");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await expect(page.getByRole("link", { name: "Files", exact: true })).toBeVisible();
  const sessionReads = app.requests.filter((request) => request.path === "/auth/status").length;
  let failedDownloads = 0;
  await page.route("**/*.js", (route) => {
    failedDownloads += 1;
    return route.abort();
  });
  await page.getByRole("link", { name: "Files", exact: true }).click();
  const main = page.getByRole("main");
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toBeVisible();
  expect(failedDownloads).toBeGreaterThan(0);
  await expect(page.getByRole("navigation", { name: "Primary navigation" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Sign out", exact: true })).toBeVisible();
  await expect(main.getByRole("button", { name: "Reload page", exact: true })).toBeVisible();
  await expect(main).not.toContainText(/Failed to fetch|TypeError|\.js/);

  await main.getByRole("link", { name: "Servers", exact: true }).click();
  await expect(search).toHaveValue("survival");
  await expect(page.getByRole("combobox", { name: "State", exact: true })).toHaveValue("running");
  await expect(page.getByRole("article")).toHaveCount(1);
  expect(app.requests.filter((request) => request.path === "/auth/status")).toHaveLength(sessionReads);

  await page.unroute("**/*.js");
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await page.getByRole("link", { name: "Files", exact: true }).click();
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toBeVisible();
  await main.getByRole("button", { name: "Reload page", exact: true }).click();
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(main.getByRole("heading", { name: "Page unavailable" })).toHaveCount(0);
  expect(app.requests.filter((request) => request.path === "/auth/status")).toHaveLength(sessionReads + 1);
});
