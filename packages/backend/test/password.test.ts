import { expect, describe, it } from "bun:test";
import {
  PasswordWorkBusyError,
  hashPassword,
  passwordHashNeedsUpgrade,
  verifyPassword,
  withPasswordWork,
} from "../src/password.js";

const password = "a-long-pássword-🙂";

describe("password encoding", () => {
  it("uses salted Argon2id hashes and verifies Unicode passwords", async () => {
    const first = await hashPassword(password);
    const second = await hashPassword(password);
    expect(first).toMatch(/^\$argon2id\$v=19\$m=65536,t=2,p=1\$/);
    expect(first).not.toBe(second);
    expect(await verifyPassword(password, first)).toBe(true);
    expect(await verifyPassword(`${password}!`, first)).toBe(false);
    expect(passwordHashNeedsUpgrade(first)).toBe(false);
  });

  it("rejects unsupported password algorithms", async () => {
    for (const encoded of ["scrypt$16384$8$1$AQEBAQEBAQEBAQEBAQEBAQ$invalid", "$2b$12$invalid", "", "fixture"])
      expect(await verifyPassword(password, encoded)).toBe(false);
  });

  it("marks lower Argon2 costs for an upgrade", async () => {
    const encoded = await Bun.password.hash(password, { algorithm: "argon2id", memoryCost: 8192, timeCost: 1 });
    expect(await verifyPassword(password, encoded)).toBe(true);
    expect(passwordHashNeedsUpgrade(encoded)).toBe(true);
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
      valid.replace(/\$[^$]+$/, "$"),
      valid.replace(/\$[^$]+$/, "$AAAAA"),
      valid.replace(/\$[^$]+$/, "$" + "A".repeat(42) + "B"),
      "$argon2id$" + "a".repeat(300),
    ]) expect(await verifyPassword(password, encoded)).toBe(false);
  });
});

describe("password work admission", () => {
  it("atomically caps work per credential, per source, and globally", async () => {
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    const hold = (source: string, subject: string) =>
      withPasswordWork(source, subject, async () => gate);

    const first = hold("source-a", "subject-a");
    await expect(hold("source-b", "subject-a"), "one credential cannot consume concurrent KDF slots").rejects.toThrow(PasswordWorkBusyError);
    const second = hold("source-a", "subject-b");
    await expect(hold("source-a", "subject-c"), "one source is limited to two active KDFs").rejects.toThrow(PasswordWorkBusyError);
    const third = hold("source-c", "subject-c");
    const fourth = hold("source-d", "subject-d");
    await expect(hold("source-e", "subject-e"), "the process-wide KDF limit is enforced before work starts").rejects.toThrow(PasswordWorkBusyError);

    release();
    await Promise.all([first, second, third, fourth]);
    expect(await withPasswordWork("source-a", "subject-a", async () => "released")).toBe("released");
  });

  it("releases admission after password work throws", async () => {
    await expect(withPasswordWork("source", "subject", async () => {
        throw new Error("fixture failure");
      })).rejects.toThrow(/fixture failure/);
    expect(await withPasswordWork("source", "subject", async () => true)).toBe(true);
  });
});
