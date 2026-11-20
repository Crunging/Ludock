// Journeys run the real Ludock binary against cmd/fakedocker. Each worker has
// its own server and fake daemon, and runs whole files in order, so a file's
// journeys share state but never see another worker's.
import { freemem } from "node:os";
import { defineConfig } from "@playwright/test";

// Stacks listen on ports the system hands out, or from LUDOCK_E2E_PORT up.
export const port = Number(process.env.LUDOCK_E2E_PORT || 0);

// Locally both projects share one run, each held to its share of workers.
// A worker takes about 600 MB with its browser and servers, and a machine
// that runs out of memory stalls every journey at once, so a run takes
// workers for at most half the memory free when it starts, from two to nine,
// leaving the rest to whatever else runs. CI runs each project in jobs of
// its own and sets --workers there.
const workers = Math.max(2, Math.min(9, Math.floor(freemem() / 2 / (600 * 1024 ** 2))));
const share = (count) => (process.env.CI ? {} : { workers: count });

export default defineConfig({
  testDir: "journeys",
  fullyParallel: false,
  workers,
  projects: [
    // Journeys that wait on Ludock's own clocks: readings, monitoring, the
    // scheduler, a warning's minute. They mostly wait, so more run at once,
    // and they are listed first so none starts last.
    { name: "slow", testMatch: "slow/*.spec.mjs", ...share(6) },
    { name: "quick", testIgnore: "slow/**", ...share(3) },
  ],
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
