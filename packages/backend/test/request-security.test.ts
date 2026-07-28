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
