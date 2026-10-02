import { expect, describe, it } from "bun:test";
import {
  PasswordWorkBusyError,
  hashPassword,
  verifyPassword,
  withPasswordWork,
} from "../src/password.js";

const password = "a-long-pássword-🙂";

describe("password encoding", () => {
  it("rejects malformed or excessive Argon2 parameters before password work", async () => {
    const valid = await hashPassword(password);
    for (const encoded of [
      valid.replace("m=65536", "m=9999999999"),
      valid.replace("t=2", "t=9999999999"),
      valid.replace(/\$[^$]+$/, "$!"),
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
});
