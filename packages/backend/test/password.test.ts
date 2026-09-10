import assert from "node:assert/strict";
import { scryptSync } from "node:crypto";
import { describe, it } from "bun:test";
import { hashPassword, passwordHashNeedsUpgrade, verifyPassword } from "../src/password.js";

const password = "a-long-pássword-🙂";

describe("password encoding migration", () => {
  it("uses salted Argon2id hashes and verifies Unicode passwords", async () => {
    const first = await hashPassword(password);
    const second = await hashPassword(password);
    assert.match(first, /^\$argon2id\$v=19\$m=65536,t=2,p=1\$/);
    assert.notEqual(first, second);
    assert.equal(await verifyPassword(password, first), true);
    assert.equal(await verifyPassword(`${password}!`, first), false);
    assert.equal(passwordHashNeedsUpgrade(first), false);
  });

  it("verifies existing scrypt accounts and marks them for a login upgrade", async () => {
    const salt = Buffer.alloc(16, 1);
    for (const options of [{ N: 16384, r: 8, p: 1 }, { N: 32768, r: 8, p: 3 }]) {
      const key = scryptSync(password, salt, 64, { ...options, maxmem: 64 * 1024 * 1024 });
      const encoded = `scrypt$${options.N}$${options.r}$${options.p}$${salt.toString("base64url")}$${key.toString("base64url")}`;
      assert.equal(await verifyPassword(password, encoded), true);
      assert.equal(await verifyPassword("incorrect-password", encoded), false);
      assert.equal(passwordHashNeedsUpgrade(encoded), true);
    }
  });

  it("rejects malformed or excessive Argon2 parameters before password work", async () => {
    const valid = await hashPassword(password);
    for (const encoded of [
      valid.replace("m=65536", "m=9999999999"),
      valid.replace("t=2", "t=9999999999"),
      valid.replace("p=1", "p=0"),
      valid.replace("p=1", "p=17"),
      valid.replace("m=65536", "m=1"),
      valid.replace("v=19", "v=16"),
      valid.replace("argon2id", "argon2i"),
      `${valid}$extra`,
      valid.slice(0, -1),
      valid.replace(/\$[^$]+$/, "$!"),
      "$argon2id$" + "a".repeat(300),
    ]) assert.equal(await verifyPassword(password, encoded), false);
  });
});
