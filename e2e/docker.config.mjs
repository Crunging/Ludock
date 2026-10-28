// Container journeys: the built image, deployed with the repository's
// compose.yaml, against a disposable Docker daemon and real game containers.
// CI runs them on amd64 and arm64, each file in its own job, so a file must
// pass on its own deployment. Never run them against a daemon that
// hosts real game servers.
import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "docker",
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 120_000,
  expect: { timeout: 20_000 },
  reporter: process.env.CI ? [["github"], ["list"]] : "list",
  globalSetup: "./docker-setup.mjs",
  outputDir: "test-results/docker",
  use: {
    baseURL: "http://127.0.0.1:3000",
    viewport: { width: 1280, height: 860 },
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: process.env.LUDOCK_E2E_CHROMIUM ? { executablePath: process.env.LUDOCK_E2E_CHROMIUM } : {},
  },
});
