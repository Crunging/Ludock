import { describe, expect, it } from "bun:test";
import { serverOptions } from "../server-options";

describe("frontend server options", () => {
  it("defaults to loopback and accepts the managed runner's port and separator arguments", () => {
    expect(serverOptions([], 3000)).toEqual({ hostname: "127.0.0.1", port: 3000 });
    expect(serverOptions(["--", "--host", "127.0.0.1", "--port", "43000", "--strictPort"], 3000))
      .toEqual({ hostname: "127.0.0.1", port: 43000 });
  });

  it.each(["0.0.0.0", "::", "192.168.1.10", "public.example", ""])(
    "rejects non-loopback binding %s", (hostname) => {
      expect(() => serverOptions(["--host", hostname], 3000)).toThrow("loopback");
    },
  );

  it.each(["0", "80", "65536", "3.5", "NaN", ""])(
    "rejects an invalid listening port %s", (port) => {
      expect(() => serverOptions(["--port", port], 3000)).toThrow("integer between 1024 and 65535");
    },
  );

  it("rejects unknown options instead of silently using unexpected defaults", () => {
    expect(() => serverOptions(["--https"], 3000)).toThrow("Unknown frontend server option");
  });
});
