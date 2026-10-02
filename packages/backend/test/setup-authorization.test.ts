import { expect, describe, it, spyOn } from "bun:test";

process.env.LUDOCK_DB_PATH = ":memory:";
delete process.env.LUDOCK_SETUP_CODE;

const [{ logSetupInstructions, SetupWindow }, applicationLogs] =
  await Promise.all([
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
const setupCode = /Ludock initial setup code: (\S+)/.exec(consoleOutput)?.[1] ?? "";
expect(setupCode).toBeTruthy();

describe("local setup authorization", () => {
  it("prints a generated code once without adding it to application logs", () => {
    expect(setupCode).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(consoleOutput.split("Ludock initial setup code:").length - 1).toBe(1);
    logSetupInstructions(setupWindow);
    const stored = applicationLogs.listApplicationLogs({ limit: 100 });
    expect(JSON.stringify(stored).includes(setupCode)).toBe(false);
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
    expect(directOutput.includes(configuredCode)).toBe(false);
    expect(JSON.stringify(applicationLogs.listApplicationLogs({ limit: 100 }))
        .includes(configuredCode)).toBe(false);
    expect(configuredWindow.takeGeneratedCode()).toBe(null);
    expect(() => new SetupWindow(Date.now, 60_000, "too-short")).toThrow(/between 32 and 128 characters/);
  });
});
