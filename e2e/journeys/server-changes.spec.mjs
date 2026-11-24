import { request } from "node:http";
import { join } from "node:path";
import { test, expect, signIn, createUser, serverRow, openServer, shareServer } from "./fixtures.mjs";

/** Recreates a container with changed environment variables and labels, as Portainer or Compose would; a bare KEY removes it. */
function recreate(container, { env = [], label = [] }) {
  const query = new URLSearchParams({ container });
  for (const entry of env) query.append("env", entry);
  for (const entry of label) query.append("label", entry);
  return new Promise((resolve, reject) => {
    const call = request({ socketPath: join(process.env.LUDOCK_E2E_DIR, "docker", "docker.sock"), method: "POST",
      path: "/fake/recreate?" + query }, (response) => {
      response.resume();
      response.on("end", resolve);
    });
    call.on("error", reject);
    call.end();
  });
}

async function deleteUser(adminPage, username) {
  await adminPage.goto("/users");
  await adminPage.locator(".user-row", { hasText: username }).getByRole("button", { name: "Delete" }).click();
  await adminPage.getByRole("dialog", { name: `Delete ${username}?` }).getByRole("button", { name: "Delete user" }).click();
  await expect(adminPage.locator("#users .alert--success")).toHaveText(`Deleted ${username}.`);
}

test("changing a password or label in Docker leaves a server as it was in Ludock", async ({ session }) => {
  const adminPage = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(adminPage, "steady", "steady password one");
  await shareServer(adminPage, "Survival", "steady", ["Send console commands"]);
  const operator = await session();
  await signIn(operator, "steady", "steady password one");
  try {
    // Another password, the console password set in the environment, and a
    // new Ludock label.
    await recreate("minecraft", { env: ["DB_PASS=rotated-db-secret", "RCON_PASSWORD=dev-rcon-secret"],
      label: ["ludock.console.password-env=RCON_PASSWORD"] });

    // The console reaches the new container, so Ludock has seen it.
    await operator.goto("/");
    await serverRow(operator, "Survival").getByRole("link", { name: "Console" }).click();
    await expect(operator.locator("[data-console-status]")).toHaveText("Connected");
    await operator.getByLabel("Game command").fill("list");
    await operator.getByRole("button", { name: "Send" }).click();
    await expect(operator.locator(".xterm-rows")).toContainText("There are 0 of a max of 10 players online:");

    await openServer(adminPage, "Survival");
    await expect(adminPage.getByRole("heading", { name: "This server changed in Docker" })).toHaveCount(0);
    // Needs attention shows nothing at all when nothing is wrong.
    const loaded = adminPage.waitForResponse((response) => response.url().includes("/fragments/attention"));
    await adminPage.goto("/");
    await loaded;
    await expect(adminPage.locator("#attention")).not.toContainText("Server changed");
  } finally {
    await recreate("minecraft", { env: ["DB_PASS=dev-db-secret", "RCON_PASSWORD"], label: ["ludock.console.password-env"] });
    await deleteUser(adminPage, "steady");
  }
});

test("a new game pauses shared access until an administrator confirms it", async ({ session }) => {
  const adminPage = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(adminPage, "friend", "friend password one");
  await shareServer(adminPage, "enshrouded", "friend", ["Restart", "Set up their own schedules"]);
  const friend = await session();
  await signIn(friend, "friend", "friend password one");
  await friend.getByRole("link", { name: "enshrouded", exact: true }).click();
  const serverURL = friend.url();
  await friend.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
  await friend.locator("#schedule-form").getByRole("button", { name: "Add schedule" }).click();
  await expect(friend.locator("#schedule-list tbody tr")).toHaveCount(1);
  let friendDeleted = false;
  try {
    await recreate("enshrouded", { label: ["ludock.game=valheim"] });

    // The administrator keeps full use of the server and sees why sharing
    // is paused.
    // Pages reuse discovery for two seconds; Docker's events refresh it.
    await expect(async () => {
      await adminPage.goto("/");
      await expect(serverRow(adminPage, "enshrouded")).toContainText("Changed in Docker. Sharing is paused until you confirm.", { timeout: 1_000 });
    }).toPass({ timeout: 15_000 });
    await expect(adminPage.locator(".needs-attention")).toContainText("Server changed");
    await openServer(adminPage, "enshrouded");
    const notice = adminPage.locator(".notice-panel");
    await expect(notice.getByRole("heading", { name: "This server changed in Docker" })).toBeVisible();
    await expect(notice).toContainText("Its game or data folders changed.");
    await expect(adminPage.locator(".detail-actions").getByRole("button", { name: "Start" })).toBeEnabled();
    await expect(adminPage.locator(".section-tabs").getByRole("link")).toContainText(["Overview", "Activity", "Schedules"]);
    await adminPage.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    await expect(adminPage.locator("#schedule-list")).toContainText("The server’s game or data folders changed.");

    // The friend loses the server until then.
    await friend.goto(serverURL);
    await expect(friend.getByText("Server not found")).toBeVisible();
    await friend.goto("/");
    await expect(friend.getByText("No servers shared with you yet")).toBeVisible();

    await adminPage.locator(".section-tabs").getByRole("link", { name: "Overview" }).click();
    await notice.getByRole("button", { name: "Confirm and restore access" }).click();
    await expect(adminPage.locator(".notice-panel")).toHaveCount(0);

    // The friend's server and schedule come back as they were.
    await friend.goto(serverURL);
    await expect(friend.getByRole("heading", { level: 1, name: "enshrouded" })).toBeVisible();
    await friend.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    const schedule = friend.locator("#schedule-list tbody tr");
    await expect(schedule).toHaveCount(1);
    await expect(schedule).not.toContainText("changed");
    await expect(schedule).toContainText("Restart");

    // Deleting the friend stops their schedule until an administrator saves
    // it, which makes it theirs.
    await adminPage.goto("/users");
    await adminPage.locator(".user-row", { hasText: "friend" }).getByRole("button", { name: "Delete" }).click();
    const confirm = adminPage.getByRole("dialog", { name: "Delete friend?" });
    await expect(confirm).toContainText("Their schedule stops until an administrator saves it again.");
    await confirm.getByRole("button", { name: "Delete user" }).click();
    await expect(adminPage.locator("#users .alert--success")).toHaveText("Deleted friend.");
    friendDeleted = true;
    await openServer(adminPage, "enshrouded");
    await adminPage.locator(".section-tabs").getByRole("link", { name: "Schedules" }).click();
    const orphan = adminPage.locator("#schedule-list tbody tr");
    await expect(orphan).toContainText("Set up by a deleted account");
    await expect(orphan).toContainText("Its owner’s account was deleted.");
    await expect(orphan.locator("td").nth(2)).toHaveText("Suspended");
    await orphan.getByRole("link", { name: "Edit" }).click();
    await expect(adminPage.locator("#schedule-form")).toContainText("saving makes it yours");
    await adminPage.locator("#schedule-form").getByRole("button", { name: "Save changes" }).click();
    await expect(orphan).not.toContainText("deleted");
    await expect(orphan).not.toContainText("Set up by");
    await orphan.getByRole("button", { name: "Delete" }).click();
    await adminPage.getByRole("dialog", { name: "Delete this schedule?" }).getByRole("button", { name: "Delete schedule" }).click();
    await expect(adminPage.locator("#schedule-list")).toContainText("No schedules.");
  } finally {
    if (!friendDeleted) await deleteUser(adminPage, "friend");
    await recreate("enshrouded", { label: ["ludock.game"] });
  }
});
