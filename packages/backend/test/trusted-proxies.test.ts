import assert from "node:assert/strict";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { after, describe, it } from "node:test";

process.env.PANEL_DB_PATH = ":memory:";

const { createApp } = await import("../src/app.js");

const servers: ReturnType<typeof createServer>[] = [];

after(async () => {
  await Promise.all(
    servers.map(
      (server) =>
        new Promise<void>((resolve) => server.close(() => resolve()))
    )
  );
});

/**
 * TRUSTED_PROXIES is read both when the app is created (Express `trust proxy`)
 * and per request (forwarded-protocol trust), so it must stay set for the whole
 * request rather than just during createApp.
 */
async function withTrustedProxies<T>(
  trustedProxies: string,
  run: (baseUrl: string) => Promise<T>
): Promise<T> {
  const previous = process.env.TRUSTED_PROXIES;
  process.env.TRUSTED_PROXIES = trustedProxies;
  try {
    const server = createServer(createApp({ frontendDist: false }));
    servers.push(server);
    await new Promise<void>((resolve) =>
      server.listen(0, "127.0.0.1", resolve)
    );
    const { port } = server.address() as AddressInfo;
    return await run(`http://127.0.0.1:${port}`);
  } finally {
    if (previous === undefined) delete process.env.TRUSTED_PROXIES;
    else process.env.TRUSTED_PROXIES = previous;
  }
}

describe("TRUSTED_PROXIES configuration", () => {
  it("starts and serves requests when the value is malformed", async () => {
    // Previously this threw out of createApp and crashed the process.
    await withTrustedProxies("not-an-ip,,,", async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/auth/status`);
      assert.equal(response.status, 200);
    });
  });

  it("fails closed on a malformed list instead of trusting the header", async () => {
    await withTrustedProxies("not-an-ip", async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/auth/status`, {
        headers: { "X-Forwarded-Proto": "https" },
      });
      assert.equal(response.status, 200);
      assert.equal(
        response.headers.get("strict-transport-security"),
        null,
        "a spoofed protocol must not be believed"
      );
    });
  });

  it("honours the header for a declared proxy", async () => {
    await withTrustedProxies("127.0.0.1/32, 10.0.0.0/8", async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/auth/status`, {
        headers: { "X-Forwarded-Proto": "https" },
      });
      assert.equal(response.status, 200);
      assert.match(
        response.headers.get("strict-transport-security") || "",
        /max-age=/
      );
    });
  });

  it("ignores the header for an undeclared peer", async () => {
    await withTrustedProxies("10.99.99.99/32", async (baseUrl) => {
      const response = await fetch(`${baseUrl}/api/auth/status`, {
        headers: { "X-Forwarded-Proto": "https" },
      });
      assert.equal(response.status, 200);
      assert.equal(response.headers.get("strict-transport-security"), null);
    });
  });
});
