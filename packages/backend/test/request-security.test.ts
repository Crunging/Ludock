import { expect, describe, it } from "bun:test";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
  requestOriginDiagnostic,
} from "../src/request-security.js";

function request(
  headers: HeadersInit,
  encrypted = false
): Request {
  const values = new Headers(headers);
  return new Request(`${encrypted ? "https" : "http"}://${values.get("host") || "panel.example"}/`, { headers: values });
}

describe("request security metadata", () => {
  it("detects HTTPS directly and through a reverse proxy", () => {
    expect(isExternalHttpsRequest(request({}, true))).toBe(true);
    expect(isExternalHttpsRequest(
        request({ "x-forwarded-proto": "https", host: "panel.example" })
      )).toBe(true);
    expect(isExternalHttpsRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      )).toBe(true);
    expect(isExternalHttpsRequest(
        request({ origin: "http://panel.example", host: "panel.example" })
      )).toBe(false);
  });

  it("accepts same-host browser origins through TLS-terminating proxies", () => {
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      )).toBe(true);
    expect(isSameOriginRequest(
        request({
          origin: "https://PANEL.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      )).toBe(true);
    expect(isSameOriginRequest(
        request({ origin: "https://panel.example", host: "panel.example" })
      )).toBe(false);
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      ), "a proxy may use the internal service name in Host").toBe(true);
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-host": "ludock:3000",
          "x-forwarded-proto": "https",
        })
      ), "the public Host remains a valid candidate").toBe(true);
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example:443",
          "x-forwarded-proto": "https",
        })
      ), "default ports are normalized").toBe(true);
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example:8443",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-port": "8443",
          "x-forwarded-proto": "https",
        })
      ), "a forwarded non-default port is included").toBe(true);
  });

  it("uses forwarded public origin metadata", () => {
    const proxied = request(
      {
        origin: "https://panel.example",
        "x-forwarded-proto": "https",
        "x-forwarded-host": "panel.example",
        host: "ludock:3000",
      },
      false
    );
    expect(isExternalHttpsRequest(proxied)).toBe(true);
    expect(isSameOriginRequest(proxied)).toBe(true);
  });

  it("accepts public metadata from a multi-proxy chain", () => {
    const proxied = request({
      origin: "https://panel.example",
      host: "ludock:3000",
      "x-forwarded-host": "panel.example, gateway.internal",
      "x-forwarded-proto": "http, https",
    });
    expect(isSameOriginRequest(proxied)).toBe(true);
    expect(requestOriginDiagnostic(proxied)).toStrictEqual({
      originHost: "panel.example",
      originProtocol: "https:",
      host: "ludock:3000",
      forwardedHost: "panel.example, gateway.internal",
      forwardedProtocol: "http, https",
      fetchSite: "missing",
      resolvedHosts: "ludock:3000,panel.example,gateway.internal",
      resolvedProtocols: "http:,https:",
    });
  });

  it("uses browser fetch metadata when proxy metadata is rewritten", () => {
    expect(isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "gateway.internal",
          "x-forwarded-proto": "http",
          "sec-fetch-site": "same-origin",
        })
      )).toBe(true);
    expect(isSameOriginRequest(
        request({
          origin: "https://attacker.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
          "sec-fetch-site": "cross-site",
        })
      )).toBe(false);
  });

  it("rejects cross-host and malformed origins", () => {
    for (const origin of [
      "https://attacker.example",
      "ftp://panel.example",
      "https://user@panel.example",
      "https://panel.example/path",
      "null",
    ]) {
      expect(isSameOriginRequest(request({ origin, host: "panel.example" }))).toBe(false);
    }
    expect(isSameOriginRequest(
        request({
          origin: "https://attacker.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      )).toBe(false);
  });
});
