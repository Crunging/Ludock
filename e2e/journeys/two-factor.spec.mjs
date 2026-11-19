import { createHmac } from "node:crypto";
import { test, expect, signIn, createUser } from "./fixtures.mjs";

/** The authenticator code for a base32 secret, steps of 30 seconds from now. */
function totp(secret, stepsAhead = 0) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
  let bits = "";
  for (const char of secret.replace(/\s/g, "")) bits += alphabet.indexOf(char).toString(2).padStart(5, "0");
  const key = Buffer.from(bits.match(/.{8}/g).map((byte) => parseInt(byte, 2)));
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30_000) + stepsAhead));
  const sum = createHmac("sha1", key).update(counter).digest();
  const offset = sum[sum.length - 1] & 0x0f;
  return String((sum.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, "0");
}

/** Turns on two-factor sign-in from the account page; returns the secret and recovery codes. */
async function enroll(page, password) {
  await page.goto("/account");
  await page.getByRole("button", { name: "Set up two-factor sign-in" }).click();
  await expect(page.getByRole("img", { name: "QR code for your authenticator app" })).toBeVisible();
  const secret = (await page.locator(".two-factor__key").textContent()).replace(/\s/g, "");
  const section = page.locator("#two-factor");
  await section.getByLabel("Current password").fill(password);
  await section.getByLabel("Code from the app").fill(totp(secret));
  await section.getByRole("button", { name: "Turn on two-factor sign-in" }).click();
  await expect(page.locator("#account-status")).toHaveText("Two-factor sign-in is on. Other sessions have been signed out.");
  const codes = await page.getByRole("list", { name: "Recovery codes" }).getByRole("listitem").allTextContents();
  expect(codes).toHaveLength(10);
  return { secret, codes };
}

async function enterCode(page, code) {
  await expect(page.getByRole("heading", { name: "Enter your code" })).toBeVisible();
  await page.getByLabel("Authentication code").fill(code);
  await page.getByRole("button", { name: "Continue" }).click();
}

test("two-factor sign-in asks for a code from an authenticator app or a recovery code", async ({ session }) => {
  await createUser(await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE }), "carol", "carol long password");
  const laptop = await session();
  const phone = await session();
  await signIn(laptop, "carol", "carol long password");
  await signIn(phone, "carol", "carol long password");

  // A wrong code doesn't turn it on, and the same setup key stays on screen.
  await laptop.goto("/account");
  await laptop.getByRole("button", { name: "Set up two-factor sign-in" }).click();
  const secret = (await laptop.locator(".two-factor__key").textContent()).replace(/\s/g, "");
  const section = laptop.locator("#two-factor");
  await section.getByLabel("Current password").fill("carol long password");
  await section.getByLabel("Code from the app").fill(totp(secret, 5));
  await section.getByRole("button", { name: "Turn on two-factor sign-in" }).click();
  await expect(laptop.locator("#account-status")).toContainText("That code didn't work.");
  expect((await laptop.locator(".two-factor__key").textContent()).replace(/\s/g, "")).toBe(secret);
  await section.getByLabel("Current password").fill("carol long password");
  await section.getByLabel("Code from the app").fill(totp(secret));
  await section.getByRole("button", { name: "Turn on two-factor sign-in" }).click();
  await expect(laptop.locator("#account-status")).toHaveText("Two-factor sign-in is on. Other sessions have been signed out.");
  const codes = await laptop.getByRole("list", { name: "Recovery codes" }).getByRole("listitem").allTextContents();
  expect(codes).toHaveLength(10);
  await expect(laptop.locator(".session-row")).toHaveCount(1);
  // Neither the setup key nor the recovery codes are shown again.
  await laptop.goto("/account");
  await expect(laptop.getByRole("list", { name: "Recovery codes" })).toHaveCount(0);
  await expect(laptop.locator(".two-factor__key")).toHaveCount(0);
  await expect(laptop.locator("#two-factor")).toContainText("10 recovery codes left.");

  // The phone was signed out; the password alone no longer signs in.
  await phone.goto("/account");
  await expect(phone).toHaveURL(/\/login/);
  await signIn(phone, "carol", "carol long password");
  await enterCode(phone, totp(secret, 5));
  await expect(phone.getByRole("alert")).toHaveText("That code didn't work. Check your authenticator app and try again.");
  await phone.goto("/account");
  await expect(phone).toHaveURL(/\/login/);
  await signIn(phone, "carol", "carol long password");
  // The code that turned it on was used, so sign in with the next one.
  await enterCode(phone, totp(secret, 1));
  await expect(phone.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();

  // A recovery code works once in place of a code.
  const tablet = await session();
  await signIn(tablet, "carol", "carol long password");
  await enterCode(tablet, codes[0].toUpperCase());
  await expect(tablet.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
  const other = await session();
  await signIn(other, "carol", "carol long password");
  await enterCode(other, codes[0]);
  await expect(other.getByRole("alert")).toContainText("That code didn't work.");
  await tablet.goto("/account");
  await expect(tablet.locator("#two-factor")).toContainText("9 recovery codes left.");

  // Turning it off asks for the password and signs out other devices.
  const disable = tablet.locator("#two-factor");
  await disable.getByLabel("Current password").fill("carol wrong password");
  await disable.getByRole("button", { name: "Turn off two-factor sign-in" }).click();
  await expect(tablet.locator("#account-status")).toHaveText("Current password is incorrect");
  await tablet.locator("#two-factor").getByLabel("Current password").fill("carol long password");
  await tablet.locator("#two-factor").getByRole("button", { name: "Turn off two-factor sign-in" }).click();
  await expect(tablet.locator("#account-status")).toHaveText("Two-factor sign-in is off. Other sessions have been signed out.");
  await expect(tablet.getByRole("button", { name: "Set up two-factor sign-in" })).toBeVisible();
  await phone.goto("/account");
  await expect(phone).toHaveURL(/\/login/);
  await signIn(phone, "carol", "carol long password");
  await expect(phone.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();
});

test("an administrator turns off two-factor sign-in for a user who lost their phone", async ({ page, session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(admin, "dave", "dave long password");
  await signIn(page, "dave", "dave long password");
  await enroll(page, "dave long password");

  await admin.goto("/users");
  const row = admin.locator(".user-row", { hasText: "dave" });
  await expect(row).toContainText("Two-factor on");
  await row.getByRole("button", { name: "Turn off two-factor" }).click();
  await admin.getByRole("dialog", { name: "Turn off two-factor sign-in for dave?" }).getByRole("button", { name: "Turn off two-factor" }).click();
  await expect(admin.locator("#users .alert--success")).toHaveText(
    "Two-factor sign-in turned off for dave. They were signed out and can set it up again in Account.");
  await expect(admin.locator(".user-row", { hasText: "dave" })).not.toContainText("Two-factor on");

  await page.goto("/account");
  await expect(page).toHaveURL(/\/login/);
  await signIn(page, "dave", "dave long password");
  await expect(page.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();

  await admin.goto("/audit");
  await expect(admin.getByText("user.two-factor.reset").first()).toBeVisible();
});
