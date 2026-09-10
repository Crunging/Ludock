import assert from "node:assert/strict";
import { describe, it } from "bun:test";
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
    assert.equal(isExternalHttpsRequest(request({}, true)), true);
    assert.equal(
      isExternalHttpsRequest(
        request({ "x-forwarded-proto": "https", host: "panel.example" })
      ),
      true
    );
    assert.equal(
      isExternalHttpsRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      true
    );
    assert.equal(
      isExternalHttpsRequest(
        request({ origin: "http://panel.example", host: "panel.example" })
      ),
      false
    );
  });

  it("accepts same-host browser origins through TLS-terminating proxies", () => {
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      true
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://PANEL.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      true
    );
    assert.equal(
      isSameOriginRequest(
        request({ origin: "https://panel.example", host: "panel.example" })
      ),
      false
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      true,
      "a proxy may use the internal service name in Host"
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "panel.example",
          "x-forwarded-host": "ludock:3000",
          "x-forwarded-proto": "https",
        })
      ),
      true,
      "the public Host remains a valid candidate"
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example:443",
          "x-forwarded-proto": "https",
        })
      ),
      true,
      "default ports are normalized"
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example:8443",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-port": "8443",
          "x-forwarded-proto": "https",
        })
      ),
      true,
      "a forwarded non-default port is included"
    );
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
    assert.equal(isExternalHttpsRequest(proxied), true);
    assert.equal(isSameOriginRequest(proxied), true);
  });

  it("accepts public metadata from a multi-proxy chain", () => {
    const proxied = request({
      origin: "https://panel.example",
      host: "ludock:3000",
      "x-forwarded-host": "panel.example, gateway.internal",
      "x-forwarded-proto": "http, https",
    });
    assert.equal(isSameOriginRequest(proxied), true);
    assert.deepEqual(requestOriginDiagnostic(proxied), {
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
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://panel.example",
          host: "ludock:3000",
          "x-forwarded-host": "gateway.internal",
          "x-forwarded-proto": "http",
          "sec-fetch-site": "same-origin",
        })
      ),
      true
    );
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://attacker.example",
          host: "panel.example",
          "x-forwarded-proto": "https",
          "sec-fetch-site": "cross-site",
        })
      ),
      false
    );
  });

  it("rejects cross-host and malformed origins", () => {
    for (const origin of [
      "https://attacker.example",
      "ftp://panel.example",
      "https://user@panel.example",
      "https://panel.example/path",
      "null",
    ]) {
      assert.equal(
        isSameOriginRequest(request({ origin, host: "panel.example" })),
        false
      );
    }
    assert.equal(
      isSameOriginRequest(
        request({
          origin: "https://attacker.example",
          host: "ludock:3000",
          "x-forwarded-host": "panel.example",
          "x-forwarded-proto": "https",
        })
      ),
      false
    );
  });
});
