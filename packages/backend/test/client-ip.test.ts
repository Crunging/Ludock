import { describe, expect, it } from "bun:test";
import { createClientIpResolver } from "../src/client-ip.js";

const request = (forwarded: string) => ({ headers: new Headers({ "x-forwarded-for": forwarded }) });

describe("trusted proxy client addresses", () => {
  it("ignores forwarding from direct clients and stops at the first untrusted hop", () => {
    expect(createClientIpResolver("")(request("198.51.100.1"), "203.0.113.1")).toBe("203.0.113.1");
    const resolve = createClientIpResolver("172.18.0.0/24,2001:db8:1234::/48");
    expect(resolve(request("198.51.100.1"), "203.0.113.1")).toBe("203.0.113.1");
    expect(resolve(request("spoofed, 203.0.113.1, 2001:db8:1234::2"), "172.18.0.2")).toBe("203.0.113.1");
    expect(resolve(request("198.51.100.1"), "172.18.1.2")).toBe("172.18.1.2");
    expect(resolve(request("198.51.100.1"), "2001:db8:1235::2")).toBe("2001:db8:1235::2");
    expect(resolve(request("198.51.100.1"), undefined)).toBeUndefined();
  });

  it("normalizes IPv6 and mapped IPv4 so alternate spellings share a throttle", () => {
    const resolve = createClientIpResolver("172.18.0.2,::ffff:172.18.0.3/128");
    expect(resolve(request("2001:0DB8:0000::1"), "::ffff:ac12:2")).toBe("2001:db8::1");
    expect(resolve(request("::ffff:203.0.113.1"), "172.18.0.3")).toBe("203.0.113.1");
    expect(createClientIpResolver("")(request(""), "::ffff:cb00:7101")).toBe("203.0.113.1");
  });

  it("falls back to the peer when a trusted forwarding hop is malformed", () => {
    const resolve = createClientIpResolver("172.18.0.2");
    for (const header of ["unknown", "127.1", "203.0.113.1,", Array(33).fill("172.18.0.2").join(",")])
      expect(resolve(request(header), "172.18.0.2")).toBe("172.18.0.2");
  });

  it("rejects invalid trust entries instead of broadening proxy access", () => {
    for (const configured of ["*", "172.18.0.2/33", "::1/129"])
      expect(() => createClientIpResolver(configured)).toThrow("LUDOCK_TRUSTED_PROXIES");
  });
});
