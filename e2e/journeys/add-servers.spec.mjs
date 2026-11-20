import { test, expect, signIn, createUser, serverRow, openServer, shareServer, fake } from "./fixtures.mjs";
import { freshStack } from "../setup.mjs";

// The fake daemon runs "satisfactory" on an image Ludock doesn't recognize,
// "valheim-old" labeled ludock.enable: "false", and "portainer" and
// "factorio" with the Docker socket, beside the four servers it finds by
// itself.

async function openAddServers(page) {
  await page.goto("/");
  await page.getByText("Missing a server?").click();
  const panel = page.locator("#add-servers");
  await expect(panel.getByRole("heading", { name: "Add a server" })).toBeVisible();
  return panel;
}

function candidate(panel, name) {
  return panel.locator(".add-servers__item", { has: panel.page().locator(".add-servers__name").getByText(name, { exact: true }) });
}

test("adding a server Ludock doesn't recognize, from the server list", async ({ session }) => {
  const page = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const panel = await openAddServers(page);
  await expect(page.locator(".server-row")).toHaveCount(4);
  const satisfactory = candidate(panel, "satisfactory");
  await expect(satisfactory).toContainText("ich777/steamcmd:satisfactory");
  await expect(satisfactory).toContainText("Stopped · Ports 7777/udp");
  // The game is suggested from the image's tag.
  await expect(satisfactory.getByLabel("Game for satisfactory")).toHaveValue("satisfactory");
  // Opted out by its owner, in control of Docker, or Ludock itself.
  for (const name of ["valheim-old", "portainer", "ludock-dev"]) {
    await expect(candidate(panel, name)).toHaveCount(0);
  }

  await satisfactory.getByRole("button", { name: "Add satisfactory" }).click();
  await expect(panel.getByRole("status")).toContainText("Added satisfactory.");
  await expect(candidate(panel, "satisfactory")).toHaveCount(0);
  await expect(page.locator(".server-row")).toHaveCount(5);
  const row = serverRow(page, "satisfactory");
  await expect(row).toContainText("Satisfactory");
  await expect(row).toContainText("7777");

  // It works as a server straight away.
  await row.getByRole("button", { name: "Start" }).click();
  await expect(row).toContainText("Running");
  await panel.getByRole("link", { name: "Open it" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "satisfactory" })).toBeVisible();
  await expect(page.locator(".detail-header__game")).toHaveText("Satisfactory");
  await expect(page.getByRole("button", { name: "Remove from Ludock" })).toBeVisible();
});

test("a game that can control the host isn't added, and says how to add it", async ({ session }) => {
  const page = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const panel = await openAddServers(page);
  // Factorio's image is recognized, but it mounts the Docker socket.
  await expect(serverRow(page, "factorio")).toHaveCount(0);
  await expect(candidate(panel.getByRole("list", { name: "Containers you can add" }), "factorio")).toHaveCount(0);
  const skipped = panel.locator("#add-servers-host");
  await expect(skipped.getByRole("heading", { name: "Not added because they can control the host" })).toBeVisible();
  await expect(skipped).toContainText("Whoever uses its console or files could take over this whole machine");
  await expect(skipped).toContainText('add the label ludock.enable: "true" where it is defined');
  const factorio = candidate(skipped, "factorio");
  await expect(factorio).toContainText("factoriotools/factorio:stable");
  await expect(factorio).toContainText("Can control the host");
  await expect(factorio.getByRole("button")).toHaveCount(0);
  // A tool that controls Docker isn't a game and isn't explained.
  await expect(candidate(skipped, "portainer")).toHaveCount(0);
});

test("a container its owner labeled ludock.enable: \"false\" can't be added", async ({ session }) => {
  const page = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const panel = await openAddServers(page);
  await expect(candidate(panel, "valheim-old")).toHaveCount(0);
  // Not even by a request the page never sends.
  const csrf = await page.locator('meta[name="csrf-token"]').getAttribute("content");
  const response = await page.request.post("/servers/add", { form: { _csrf: csrf, container: "valheim-old" },
    headers: { "HX-Request": "true" } });
  expect(await response.text()).toContain("This container can’t be added.");
  await page.reload();
  await expect(page.locator(".server-row")).toHaveCount(5);
  await expect(serverRow(page, "valheim-old")).toHaveCount(0);
});

test("a non-administrator sees no containers and can't add or remove servers", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(admin, "friend1", "friend password one");
  const friend = await session();
  await signIn(friend, "friend1", "friend password one");
  await expect(friend.getByText("No servers shared with you yet")).toBeVisible();
  await expect(friend.getByText("Missing a server?")).toHaveCount(0);
  const csrf = await friend.locator('meta[name="csrf-token"]').getAttribute("content");
  const list = await friend.request.get("/fragments/unmanaged", { headers: { "HX-Request": "true" } });
  expect(list.status()).toBe(403);
  expect(await list.text()).not.toContain("satisfactory");
  const add = await friend.request.post("/servers/add", { form: { _csrf: csrf, container: "portainer" },
    headers: { "HX-Request": "true" } });
  expect(add.status()).toBe(403);

  // Once shared, the added server shows up, without the administrator's controls.
  await shareServer(admin, "satisfactory", "friend1", "Can see it");
  await openServer(friend, "satisfactory");
  await expect(friend.getByRole("heading", { name: "Remove from Ludock" })).toHaveCount(0);
  const remove = await friend.request.post(friend.url() + "/remove", { form: { _csrf: csrf }, headers: { "HX-Request": "true" } });
  expect(remove.status()).toBe(403);
});

test("removing servers and adding them back keeps their sharing", async ({ session }) => {
  const page = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  const friend = await session();
  await signIn(friend, "friend1", "friend password one");
  await expect(serverRow(friend, "satisfactory")).toBeVisible();

  await openServer(page, "satisfactory");
  await page.getByRole("button", { name: "Remove from Ludock" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("Remove satisfactory from Ludock?");
  await dialog.getByRole("button", { name: "Remove server" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.locator(".server-row")).toHaveCount(4);
  await friend.reload();
  await expect(friend.getByText("No servers shared with you yet")).toBeVisible();
  // The friend's share waits for the server, out of sight meanwhile.
  await page.goto("/users");
  await expect(page.locator(".user-row", { hasText: "friend1" })).toContainText("Nothing shared yet");

  // The container is untouched and offered again; adding it brings back its
  // sharing.
  let panel = await openAddServers(page);
  const satisfactory = candidate(panel, "satisfactory");
  await expect(satisfactory).toContainText("Running");
  await satisfactory.getByRole("button", { name: "Add satisfactory" }).click();
  await expect(panel.getByRole("status")).toContainText("Added satisfactory.");
  await friend.reload();
  await expect(serverRow(friend, "satisfactory")).toBeVisible();

  // A server Ludock found by itself can be removed too, and its game is
  // suggested when it is offered again.
  await openServer(page, "enshrouded");
  await page.getByRole("button", { name: "Remove from Ludock" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove server" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(serverRow(page, "enshrouded")).toHaveCount(0);
  panel = await openAddServers(page);
  const enshrouded = candidate(panel, "enshrouded");
  await expect(enshrouded.getByLabel("Game for enshrouded")).toHaveValue("enshrouded");
  await enshrouded.getByRole("button", { name: "Add enshrouded" }).click();
  await expect(serverRow(page, "enshrouded")).toContainText("Enshrouded");

  // Leave the fleet as other journeys expect it.
  await openServer(page, "satisfactory");
  await page.getByRole("button", { name: "Remove from Ludock" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove server" }).click();
  await expect(page.locator(".server-row")).toHaveCount(4);
  await page.goto("/users");
  await page.locator(".user-row", { hasText: "friend1" }).getByRole("button", { name: "Delete" }).click();
  await page.getByRole("dialog", { name: "Delete friend1?" }).getByRole("button", { name: "Delete user" }).click();
  await expect(page.locator("#users .alert--success")).toHaveText("Deleted friend1.");
});

test.describe("a game its owner labeled in although it can control the host", () => {
  // Labeling factorio in adds a server, so this runs on an install of its own.
  let stack;
  test.beforeAll(async ({}, info) => {
    stack = await freshStack(info.parallelIndex);
  });
  test.afterAll(async () => {
    await stack?.stop();
  });

  test("is managed, and its Sharing tab warns what sharing hands over", async ({ session }) => {
    const page = await session({ baseURL: stack.env.LUDOCK_E2E_URL, storageState: stack.env.LUDOCK_E2E_ADMIN_STATE });
    await fake("/fake/recreate", { container: "factorio", label: "ludock.enable=true" }, stack.env.LUDOCK_E2E_DIR);
    await page.goto("/");
    await expect(serverRow(page, "factorio")).toContainText("Factorio");
    await openServer(page, "factorio");
    await page.getByRole("tab", { name: "Sharing" }).click();
    const warning = page.locator("#sharing-host-warning");
    await expect(warning).toContainText("This container can control the host");
    await expect(warning).toContainText("Share it only as Can see it, or with people you would trust with the host.");
    // An ordinary server carries no warning.
    await openServer(page, "Survival");
    await page.getByRole("tab", { name: "Sharing" }).click();
    await expect(page.locator("#sharing")).toBeVisible();
    await expect(page.locator("#sharing-host-warning")).toHaveCount(0);
  });
});
