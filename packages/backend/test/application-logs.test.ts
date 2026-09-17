import { expect, describe, it } from "bun:test";
import {
  listApplicationLogs,
  recordApplicationLog,
  redactApplicationLog,
} from "../src/application-logs.js";

describe("application log buffer", () => {
  it("redacts credentials in messages and context values", () => {
    const input = [
      "Authorization: Bearer header-secret",
      "ludock_session=cookie-secret; Path=/",
      'payload={"password":"json-secret"}',
      "https://example.test/path?api_key=query-secret&safe=value",
    ].join("\n");
    const redacted = redactApplicationLog(input);

    for (const secret of [
      "header-secret",
      "cookie-secret",
      "json-secret",
      "query-secret",
    ]) {
      expect(redacted).not.toMatch(new RegExp(secret));
    }
    expect(redacted).toMatch(/safe=value/);
  });

  it("redacts development session cookies before buffering logs", () => {
    recordApplicationLog({
      timestamp: 122,
      level: "warn",
      component: "test",
      message:
        "ludock_session_012345abcdef=development-secret; Path=/; HttpOnly",
      context: {
        detail: "theme=dark; ludock_session_fedcba543210=context-secret",
        apiKey: "unlabelled-key-secret",
      },
    });
    const entry = listApplicationLogs({ limit: 1 }).entries[0];
    expect(entry).toBeTruthy();
    expect(JSON.stringify(entry)).not.toMatch(/development-secret|context-secret|unlabelled-key-secret/);
    expect(entry.message).toBe("ludock_session_012345abcdef=[REDACTED]; Path=/; HttpOnly");
    expect(entry.context?.detail).toBe("theme=dark; ludock_session_fedcba543210=[REDACTED]");
    expect(entry.context?.apiKey).toBe("[REDACTED]");
  });

  it("returns structured entries and resets stale process cursors", () => {
    recordApplicationLog({
      timestamp: 123,
      level: "warn",
      component: "test",
      message: "diagnostic token=message-secret",
      context: { detail: "password=context-secret" },
    });
    const first = listApplicationLogs({ limit: 10 });
    const entry = first.entries.at(-1);
    expect(entry).toBeTruthy();
    expect(entry.component).toBe("test");
    expect(entry.timestamp).toBe(123);
    expect(JSON.stringify(entry)).not.toMatch(/message-secret|context-secret/);

    const fromStaleGeneration = listApplicationLogs({
      after: Number.MAX_SAFE_INTEGER,
      limit: 10,
      generation: "previous-process",
    });
    expect(fromStaleGeneration.generation).toBe(first.generation);
    expect(fromStaleGeneration.entries.length > 0).toBeTruthy();
  });
});
