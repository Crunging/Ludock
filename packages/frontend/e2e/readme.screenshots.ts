import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { backupsResponseSchema, serverResponseSchema } from "@ludock/shared";
import { test, expect, RUNNING_ID, RUNNING_NAME } from "./fixtures";

// Capture the built UI with the same contract-checked, network-isolated fixtures
// as browser tests. Run explicitly; normal checks never rewrite docs.
const directory = fileURLToPath(new URL("../../../docs/screenshots/", import.meta.url));
const now = Date.parse("2026-09-15T12:00:00Z");
const latestBackup = { createdAt: now - 2 * 3_600_000, size: 1_288_490_188 };

test("capture README screenshots", async ({ app, page }) => {
  await mkdir(directory, { recursive: true });
  await page.clock.setFixedTime(now);
  app.servers[0].latestBackup = latestBackup;
  app.servers[1].latestBackup = { createdAt: now - 86_400_000, size: 87_031_808 };
  app.servers.push({
    ...app.servers[0],
    id: "33333333-3333-4333-8333-333333333333",
    name: "valheim", displayName: "Valheim co-op",
    image: "ghcr.io/lloesche/valheim-server:latest", gameType: "valheim", gameConsole: null,
    ports: [{ private: 2456, public: 2456, type: "udp" }],
    latestBackup: { createdAt: now - 4 * 3_600_000, size: 451_936_256 },
  });
  await app.open();
  await expect(page.getByRole("article")).toHaveCount(3);
  await expect(page.getByRole("article", { name: RUNNING_NAME }).getByRole("button", { name: "Stop", exact: true })).toBeEnabled();
  await expect(page.getByText("Loading attention items…")).toHaveCount(0);
  await page.screenshot({ path: `${directory}/servers.png`, fullPage: true, animations: "disabled" });

  await page.route(`**/api/v1/servers/${RUNNING_ID}`, (route) => route.fulfill({
    json: serverResponseSchema.parse({
      server: app.servers[0], stats: { cpuPercent: 8.4, memUsageMB: 2048, memLimitMB: 4096 },
    }),
  }));
  await page.route(`**/api/v1/servers/${RUNNING_ID}/backups`, (route) => route.fulfill({
    json: backupsResponseSchema.parse({ backups: [0, 1, 2].map((day) => ({
      id: `55555555-5555-4555-8555-55555555555${day}`, serverId: RUNNING_ID,
      createdAt: latestBackup.createdAt - day * 86_400_000,
      size: latestBackup.size - day * 33_554_432,
      checksum: "demo-checksum", roots: [{ id: "data", path: "/data" }], state: "complete",
    })) }),
  }));
  await page.getByRole("link", { name: RUNNING_NAME, exact: true }).click();
  await page.getByRole("tab", { name: "Backups", exact: true }).click();
  await expect(page.getByRole("button", { name: "Create backup", exact: true })).toBeEnabled();
  await expect(page.getByRole("link", { name: "Download", exact: true })).toHaveCount(3);
  await page.screenshot({ path: `${directory}/backups.png`, fullPage: true, animations: "disabled" });

  app.files.set("data:", [
    ...["logs", "plugins", "world", "world_nether", "world_the_end"].map((name) => ({
      name, type: "directory" as const, size: 0, modifiedAt: now - 600_000,
    })),
    { name: "server.properties", type: "file", size: 1428, modifiedAt: now - 86_400_000 },
    { name: "whitelist.json", type: "file", size: 248, modifiedAt: now - 86_400_000 },
    { name: "eula.txt", type: "file", size: 10, modifiedAt: now - 7 * 86_400_000 },
  ]);
  await app.open(`/files/${RUNNING_ID}`);
  await expect(page.getByText("server.properties", { exact: true })).toBeVisible();
  await expect(page.getByText("whitelist.json", { exact: true })).toBeVisible();
  await page.screenshot({ path: `${directory}/files.png`, fullPage: true, animations: "disabled" });
});
