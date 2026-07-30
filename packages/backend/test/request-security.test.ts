import assert from "node:assert/strict";
import type { IncomingMessage } from "node:http";
import { describe, it } from "node:test";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
} from "../src/request-security.js";

function request(
  headers: IncomingMessage["headers"],
  encrypted = false
): Pick<IncomingMessage, "headers" | "socket"> {
  return {
    headers,
    socket: { encrypted } as IncomingMessage["socket"] & {
      encrypted?: boolean;
    },
  };
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
