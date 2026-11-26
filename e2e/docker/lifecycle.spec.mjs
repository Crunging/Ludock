// The built image against real Docker: the shell and logs, image updates,
// Ludock updating itself, cleanup when Ludock starts, and account recovery.
import { spawn, spawnSync } from "node:child_process";
import { closeSync, openSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { test, expect, asAdmin, admin, openServer, expectOperationSucceeded, expectOffline } from "../journeys/fixtures.mjs";
import { docker, gameImage, ludock, game, stack, gameContainer, healthy, ludockImage, nextImage, unhealthyImage } from "../docker-setup.mjs";

asAdmin();

test("the shell runs commands and logs follow the container", async ({ page }) => {
  await openServer(page, "Test Game");
  await page.locator(".detail-actions").getByRole("link", { name: /^(Console|Logs)$/ }).click();
  await page.getByRole("navigation", { name: "Console mode" }).getByRole("link", { name: "Logs", exact: true }).click();
  await expect(page.locator(".xterm-rows")).toContainText("tick ");
  await page.getByRole("navigation", { name: "Console mode" }).getByRole("link", { name: "Shell", exact: true }).click();
  await expect(page.locator("[data-console-status]")).toHaveText("Connected");
  await page.getByLabel("Shell command").fill("cat /data/server.properties");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.locator(".xterm-rows")).toContainText("motd=Hello from the container test");
});

test("an update recreates the server with its settings and unnamed volume", async ({ page }) => {
  const before = gameContainer();
  docker("exec", before, "sh", "-c", "echo kept > /state/marker");
  const inspect = (id) => JSON.parse(docker("inspect", id))[0];
  const old = inspect(before);
  await openServer(page, "Test Game");
  await page.locator(".section-tabs").getByRole("link", { name: "Update" }).click();
  const form = page.locator("#update-panel form");
  // A console command may hold the server for a moment; check again.
  const checkAgain = page.locator("#update-panel").getByRole("button", { name: "Check again" });
  await expect(async () => {
    if (await checkAgain.count()) await checkAgain.click();
    await expect(form).toContainText(gameImage, { timeout: 2_000 });
  }).toPass();
  await form.locator("summary", { hasText: "Advanced" }).click();
  await form.getByLabel("Update even if the image is already up to date").check();
  await form.getByLabel("Back up first").uncheck();
  await form.getByLabel(/Type Test Game to skip the backup/).fill("Test Game");
  await form.getByRole("button").last().click();
  await page.getByRole("dialog", { name: "Update Test Game?" }).getByRole("button", { name: /^Update/ }).click();
  await expectOperationSucceeded(page);
  await expect.poll(gameContainer).not.toBe(before);
  const replacement = inspect(gameContainer());
  expect(replacement.State.Running).toBe(true);
  expect(replacement.Name).toBe(old.Name);
  expect(replacement.Config.Cmd).toEqual(old.Config.Cmd);
  // Compose's image label is checked by asking Compose below.
  const labels = ({ Config: { Labels } }) => ({ ...Labels, "com.docker.compose.image": undefined, "ludock.update": undefined });
  expect(labels(replacement)).toEqual(labels(old));
  expect(replacement.Config.Labels["ludock.update"]).toMatch(/^[0-9a-f-]{36}$/);
  expect(Object.keys(replacement.NetworkSettings.Networks)).toEqual(Object.keys(old.NetworkSettings.Networks));
  const volume = (container) => container.Mounts.find((mount) => mount.Destination === "/state").Name;
  expect(volume(replacement)).toBe(volume(old));
  expect(docker("exec", replacement.Id, "cat", "/state/marker")).toBe("kept");
  expect(docker("ps", "--all", "--quiet", "--filter", `id=${before}`)).toBe("");
  // Compose sees the replacement as its own, up-to-date container.
  const plan = spawnSync("docker", [...game(), "up", "--detach", "--dry-run", "game"], { encoding: "utf8" });
  expect(plan.status).toBe(0);
  expect(plan.stdout + plan.stderr).not.toMatch(/Recreat/);
});

// A registry on the host, which Docker trusts over plain HTTP on localhost.
const registry = "localhost:5000/ludock";

/** Points the registry's e2e tag at what another of its tags holds, leaving local tags alone. */
async function publish(from) {
  const accept = ["application/vnd.oci.image.index.v1+json", "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json"].join(", ");
  const manifest = await fetch(`http://127.0.0.1:5000/v2/ludock/manifests/${from}`, { headers: { Accept: accept } });
  expect(manifest.ok).toBe(true);
  const put = await fetch("http://127.0.0.1:5000/v2/ludock/manifests/e2e", { method: "PUT",
    headers: { "Content-Type": manifest.headers.get("content-type") }, body: Buffer.from(await manifest.arrayBuffer()) });
  expect(put.status).toBe(201);
}

const ludockContainers = () => docker("ps", "--all", "--quiet", "--no-trunc", "--filter", "label=com.docker.compose.project=ludocktest-e2e-ludock",
  "--filter", "label=com.docker.compose.service=ludock").split("\n").filter(Boolean);
const inspect = (id) => JSON.parse(docker("inspect", id))[0];
const mounts = (container) => container.Mounts.map((mount) => `${mount.Type} ${mount.Name || mount.Source} ${mount.Destination}`).sort();
const composeLabels = (container) => Object.fromEntries(Object.entries(container.Config.Labels)
  .filter(([key]) => key.startsWith("com.docker.compose.") && key !== "com.docker.compose.image"));

/** Presses Update Ludock once Settings offers it, and confirms. */
async function updateLudock(page) {
  const check = page.locator("#ludock-check");
  const notice = page.locator("#ludock-notice");
  // Manual checks are 10 seconds apart.
  await expect(async () => {
    await page.goto("/settings");
    const advanced = page.locator("#advanced-settings");
    if (!(await advanced.evaluate((details) => details.open))) await advanced.locator(":scope > summary").click();
    await check.getByRole("button", { name: "Check now" }).click({ timeout: 2_000 });
    await expect(notice.getByRole("button", { name: "Update Ludock" })).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 60_000, intervals: [1_000, 3_000] });
  expectOffline(page);
  await notice.getByRole("button", { name: "Update Ludock" }).click();
  await page.getByRole("dialog", { name: "Update Ludock?" }).getByRole("button", { name: "Update Ludock" }).click();
  await expect(notice).toContainText("Ludock is updating.");
  return notice;
}

// Ludock runs from a tag in a registry, which then points to a build that
// reports another version. Update Ludock replaces its container through a
// helper and the page comes back on the new version, with the game left
// running. A version whose health check never passes is then put back.
test("Ludock updates itself, and puts itself back when the new version fails", async ({ page }, testInfo) => {
  test.skip(!nextImage || !unhealthyImage, "LUDOCK_TEST_NEXT_IMAGE and LUDOCK_TEST_UNHEALTHY_IMAGE name second builds of the image");
  test.setTimeout(600_000);
  // Recorded as they happen: Docker keeps only its last 256 events, which
  // the game's health checks fill within minutes.
  const eventsFile = testInfo.outputPath("container-events.txt");
  const eventsOut = openSync(eventsFile, "w");
  const events = spawn("docker", ["events", "--filter", "type=container",
    ...["create", "start", "die", "kill", "stop", "destroy", "rename", "health_status"].flatMap((event) => ["--filter", `event=${event}`]),
    "--format", "{{.TimeNano}} {{.Action}} {{.Actor.ID}} {{.Actor.Attributes.name}} {{.Actor.Attributes.image}} exit={{.Actor.Attributes.exitCode}}"],
  { stdio: ["ignore", eventsOut, eventsOut] });
  docker("run", "--detach", "--name", "ludocktest-registry", "--publish", "127.0.0.1:5000:5000", "registry:2");
  try {
    await expect.poll(async () => (await fetch("http://127.0.0.1:5000/v2/").catch(() => ({ ok: false }))).ok, { timeout: 30_000 }).toBe(true);
    for (const [local, tag] of [[ludockImage, "e2e"], [nextImage, "next"]]) {
      docker("tag", local, `${registry}:${tag}`);
      docker("push", `${registry}:${tag}`);
    }
    // Ludock runs from the registry's tag, as from ghcr.io.
    const override = join(stack(), "..", "..", "compose.registry.yaml");
    writeFileSync(override, `services:\n  ludock:\n    image: ${registry}:e2e\n    pull_policy: missing\n`);
    docker(...ludock(), "-f", override, "up", "--detach", "ludock");
    await expect.poll(healthy, { timeout: 60_000 }).toBe(true);
    const [original] = ludockContainers();
    const before = inspect(original);
    const gameStarted = inspect(gameContainer()).State.StartedAt;
    const nextID = inspect(nextImage).Id;

    await publish("next");
    const notice = await updateLudock(page);
    // The page reloads once the new version answers.
    await expect(notice).toContainText("Ludock was updated to version e2e-next.", { timeout: 180_000 });
    await page.goto("/settings/system");
    await expect(page.locator("dd", { hasText: "e2e-next" })).toBeVisible();
    const [replaced, ...others] = ludockContainers();
    expect(others).toEqual([]);
    const after = inspect(replaced);
    expect(after.Id).not.toBe(original);
    expect(after.Image).toBe(nextID);
    expect(after.Name).toBe(before.Name);
    expect(after.State.Running).toBe(true);
    expect(mounts(after)).toEqual(mounts(before));
    expect(composeLabels(after)).toEqual(composeLabels(before));
    expect(after.HostConfig.RestartPolicy).toEqual(before.HostConfig.RestartPolicy);
    expect(inspect(gameContainer()).State.StartedAt).toBe(gameStarted);
    expect(docker("ps", "--all", "--quiet", "--filter", "name=ludock-self-update-")).toBe("");
    // Compose sees the new container as its own.
    const plan = spawnSync("docker", [...ludock(), "-f", override, "up", "--detach", "--dry-run", "ludock"], { encoding: "utf8" });
    expect(plan.status).toBe(0);
    expect(plan.stdout + plan.stderr).not.toMatch(/Recreat/);
    await page.goto("/activity?show=everything&action=Updated Ludock");
    const entry = page.locator(".audit-entry").first();
    await expect(entry).toContainText("Updated Ludock from version dev to version e2e-next");
    await expect(entry).toContainText("Succeeded");
    await expect(entry).toContainText(admin.username);

    // A version whose health check never passes.
    docker("tag", unhealthyImage, `${registry}:unhealthy`);
    docker("push", `${registry}:unhealthy`);
    await publish("unhealthy");
    const failing = await updateLudock(page);
    await expect(failing).toContainText("Ludock’s update didn’t finish: the new version reported itself unhealthy, as one whose image is older than this Ludock’s data does. Ludock put version e2e-next back, with its data as it was.",
      { timeout: 180_000 });
    const [kept, ...extra] = ludockContainers();
    expect(extra).toEqual([]);
    expect(kept).toBe(replaced);
    expect(inspect(kept).Image).toBe(nextID);
    expect(inspect(kept).State.Running).toBe(true);
    expect(inspect(gameContainer()).State.StartedAt).toBe(gameStarted);
    expect(docker("ps", "--all", "--quiet", "--filter", "name=ludock-self-update-")).toBe("");
    await page.goto("/");
    await expect(page.locator(".needs-attention__list li", { hasText: "Ludock’s update didn’t finish" }))
      .toContainText("The new version reported itself unhealthy, as one whose image is older than this Ludock’s data does. Ludock is still on version e2e-next");
  } catch (error) {
    diagnose(eventsFile);
    throw error;
  } finally {
    events.kill();
    closeSync(eventsOut);
    docker("rm", "--force", "ludocktest-registry");
  }
});

/** Prints the container events recorded in eventsFile, each Ludock container's and marker's state and health, and the logs of Ludock and its helpers. */
function diagnose(eventsFile) {
  const run = (...args) => {
    try {
      return docker(...args);
    } catch (error) {
      return String(error.stderr || error);
    }
  };
  // Ludock writes warnings to stderr.
  const logs = (id) => {
    const result = spawnSync("docker", ["logs", "--tail", "100", id], { encoding: "utf8" });
    return `${result.stdout}${result.stderr}`.trim();
  };
  const out = ["--- container events", readFileSync(eventsFile, "utf8"),
    "--- containers", run("ps", "--all", "--no-trunc", "--format", "{{.ID}} {{.Names}} {{.Image}} {{.Status}}")];
  const ids = run("ps", "--all", "--quiet", "--filter", "label=ludock.internal").split("\n")
    .concat(run("ps", "--all", "--quiet", "--filter", "label=com.docker.compose.service=ludock").split("\n")).filter(Boolean);
  for (const id of new Set(ids)) {
    out.push(`--- ${id}`, run("inspect", "--format", "{{.Name}} {{.Image}} restarts={{.RestartCount}} {{json .State}} {{json .Config.Labels}} {{json .HostConfig.RestartPolicy}}", id),
      logs(id));
  }
  console.log(out.join("\n"));
}

// A file command interrupted by a restart may still be writing through its
// helper, so Ludock removes every file helper before work resumes.
test("a file helper left by an interrupted run is removed when Ludock starts", async ({ page }) => {
  docker(...ludock(), "stop", "ludock");
  const leftover = docker("run", "--detach", "--label", "ludock.enable=false", "--label", "ludock.internal=file-helper",
    "--volume", `${join(stack(), "data")}:/data`, gameImage, "sh", "-c", "while true; do date > /data/leftover; sleep 1; done");
  const unrelated = docker("run", "--detach", "--label", "ludock.internal=other", gameImage, "sleep", "600");
  try {
    docker(...ludock(), "start", "ludock");
    await expect.poll(healthy, { timeout: 60_000 }).toBe(true);
    await expect.poll(() => docker("ps", "--all", "--quiet", "--filter", `id=${leftover}`), { timeout: 30_000 }).toBe("");
    expect(docker("ps", "--quiet", "--filter", `id=${unrelated}`)).not.toBe("");
    expect(docker("inspect", "--format", "{{.State.Running}}", gameContainer())).toBe("true");
    await openServer(page, "Test Game");
  } finally {
    docker("rm", "--force", unrelated);
  }
});

// The documented procedure: stop Ludock, run the recovery command, start it.
// Recovery signs every session out, so this journey runs last.
test("an administrator password can be recovered from the console", async ({ page }) => {
  docker(...ludock(), "stop", "ludock");
  docker(...ludock(), "run", "--rm", "--no-deps", "-e", "LUDOCK_RECOVERY_PASSWORD=recovered long password", "ludock", "recover", "admin");
  docker(...ludock(), "start", "ludock");
  await expect.poll(healthy, { timeout: 60_000 }).toBe(true);

  await page.context().clearCookies();
  await page.goto("/login");
  await page.getByLabel("Username").fill("admin");
  await page.getByLabel("Password", { exact: true }).fill("recovered long password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
});
