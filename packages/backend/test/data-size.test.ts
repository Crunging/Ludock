import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import { formatByteSize, parseByteSize } from "@ludock/shared";
import { getMaxUploadBytes } from "../src/upload-limit.js";

describe("readable byte sizes", () => {
  it("parses decimal and binary units into exact safe integer bytes", () => {
    for (const [input, expected] of [
      ["0", 0],
      [" 42 B ", 42],
      ["500 MB", 500_000_000],
      ["2GiB", 2_147_483_648],
      ["1.5 GiB", 1_610_612_736],
      ["0.57KB", 570],
      [".5 kib", 512],
      ["1.0 mIb", 1_048_576],
      ["1 gb", 1_000_000_000],
      ["1 TB", 1_000_000_000_000],
      ["1 TiB", 1_099_511_627_776],
      ["0.0009765625 KiB", 1],
      ["9007199254740991", Number.MAX_SAFE_INTEGER],
      ["9007199.254740991 GB", Number.MAX_SAFE_INTEGER],
    ] as const) {
      assert.equal(parseByteSize(input), expected, input);
    }
    for (const input of [
      "", " ", "-1 GiB", "+1", "1e3", "0x100", "Infinity", "NaN",
      "1 PB", "1 K", "1 Mi B", "1,000 B", "1.5 B", "0.0001 KB",
      "9007199254740992", "9007199.254740992 GB", "999999999999 TiB",
    ]) {
      assert.equal(parseByteSize(input), null, input);
    }
  });

  it("formats rounded IEC units without unnecessary decimal zeros", () => {
    for (const [input, expected] of [
      [0, "0 B"], [42, "42 B"], [1023, "1023 B"], [1024, "1 KiB"],
      [1536, "1.5 KiB"], [123_456_789, "117.74 MiB"],
      [2_147_483_648, "2 GiB"], [1_099_511_627_776, "1 TiB"],
    ] as const) {
      assert.equal(formatByteSize(input), expected);
    }
  });
});

describe("upload size configuration", () => {
  it("uses readable sizes first, then legacy integer bytes, then the default", () => {
    for (const [env, expected] of [
      [{}, 2_147_483_648],
      [{ MAX_UPLOAD_SIZE: " ", MAX_UPLOAD_BYTES: "\t" }, 2_147_483_648],
      [{ MAX_UPLOAD_SIZE: "500 MB" }, 500_000_000],
      [{ MAX_UPLOAD_SIZE: "1.5 GiB", MAX_UPLOAD_BYTES: "1" }, 1_610_612_736],
      [{ MAX_UPLOAD_SIZE: "2GiB", MAX_UPLOAD_BYTES: "invalid" }, 2_147_483_648],
      [{ MAX_UPLOAD_BYTES: " 570 " }, 570],
      [{ MAX_UPLOAD_SIZE: "", MAX_UPLOAD_BYTES: "1024" }, 1024],
    ] as const) {
      assert.equal(getMaxUploadBytes(env), expected);
    }
  });

  it("rejects invalid configured limits with guidance that does not echo values", () => {
    for (const variable of ["MAX_UPLOAD_SIZE", "MAX_UPLOAD_BYTES"] as const) {
      const invalid = variable === "MAX_UPLOAD_SIZE"
        ? ["0", "-1 GiB", "1.5 B", "9007199254740992", "secret-marker-value"]
        : ["0", "-1", "1.5", "1e3", "1 KiB", "9007199254740992", "secret-marker-value"];
      for (const value of invalid) {
        assert.throws(
          () => getMaxUploadBytes({ [variable]: value }),
          (error) => error instanceof Error &&
            error.message.startsWith(`Invalid ${variable}:`) &&
            error.message.includes("500 MB") &&
            !error.message.includes("secret-marker-value"),
        );
      }
    }
    assert.throws(
      () => getMaxUploadBytes({ MAX_UPLOAD_SIZE: "invalid", MAX_UPLOAD_BYTES: "1024" }),
      /^Error: Invalid MAX_UPLOAD_SIZE:/,
    );
  });
});
