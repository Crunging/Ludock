import { expect, describe, it } from "bun:test";
import {
  isExternalHttpsRequest,
  isSameOriginRequest,
} from "../src/request-security.js";

function request(headers: Record<string, string>): Request {
  const values = new Headers(headers);
  return new Request(`http://${values.get("host") || "panel.example"}/`, { headers: values });
}

describe("request security metadata", () => {
  it("uses forwarded public origin metadata", () => {
    const proxied = request(
      {
        origin: "https://panel.example",
        "x-forwarded-proto": "https",
        "x-forwarded-host": "panel.example",
        host: "ludock:3000",
      },
    );
    expect(isExternalHttpsRequest(proxied)).toBe(true);
    expect(isSameOriginRequest(proxied)).toBe(true);
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
