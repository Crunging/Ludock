import { test, expect, signIn, createUser, serverRow, openServer, shareServer, chooseAccess } from "./fixtures.mjs";

async function openSharing(page, server) {
  await openServer(page, server);
  await page.getByRole("tab", { name: "Sharing" }).click();
  await expect(page).toHaveURL(/\?tab=sharing$/);
  return page.locator("#sharing");
}

const shareRow = (sharing, username) => sharing.locator(".share-row", { has: sharing.page().getByText(username, { exact: true }) });

test("sharing a server with someone new by invite link, and what they can see", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const sharing = await openSharing(admin, "Survival");
  const form = sharing.locator("form.share-form");
  // The person list appears once other members exist.
  if (await form.getByLabel("Person").count()) await form.getByLabel("Person").selectOption("new");
  await form.getByLabel("Their username").fill("pal");
  await expect(form.getByLabel("Access")).toHaveValue("run");
  await chooseAccess(form, "Can see it");
  await expect(form.getByText("Whether it’s running, who is online, and the address to join.")).toBeVisible();
  await form.getByRole("button", { name: "Share" }).click();
  await expect(sharing.locator(".alert--success")).toHaveText("Shared with pal. Send them this link to choose a password and sign in.");
  const result = sharing.getByRole("region", { name: "Invite link for pal" });
  const link = await result.getByRole("textbox").inputValue();
  await expect(result.getByRole("button", { name: "Copy link" })).toBeVisible();
  await expect(shareRow(sharing, "pal").getByLabel("Access for pal")).toHaveValue("see");

  // The friend chooses a password and finds only that server.
  const pal = await session();
  await pal.goto(link);
  await pal.getByLabel("New password").fill("pal chose this password");
  await pal.getByLabel("Confirm password").fill("pal chose this password");
  await pal.getByRole("button", { name: "Save and sign in" }).click();
  await expect(pal.locator(".server-row")).toHaveCount(1);
  const row = serverRow(pal, "Survival");
  await expect(row.getByRole("button", { name: "Copy address for Survival" })).toBeVisible();
  await expect(row.getByRole("button", { name: "Stop" })).toHaveCount(0);
  await expect(row.getByRole("link", { name: "Console" })).toHaveCount(0);
  await pal.getByRole("link", { name: "Survival", exact: true }).click();
  await expect(pal.getByRole("tab")).toHaveText(["Overview", "Activity"]);
  await expect(pal.locator(".detail-actions")).toHaveCount(0);

  // Requests the page doesn't offer are refused.
  const id = pal.url().match(/\/servers\/([^/?]+)/)[1];
  const csrf = await pal.locator('meta[name="csrf-token"]').getAttribute("content");
  const headers = { "HX-Request": "true" };
  for (const action of ["start", "stop", "restart", "sharing"]) {
    const response = await pal.request.post(`/servers/${id}/${action}`, { form: { _csrf: csrf, person: "new", username: "sneaky", level: "full" }, headers });
    expect(response.status(), action).toBe(403);
  }
  await pal.goto("/users");
  await expect(pal).toHaveURL(/\/$/);
  await expect(pal.getByRole("link", { name: "Users" })).toHaveCount(0);
  expect((await pal.request.get("/servers/00000000-0000-4000-8000-000000000000")).status()).toBe(404);
});

test("changing someone's access and stopping sharing from the server's page", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const pal = await session();
  await signIn(pal, "pal", "pal chose this password");
  await pal.getByRole("link", { name: "Survival", exact: true }).click();
  const survivalURL = pal.url();

  // Can run it: start, stop, restart, the console, and logs.
  let sharing = await openSharing(admin, "Survival");
  let row = shareRow(sharing, "pal");
  await chooseAccess(row, "Can run it");
  await row.getByRole("button", { name: "Save" }).click();
  await expect(sharing.locator(".alert--success")).toHaveText("Changed pal’s access to “Can run it”.");
  await pal.reload();
  await expect(pal.getByRole("tab")).toHaveText(["Overview", "Activity", "Update"]);
  await expect(pal.locator(".detail-actions").getByRole("button", { name: "Stop" })).toBeVisible();
  await expect(pal.locator(".detail-actions").getByRole("link", { name: "Console" })).toBeVisible();
  await expect(pal.locator(".detail-actions").getByRole("link", { name: "Files" })).toHaveCount(0);
  await expect(pal.getByLabel("Monitor this server")).toHaveCount(0);
  await expect(pal.getByText("Ask an administrator")).toHaveCount(0);

  // Full access adds files, backups, and schedules, never sharing.
  sharing = await openSharing(admin, "Survival");
  row = shareRow(sharing, "pal");
  await chooseAccess(row, "Full access");
  await row.getByRole("button", { name: "Save" }).click();
  await expect(sharing.locator(".alert--success")).toHaveText("Changed pal’s access to “Full access”.");
  await pal.reload();
  await expect(pal.getByRole("tab")).toHaveText(["Overview", "Activity", "Backups", "Schedules", "Update"]);
  await expect(pal.locator(".detail-actions").getByRole("link", { name: "Files" })).toBeVisible();

  // The users page lists what each person can use.
  await admin.goto("/users");
  const user = admin.locator(".user-row", { has: admin.getByText("pal", { exact: true }) });
  await expect(user.getByRole("list", { name: "Servers shared with pal" })).toHaveText("Survival Full access");

  // Stopping takes effect immediately, even for a page already open.
  sharing = await openSharing(admin, "Survival");
  await shareRow(sharing, "pal").getByRole("button", { name: "Stop sharing" }).click();
  await admin.getByRole("dialog", { name: "Stop sharing Survival with pal?" }).getByRole("button", { name: "Stop sharing" }).click();
  await expect(sharing.locator(".alert--success")).toHaveText("Stopped sharing with pal.");
  await expect(shareRow(sharing, "pal")).toHaveCount(0);
  await expect(sharing.locator("form.share-form").getByLabel("Person").locator("option", { hasText: "pal" })).toHaveCount(1);
  await pal.getByRole("tab", { name: "Activity" }).click();
  await expect(pal.getByText("Server not found")).toBeVisible();
  await pal.goto("/");
  await expect(pal.getByText("No servers shared with you yet")).toBeVisible();
  await pal.goto(survivalURL);
  await expect(pal.getByText("Server not found")).toBeVisible();
  await admin.goto("/users");
  await expect(user).toContainText("Nothing shared yet");

  // Disabling the account signs them out.
  await user.getByRole("button", { name: "Disable" }).click();
  await expect(user).toContainText("Disabled");
  await pal.goto("/");
  await expect(pal).toHaveURL(/\/login/);
  await user.getByRole("button", { name: "Delete" }).click();
  await admin.getByRole("dialog", { name: "Delete pal?" }).getByRole("button", { name: "Delete user" }).click();
  await expect(admin.locator("#users .alert--success")).toHaveText("Deleted pal.");
});

test("a custom share gives exactly the actions chosen", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(admin, "picky", "picky password one");
  await shareServer(admin, "Survival", "picky", ["Read logs"]);
  const row = shareRow(admin.locator("#sharing"), "picky");
  await expect(row.getByLabel("Access for picky")).toHaveValue("custom");
  await expect(row.getByLabel("See the server")).toBeChecked();
  await expect(row.getByLabel("Read logs")).toBeChecked();
  await expect(row.getByLabel("Start", { exact: true })).not.toBeChecked();

  const picky = await session();
  await signIn(picky, "picky", "picky password one");
  await picky.getByRole("link", { name: "Survival", exact: true }).click();
  await expect(picky.locator(".detail-actions").getByRole("link", { name: "Logs" })).toBeVisible();
  await expect(picky.locator(".detail-actions").getByRole("button")).toHaveCount(0);
  await admin.goto("/users");
  await expect(admin.getByRole("list", { name: "Servers shared with picky" })).toHaveText("Survival Custom");
});

test("making someone an administrator and back starts them with nothing shared", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(admin, "deputy", "deputy password one");
  await shareServer(admin, "valheim", "deputy", "Full access");
  await admin.goto("/users");
  const user = admin.locator(".user-row", { has: admin.getByText("deputy", { exact: true }) });
  await user.getByRole("button", { name: "Make administrator" }).click();
  await admin.getByRole("dialog", { name: "Make deputy an administrator?" }).getByRole("button", { name: "Make administrator" }).click();
  await expect(user).toContainText("Administrator · every server and setting");

  const deputy = await session();
  await signIn(deputy, "deputy", "deputy password one");
  await expect(deputy.getByRole("link", { name: "Users" })).toBeVisible();
  await expect(serverRow(deputy, "Survival")).toBeVisible();

  await user.getByRole("button", { name: "Make member" }).click();
  await admin.getByRole("dialog", { name: "Make deputy a member?" }).getByRole("button", { name: "Make member" }).click();
  await expect(user).toContainText("Nothing shared yet");
  await deputy.goto("/");
  await expect(deputy.getByText("No servers shared with you yet")).toBeVisible();
});
