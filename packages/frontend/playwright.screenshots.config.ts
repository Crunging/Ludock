import { defineConfig } from "@playwright/test";
import config from "./playwright.config";

export default defineConfig({
  ...config,
  testMatch: "readme.screenshots.ts",
  projects: [{ name: "readme", use: { viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1 } }],
  reporter: "list",
  workers: 1,
});
