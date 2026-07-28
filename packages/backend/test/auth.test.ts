import assert from "node:assert/strict";
import { describe, it } from "node:test";

process.env.PANEL_DB_PATH = ":memory:";
process.env.PANEL_SETUP_TOKEN = "test-setup-token";

const {
  authenticateUser,
  createInitialAdmin,
  hashPassword,
  isSetupRequired,
  isSetupTokenRequired,
  verifyPassword,
} = await import("../src/auth.js");

describe("account authentication", () => {
  it("hashes and verifies passwords without storing plaintext", async () => {
    const encoded = await hashPassword("a-long-test-password");
    assert.match(encoded, /^scrypt\$/);
    assert.equal(encoded.includes("a-long-test-password"), false);
    assert.equal(await verifyPassword("a-long-test-password", encoded), true);
    assert.equal(await verifyPassword("wrong-password", encoded), false);
  });

  it("requires the configured token for initial setup", async () => {
    assert.equal(isSetupTokenRequired(), true);
    await assert.rejects(
      createInitialAdmin({
        setupToken: "wrong",
        username: "admin",
        password: "a-long-test-password",
      }),
      (error: Error & { code?: string }) => {
        assert.equal(error.code, "INVALID_SETUP_TOKEN");
        return true;
      }
    );
    assert.equal(isSetupRequired(), true);
  });

  it("creates the initial administrator without a token by default", async () => {
    delete process.env.PANEL_SETUP_TOKEN;
    assert.equal(isSetupTokenRequired(), false);

    const user = await createInitialAdmin({
      username: "admin",
      password: "a-long-test-password",
    });
    assert.equal(user.role, "admin");
    assert.equal(isSetupRequired(), false);

    await assert.rejects(
      createInitialAdmin({
        username: "other",
        password: "another-long-password",
      }),
      (error: Error & { code?: string }) => {
        assert.equal(error.code, "SETUP_COMPLETE");
        return true;
      }
    );
  });

  it("authenticates valid credentials", async () => {
    assert.equal(
      (await authenticateUser("ADMIN", "a-long-test-password"))?.username,
      "admin"
    );
    assert.equal(
      await authenticateUser("admin", "definitely-wrong-password"),
      null
    );
    assert.equal(
      await authenticateUser("missing", "definitely-wrong-password"),
      null
    );
  });
});
