import { defineConfig } from "@playwright/test";

const port = Number(process.env.LUDOCK_E2E_PORT || 4179);
if (!Number.isInteger(port) || port < 1024 || port > 65535) {
  throw new Error("LUDOCK_E2E_PORT must be an integer between 1024 and 65535.");
}
const origin = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.spec.ts",
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  workers: 2,
  timeout: 30_000,
  expect: { timeout: 5_000 },
  reporter: [["list"], ["html", { open: "never" }]],
  use: {
    baseURL: origin,
    browserName: "chromium",
    serviceWorkers: "block",
    timezoneId: "UTC",
    locale: "en-US",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: {
      ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
        ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
        : {}),
    },
  },
  projects: [
    { name: "desktop", use: { viewport: { width: 1360, height: 900 } } },
    {
      name: "mobile",
      use: {
        viewport: { width: 390, height: 844 },
        isMobile: true,
        hasTouch: true,
        deviceScaleFactor: 1,
      },
    },
  ],
  webServer: {
    command: "pnpm exec vite preview --config e2e/preview.config.ts",
    url: origin,
    env: { LUDOCK_E2E_PORT: String(port) },
    // Never reuse a development backend or a server owned by another checkout.
    reuseExistingServer: false,
    gracefulShutdown: { signal: "SIGTERM", timeout: 5_000 },
  },
});
