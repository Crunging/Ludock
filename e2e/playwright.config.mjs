// Journeys run the real Ludock binary against cmd/fakedocker. Each worker has
// its own server and fake daemon, and runs whole files in order, so a file's
// journeys share state but never see another worker's.
import { defineConfig } from "@playwright/test";

export const port = Number(process.env.LUDOCK_E2E_PORT || 39871);

export default defineConfig({
  testDir: "journeys",
  fullyParallel: false,
  workers: 4,
  // A retry shows whether a failure is flaky, but a flaky journey still fails.
  retries: process.env.CI ? 1 : 0,
  failOnFlakyTests: !!process.env.CI,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  globalSetup: "./setup.mjs",
  outputDir: "test-results",
  use: {
    viewport: { width: 1280, height: 860 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: process.env.LUDOCK_E2E_CHROMIUM ? { executablePath: process.env.LUDOCK_E2E_CHROMIUM } : {},
  },
});
