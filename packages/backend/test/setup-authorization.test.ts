import assert from "node:assert/strict";
import { serve } from "bun";
import { afterAll, describe, it, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
delete process.env.LUDOCK_SETUP_CODE;

const [{ createApp }, { logSetupInstructions, SetupWindow }, applicationLogs] =
  await Promise.all([
    import("../src/app.js"),
    import("../src/auth.js"),
    import("../src/application-logs.js"),
  ]);

const setupWindow = new SetupWindow(Date.now, 60_000);
let consoleOutput = "";
const write = spyOn(process.stdout, "write").mockImplementation(
  ((chunk: string | Uint8Array) => {
    consoleOutput += chunk.toString();
    return true;
  }) as typeof process.stdout.write,
);
try {
  logSetupInstructions(setupWindow);
} finally {
  write.mockRestore();
}
const setupCode = /Ludock initial setup code: (\S+)/.exec(consoleOutput)?.[1];
assert.ok(setupCode);

const server = serve({
  ...createApp({ frontendDist: false, setupWindow }),
  hostname: "127.0.0.1",
  port: 0,
});

afterAll(async () => {
  await server.stop(true);
});

function setup(body: unknown): Promise<Response> {
  return fetch(new URL("/api/v1/auth/setup", server.url), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

describe("local setup authorization", () => {
  it("prints a generated code once without adding it to application logs", () => {
    assert.match(setupCode, /^[A-Za-z0-9_-]{43}$/);
    assert.equal(consoleOutput.split("Ludock initial setup code:").length - 1, 1);
    logSetupInstructions(setupWindow);
    const stored = applicationLogs.listApplicationLogs({ limit: 100 });
    assert.equal(JSON.stringify(stored).includes(setupCode), false);
  });

  it("does not print configured codes and rejects weak configuration", () => {
    const configuredCode = "configured-setup-code-0123456789abcdef";
    const configuredWindow = new SetupWindow(Date.now, 60_000, configuredCode);
    let directOutput = "";
    const configuredWrite = spyOn(process.stdout, "write").mockImplementation(
      ((chunk: string | Uint8Array) => {
        directOutput += chunk.toString();
        return true;
      }) as typeof process.stdout.write,
    );
    try {
      logSetupInstructions(configuredWindow);
    } finally {
      configuredWrite.mockRestore();
    }
    assert.equal(directOutput.includes(configuredCode), false);
    assert.equal(
      JSON.stringify(applicationLogs.listApplicationLogs({ limit: 100 }))
        .includes(configuredCode),
      false,
    );
    assert.equal(configuredWindow.takeGeneratedCode(), null);
    assert.throws(
      () => new SetupWindow(Date.now, 60_000, "too-short"),
      /between 32 and 128 characters/,
    );
  });

  it("rejects missing and incorrect codes before validating account fields", async () => {
    const missing = await setup({ username: "x", password: "short" });
    const incorrect = await setup({
      username: "x",
      password: "short",
      bootstrapCode: "incorrect-setup-code-0123456789abcdef",
    });
    assert.equal(missing.status, 403);
    assert.equal(incorrect.status, 403);
    const missingBody = await missing.json();
    const incorrectBody = await incorrect.json();
    assert.deepEqual(missingBody, incorrectBody);
    assert.deepEqual(missingBody, {
      error: "Initial setup authorization failed",
    });
  });

  it("still validates the strict setup contract after code authorization", async () => {
    const invalid = await setup({
      username: "x",
      password: "short",
      bootstrapCode: setupCode,
      ignored: true,
    });
    assert.equal(invalid.status, 400);

    const created = await setup({
      username: "admin",
      password: "a-valid-setup-password",
      bootstrapCode: setupCode,
    });
    assert.equal(created.status, 201);
    assert.equal(JSON.stringify(applicationLogs.listApplicationLogs({ limit: 100 }))
      .includes(setupCode), false);
  });
});
