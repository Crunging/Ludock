import { expect, describe, it } from "bun:test";
import {
  createLogger,
  getLogLevelConfiguration,
} from "../src/logger.js";
import { listApplicationLogs } from "../src/application-logs.js";

describe("logger", () => {
  it("uses info by default and falls back to info for invalid values", () => {
    expect(getLogLevelConfiguration(undefined)).toStrictEqual({ level: "info" });
    expect(getLogLevelConfiguration(" DEBUG ")).toStrictEqual({ level: "debug" });
    expect(getLogLevelConfiguration("verbose")).toStrictEqual({
      level: "info",
      invalidValue: "verbose",
    });
  });

  it("filters by level, includes structured context, and redacts secrets", () => {
    const previousLevel = process.env.LOG_LEVEL;
    const previousWarn = console.warn;
    const previousDebug = console.debug;
    const warnings: string[] = [];
    const debugMessages: string[] = [];
    process.env.LOG_LEVEL = "warn";
    console.warn = (message?: unknown) => warnings.push(String(message));
    console.debug = (message?: unknown) => debugMessages.push(String(message));

    try {
      const logger = createLogger("test");
      logger.debug("hidden");
      logger.warn("visible authorization=Bearer message-secret", {
        requestId: "request-1",
        apiToken: "must-not-appear",
        detail: "password=context-secret",
      });

      expect(debugMessages.length).toBe(0);
      expect(warnings.length).toBe(1);
      const entry = listApplicationLogs({ limit: 1 }).entries[0];
      expect(entry).toBeTruthy();
      expect(warnings[0]).toBe(`${new Date(entry.timestamp).toISOString()} WARN [test] ${entry.message} ${JSON.stringify(entry.context)}`);
      expect(warnings[0] || "").toMatch(/"requestId":"request-1"/);
      expect(warnings[0] || "").toMatch(/"apiToken":"\[REDACTED\]"/);
      expect(warnings[0] || "").toMatch(/"detail":"password=\[REDACTED\]"/);
      for (const output of [warnings[0] || "", JSON.stringify(entry)]) {
        expect(output).not.toMatch(/message-secret|must-not-appear|context-secret/);
      }
    } finally {
      if (previousLevel === undefined) delete process.env.LOG_LEVEL;
      else process.env.LOG_LEVEL = previousLevel;
      console.warn = previousWarn;
      console.debug = previousDebug;
    }
  });
});
