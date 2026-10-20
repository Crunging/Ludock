import { test, expect, signIn, createUser } from "./fixtures.mjs";

test("changing a password signs out the account's other devices", async ({ session }) => {
  await createUser(await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE }), "player1", "first long password", "viewer");

  const laptop = await session();
  const phone = await session();
  await signIn(laptop, "player1", "first long password");
  await signIn(phone, "player1", "first long password");

  await laptop.getByRole("link", { name: "Account" }).first().click();
  await expect(laptop.locator(".session-row")).toHaveCount(2);
  await laptop.getByLabel("Current password").fill("first long password");
  await laptop.getByLabel("New password", { exact: true }).fill("second long password");
  await laptop.getByLabel("Confirm new password").fill("second long passwort");
  await laptop.getByRole("button", { name: "Change password" }).click();
  await expect(laptop.locator("#account-status")).toHaveText("New passwords do not match.");

  await laptop.getByLabel("Current password").fill("first long password");
  await laptop.getByLabel("New password", { exact: true }).fill("second long password");
  await laptop.getByLabel("Confirm new password").fill("second long password");
  await laptop.getByRole("button", { name: "Change password" }).click();
  await expect(laptop.locator("#account-status")).toHaveText("Password changed. Other sessions have been signed out.");
  await expect(laptop.locator(".session-row")).toHaveCount(1);

  await phone.goto("/");
  await expect(phone).toHaveURL(/\/login/);
  await signIn(phone, "player1", "second long password");
  await expect(phone.getByRole("heading", { name: "Servers", exact: true })).toBeVisible();

  // Signing this device out from the session list.
  await laptop.reload();
  await laptop.locator(".session-row", { hasText: "This device" }).getByRole("button", { name: "Sign out" }).click();
  await expect(laptop).toHaveURL(/\/login/);
});
