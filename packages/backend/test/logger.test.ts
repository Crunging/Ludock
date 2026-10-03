import { expect, describe, it } from "bun:test";
import { createLogger } from "../src/logger.js";
import { listApplicationLogs } from "../src/application-logs.js";

describe("logger", () => {
  it("redacts message and structured secrets from console and stored logs", () => {
    const previousLevel = process.env.LOG_LEVEL;
    const previousWarn = console.warn;
    const warnings: string[] = [];
    process.env.LOG_LEVEL = "warn";
    console.warn = (message?: unknown) => warnings.push(String(message));

    try {
      const logger = createLogger("test");
      logger.warn("visible authorization=Bearer message-secret", {
        requestId: "request-1",
        apiToken: "must-not-appear",
        detail: "password=context-secret",
      });

      expect(warnings.length).toBe(1);
      const entry = listApplicationLogs({ limit: 1 }).entries[0];
      expect(entry).toBeTruthy();
      expect(warnings[0] || "").toMatch(/"apiToken":"\[REDACTED\]"/);
      expect(warnings[0] || "").toMatch(/"detail":"password=\[REDACTED\]"/);
      for (const output of [warnings[0] || "", JSON.stringify(entry)]) {
        expect(output).not.toMatch(/message-secret|must-not-appear|context-secret/);
      }
    } finally {
      if (previousLevel === undefined) delete process.env.LOG_LEVEL;
      else process.env.LOG_LEVEL = previousLevel;
      console.warn = previousWarn;
    }
  });
});
