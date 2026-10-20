// Journeys run the real Ludock binary against cmd/fakedocker. They share one
// server and one fake daemon, so they run in order in a single worker.
import { defineConfig } from "@playwright/test";

export const port = Number(process.env.LUDOCK_E2E_PORT || 39871);

export default defineConfig({
  testDir: "journeys",
  fullyParallel: false,
  workers: 1,
  retries: process.env.CI ? 1 : 0,
  timeout: 60_000,
  expect: { timeout: 10_000 },
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  globalSetup: "./setup.mjs",
  outputDir: "test-results",
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    viewport: { width: 1280, height: 860 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: process.env.LUDOCK_E2E_CHROMIUM ? { executablePath: process.env.LUDOCK_E2E_CHROMIUM } : {},
  },
});
