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

  it("redacts session cookies before buffering logs", () => {
    recordApplicationLog({
      timestamp: 122,
      level: "warn",
      component: "test",
      message:
        "ludock_session=development-secret; Path=/; HttpOnly",
      context: {
        detail: "theme=dark; ludock_session=context-secret",
        apiKey: "unlabelled-key-secret",
      },
    });
    const entry = listApplicationLogs({ limit: 1 }).entries[0];
    expect(entry).toBeTruthy();
    expect(JSON.stringify(entry)).not.toMatch(/development-secret|context-secret|unlabelled-key-secret/);
    expect(entry.message).toBe("ludock_session=[REDACTED]; Path=/; HttpOnly");
    expect(entry.context?.detail).toBe("theme=dark; ludock_session=[REDACTED]");
    expect(entry.context?.apiKey).toBe("[REDACTED]");
  });

  it("redacts complete quoted credentials, including spaces and escaped quotes", () => {
    for (const input of [
      'password="synthetic secret words" action=connect',
      "token='synthetic secret words' action=connect",
      String.raw`password="synthetic\"credential-tail" action=connect`,
      String.raw`secret='synthetic\'credential-tail' action=connect`,
      'authorization="Bearer synthetic secret words" action=connect',
      'password="synthetic unfinished secret',
      'password="synthetic unfinished secret\\',
    ]) {
      const redacted = redactApplicationLog(input);
      expect(redacted).not.toMatch(/synthetic|secret words|credential-tail|unfinished secret/);
      expect(redacted).toContain("[REDACTED]");
      if (input.endsWith("action=connect")) expect(redacted).toContain("action=connect");
    }
  });

  it("redacts escaped JSON credentials before storing messages and context", () => {
    const input = JSON.stringify({ password: 'synthetic"credential-tail\\', safe: true });
    recordApplicationLog({
      timestamp: 123,
      level: "warn",
      component: "test",
      message: input,
      context: { detail: input },
    });
    const entry = listApplicationLogs({ limit: 1 }).entries[0];
    expect(JSON.stringify(entry)).not.toMatch(/synthetic|credential-tail/);
    expect(JSON.parse(entry.message)).toEqual({ password: "[REDACTED]", safe: true });
    expect(entry.context?.detail).toBe(entry.message);
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
