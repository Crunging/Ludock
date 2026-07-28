import assert from "node:assert/strict";
import type { IncomingMessage } from "node:http";
import { describe, it } from "node:test";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
} from "../src/request-security.js";

function request(
  headers: IncomingMessage["headers"],
  encrypted = false,
  remoteAddress = "10.1.2.3"
): Pick<IncomingMessage, "headers" | "socket"> {
  return {
    headers,
    socket: { encrypted, remoteAddress } as IncomingMessage["socket"] & {
      encrypted?: boolean;
    },
  };
}

describe("request security metadata", () => {
  it("detects direct and reverse-proxied HTTPS without trusting client IPs", () => {
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
  });

  it("only believes x-forwarded-proto from a declared proxy", () => {
    const forwarded = { "x-forwarded-proto": "https", host: "panel.example" };
    const previous = process.env.TRUSTED_PROXIES;
    process.env.TRUSTED_PROXIES = "10.1.2.3/32";
    try {
      assert.equal(
        isExternalHttpsRequest(request(forwarded, false, "10.1.2.3")),
        true
      );
      assert.equal(
        isExternalHttpsRequest(request(forwarded, false, "10.9.9.9")),
        false,
        "a direct client must not be able to spoof HTTPS"
      );
      assert.equal(
        isSameOriginRequest(
          request(
            { ...forwarded, origin: "https://panel.example" },
            false,
            "10.9.9.9"
          )
        ),
        false
      );
    } finally {
      if (previous === undefined) delete process.env.TRUSTED_PROXIES;
      else process.env.TRUSTED_PROXIES = previous;
    }
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
  });
});
