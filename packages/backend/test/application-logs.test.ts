import assert from "node:assert/strict";
import { describe, it } from "bun:test";
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
      assert.doesNotMatch(redacted, new RegExp(secret));
    }
    assert.match(redacted, /safe=value/);
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
      },
    });
    const entry = listApplicationLogs({ limit: 1 }).entries[0];
    assert.ok(entry);
    assert.doesNotMatch(
      JSON.stringify(entry),
      /development-secret|context-secret/,
    );
    assert.equal(
      entry.message,
      "ludock_session_012345abcdef=[REDACTED]; Path=/; HttpOnly",
    );
    assert.equal(
      entry.context?.detail,
      "theme=dark; ludock_session_fedcba543210=[REDACTED]",
    );
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
    assert.ok(entry);
    assert.equal(entry.component, "test");
    assert.equal(entry.timestamp, 123);
    assert.doesNotMatch(JSON.stringify(entry), /message-secret|context-secret/);

    const fromStaleGeneration = listApplicationLogs({
      after: Number.MAX_SAFE_INTEGER,
      limit: 10,
      generation: "previous-process",
    });
    assert.equal(fromStaleGeneration.generation, first.generation);
    assert.ok(fromStaleGeneration.entries.length > 0);
  });
});
