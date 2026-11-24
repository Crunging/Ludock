import AxeBuilder from "@axe-core/playwright";
import { test, expect, asAdmin, openServer } from "./fixtures.mjs";

asAdmin();
// Without motion, colors are final as soon as the scheme changes.
test.use({ viewport: { width: 360, height: 740 }, isMobile: true, hasTouch: true, reducedMotion: "reduce" });

// An administrator opens every main page on a narrow phone: each fits the
// screen without scrolling sideways, and has no accessibility problem axe can
// find, in the light and dark schemes.
test("the main pages fit a phone and pass an accessibility check", async ({ page }) => {
  await openServer(page, "Survival");
  const id = page.url().match(/\/servers\/([^/?]+)/)[1];
  const pages = ["/", "/activity", "/users", "/settings", "/settings/system", "/account", `/files/${id}`, `/console/${id}`, "/no-such-page",
    ...["overview", "activity", "backups", "schedules", "sharing", "update"].map((tab) => `/servers/${id}?tab=${tab}`)];
  for (const path of pages) {
    await page.emulateMedia({ colorScheme: "light", reducedMotion: "reduce" });
    await page.goto(path);
    await expect(page.locator("h1").first()).toBeVisible();
    // Disclosures open too, such as Advanced settings and each user's password field.
    await page.locator("details:not([open]) > summary").evaluateAll((summaries) => summaries.forEach((summary) => summary.click()));
    const width = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
    expect(width[0], `${path} scrolls sideways`).toBeLessThanOrEqual(width[1]);
    for (const colorScheme of ["light", "dark"]) {
      await page.emulateMedia({ colorScheme, reducedMotion: "reduce" });
      const { violations } = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa"]).analyze();
      expect(violations.map((violation) => `${violation.id}: ${violation.nodes.map((node) => node.target).join(", ")}`), `${path} in ${colorScheme}`).toEqual([]);
    }
  }
});
