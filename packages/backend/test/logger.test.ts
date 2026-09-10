import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  createLogger,
  getLogLevelConfiguration,
} from "../src/logger.js";

describe("logger", () => {
  it("uses info by default and falls back to info for invalid values", () => {
    assert.deepEqual(getLogLevelConfiguration(undefined), { level: "info" });
    assert.deepEqual(getLogLevelConfiguration(" DEBUG "), { level: "debug" });
    assert.deepEqual(getLogLevelConfiguration("verbose"), {
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
      logger.warn("visible", {
        requestId: "request-1",
        apiToken: "must-not-appear",
      });

      assert.equal(debugMessages.length, 0);
      assert.equal(warnings.length, 1);
      assert.match(
        warnings[0] || "",
        /^\d{4}-\d{2}-\d{2}T.* WARN \[test\] visible /
      );
      assert.match(warnings[0] || "", /"requestId":"request-1"/);
      assert.match(warnings[0] || "", /"apiToken":"\[REDACTED\]"/);
      assert.doesNotMatch(warnings[0] || "", /must-not-appear/);
    } finally {
      if (previousLevel === undefined) delete process.env.LOG_LEVEL;
      else process.env.LOG_LEVEL = previousLevel;
      console.warn = previousWarn;
      console.debug = previousDebug;
    }
  });
});
