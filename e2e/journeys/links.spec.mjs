import { test, expect, signIn, createUser } from "./fixtures.mjs";

const userRow = (page, name) => page.locator(".user-row", { has: page.getByText(name, { exact: true }) });

/** Reads the link Ludock just showed on the users page. */
async function shownLink(page, heading) {
  const result = page.getByRole("region", { name: heading });
  await expect(result).toBeVisible();
  return result.getByRole("textbox").inputValue();
}

async function choosePassword(page, password, button) {
  await page.getByLabel("New password").fill(password);
  await page.getByLabel("Confirm password").fill(password);
  await page.getByRole("button", { name: button }).click();
}

test("inviting a user who chooses their own password, once", async ({ page, session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await admin.goto("/users");
  const form = admin.locator("#users form.create-user");
  await expect(form.getByLabel("Make an invite link")).toBeChecked();
  await expect(form.getByLabel("Initial password")).toBeHidden();
  await form.getByLabel("Username").fill("dana");
  await form.getByLabel("Account").selectOption("member");
  await form.getByRole("button", { name: "Create user" }).click();
  const link = await shownLink(admin, "Invite link for dana");
  expect(link).toMatch(new RegExp(`^${new URL(admin.url()).origin}/link/[\\w-]{43}$`));
  // Journeys open Ludock at 127.0.0.1, which friends elsewhere can't reach.
  await expect(admin.getByRole("region", { name: "Invite link for dana" }).getByRole("note")).toHaveText(
    "This link opens only on this computer, because you’re using Ludock at 127.0.0.1. To send it to dana, open Ludock at an address they can reach, then make a new link.");
  await expect(userRow(admin, "dana")).toContainText("Invite link waiting to be used.");

  // The account can't sign in before the invite is used.
  await signIn(page, "dana", "dana chose this password");
  await expect(page.getByRole("alert")).toHaveText("Invalid username or password");

  const dana = await session();
  await dana.goto(link);
  await expect(dana).toHaveURL(/\/link$/);
  await expect(dana.getByRole("heading", { name: "Welcome to Ludock" })).toBeVisible();
  await choosePassword(dana, "dana chose this password", "Save and sign in");
  await expect(dana.getByText("No servers shared")).toBeVisible();

  const again = await session();
  await again.goto(link);
  await expect(again.getByRole("heading", { name: "This link doesn't work" })).toBeVisible();
  await admin.reload();
  await expect(userRow(admin, "dana")).not.toContainText("waiting to be used");
});

test("resetting a password by link, after revoking an earlier one", async ({ session }) => {
  const admin = await session({ storageState: process.env.LUDOCK_E2E_ADMIN_STATE });
  await createUser(admin, "erin", "erin first long password");
  const laptop = await session();
  await signIn(laptop, "erin", "erin first long password");
  await expect(laptop.getByText("No servers shared")).toBeVisible();

  await userRow(admin, "erin").getByRole("button", { name: "Reset link" }).click();
  const revoked = await shownLink(admin, "Password reset link for erin");
  await userRow(admin, "erin").getByRole("button", { name: "Revoke link" }).click();
  await expect(admin.locator("#users .alert--success")).toHaveText("Revoked the link for erin. It no longer works.");
  const phone = await session();
  await phone.goto(revoked);
  await expect(phone.getByRole("heading", { name: "This link doesn't work" })).toBeVisible();

  await userRow(admin, "erin").getByRole("button", { name: "Reset link" }).click();
  await phone.goto(await shownLink(admin, "Password reset link for erin"));
  await expect(phone.getByRole("heading", { name: "Choose a new password" })).toBeVisible();
  await choosePassword(phone, "erin second long password", "Save and sign in");
  await expect(phone.getByText("No servers shared")).toBeVisible();

  // The reset signed out the session that used the old password.
  await laptop.goto("/");
  await expect(laptop).toHaveURL(/\/login/);
});
