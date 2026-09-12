import assert from "node:assert/strict";
import { describe, it } from "bun:test";
import {
  nextScheduleRun,
  savedScheduleSchema,
  scheduleEnabledRequestSchema,
  scheduleSlot,
  updateScheduleRequestSchema,
  type ScheduleInput,
} from "@ludock/shared";

const input: ScheduleInput = {
  action: "restart",
  enabled: true,
  time: "09:30",
  days: [1, 3],
  timezone: "UTC",
};
const at = (value: string) => Date.parse(value);

describe("next schedule run", () => {
  it("uses selected weekdays, includes the current minute, and skips missed minutes", () => {
    for (const now of ["2026-09-07T09:29:00Z", "2026-09-07T09:30:00Z", "2026-09-07T09:30:59.999Z"])
      assert.equal(nextScheduleRun(input, at(now)), at("2026-09-07T09:30:00Z"));
    assert.equal(nextScheduleRun(input, at("2026-09-07T09:31:00Z")), at("2026-09-09T09:30:00Z"));
    assert.equal(nextScheduleRun(input, at("2026-09-08T09:00:00Z")), at("2026-09-09T09:30:00Z"));
  });

  it("excludes a consumed slot while including an unconsumed current minute", () => {
    const now = at("2026-09-07T09:30:42Z");
    const slot = scheduleSlot(input, now);
    assert.equal(slot, "2026-09-07T09:30:UTC");
    assert.equal(nextScheduleRun(input, now), at("2026-09-07T09:30:00Z"));
    assert.equal(nextScheduleRun(input, now, slot), at("2026-09-09T09:30:00Z"));
    assert.equal(nextScheduleRun(input, now, "an older slot"), at("2026-09-07T09:30:00Z"));
  });

  it("offers both fall-back occurrences until that local slot has been consumed", () => {
    const fall = { ...input, timezone: "America/Los_Angeles", days: [0], time: "01:30" };
    const first = at("2026-11-01T08:30:00Z");
    const second = at("2026-11-01T09:30:00Z");
    const slot = scheduleSlot(fall, first);
    assert.equal(slot, scheduleSlot(fall, second));
    assert.equal(nextScheduleRun(fall, first - 60_000), first);
    assert.equal(nextScheduleRun(fall, first + 15_000), first);
    assert.equal(nextScheduleRun(fall, first + 60_000), second);
    assert.equal(nextScheduleRun(fall, second + 15_000), second);
    assert.equal(nextScheduleRun(fall, first, slot), at("2026-11-08T09:30:00Z"));
    assert.equal(nextScheduleRun(fall, second, slot), at("2026-11-08T09:30:00Z"));
  });

  it("looks beyond seven days when the next weekly slot falls in a spring gap", () => {
    const spring = { ...input, timezone: "America/Los_Angeles", days: [0], time: "02:30" };
    assert.equal(
      nextScheduleRun(spring, at("2026-03-01T10:31:00Z")),
      at("2026-03-15T09:30:00Z"),
    );
    assert.equal(
      nextScheduleRun(spring, at("2026-03-08T09:59:00Z")),
      at("2026-03-15T09:30:00Z"),
    );
    assert.equal(scheduleSlot(spring, at("2026-03-08T10:30:00Z")), null);
  });

  it("uses local weekdays around midnight in fractional-offset timezones", () => {
    const nepal = { ...input, timezone: "Asia/Kathmandu", days: [1], time: "00:15" };
    const due = at("2026-09-06T18:30:00Z");
    assert.equal(nextScheduleRun(nepal, at("2026-09-06T18:20:00Z")), due);
    assert.equal(scheduleSlot(nepal, due), "2026-09-07T00:15:Asia/Kathmandu");
    assert.equal(nextScheduleRun(nepal, due + 30_000), due);
    assert.equal(nextScheduleRun(nepal, due + 60_000), at("2026-09-13T18:30:00Z"));
  });

  it("handles half-hour daylight-saving transitions", () => {
    const fall = { ...input, timezone: "Australia/Lord_Howe", days: [0], time: "01:45" };
    const first = at("2026-04-04T14:45:00Z");
    const second = at("2026-04-04T15:15:00Z");
    assert.equal(scheduleSlot(fall, first), scheduleSlot(fall, second));
    assert.equal(nextScheduleRun(fall, first + 60_000), second);
    assert.equal(nextScheduleRun(fall, first, scheduleSlot(fall, first)), at("2026-04-11T15:15:00Z"));
    assert.equal(
      nextScheduleRun({ ...fall, time: "02:15" }, at("2026-10-03T14:00:00Z")),
      at("2026-10-10T15:15:00Z"),
    );
  });

  it("skips a local date removed by a timezone transition", () => {
    const samoa = { ...input, timezone: "Pacific/Apia", days: [5], time: "03:00" };
    assert.equal(nextScheduleRun(samoa, at("2011-12-29T12:00:00Z")), at("2012-01-05T13:00:00Z"));
  });

  it("finds a repeated slot on yesterday's date after a cross-midnight rollback", () => {
    const casey = {
      ...input,
      timezone: "Antarctica/Casey",
      days: [4],
      time: "23:30",
    };
    const first = at("2010-03-04T12:30:00Z");
    const second = at("2010-03-04T15:30:00Z");
    const now = at("2010-03-04T13:30:00Z");
    assert.equal(nextScheduleRun(casey, now), second);
    assert.equal(scheduleSlot(casey, first), scheduleSlot(casey, second));
    assert.equal(
      nextScheduleRun(casey, now, scheduleSlot(casey, first)),
      at("2010-03-11T15:30:00Z"),
    );
  });

  it("returns no slot or preview for paused and invalid clock inputs", () => {
    const now = at("2026-09-07T09:30:00Z");
    for (const invalid of [
      { ...input, enabled: false },
      { ...input, timezone: "Not/A_Timezone" },
      { ...input, time: "25:00" },
      { ...input, days: [] },
      { ...input, days: [7] },
    ]) {
      assert.equal(nextScheduleRun(invalid, now), null);
      assert.equal(scheduleSlot(invalid, now), null);
    }
    for (const invalidNow of [NaN, Infinity, -1, 9e15]) {
      assert.equal(nextScheduleRun(input, invalidNow), null);
      assert.equal(scheduleSlot(input, invalidNow), null);
    }
  });
});

describe("schedule editing contracts", () => {
  it("requires a positive revision and full schedule input for edits", () => {
    assert.equal(updateScheduleRequestSchema.safeParse({ ...input, revision: 1 }).success, true);
    assert.equal(updateScheduleRequestSchema.safeParse(input).success, false);
    assert.equal(updateScheduleRequestSchema.safeParse({ revision: 1, time: "12:00" }).success, false);
    assert.equal(updateScheduleRequestSchema.safeParse({ ...input, revision: 0 }).success, false);
    assert.equal(updateScheduleRequestSchema.safeParse({ ...input, revision: 1, ownerId: "other" }).success, false);
    assert.equal(
      updateScheduleRequestSchema.safeParse({ ...input, enabled: undefined, revision: 1 }).success,
      false,
    );
  });

  it("requires an explicit enabled value and revision for pause/resume", () => {
    for (const enabled of [true, false])
      assert.equal(scheduleEnabledRequestSchema.safeParse({ enabled, revision: 1 }).success, true);
    for (const invalid of [{ enabled: true }, { revision: 1 }, { enabled: "false", revision: 1 }, { enabled: false, revision: 0 }, { enabled: false, revision: 1, time: "12:00" }])
      assert.equal(scheduleEnabledRequestSchema.safeParse(invalid).success, false);
  });

  it("requires saved revisions and nullable nonnegative next-run timestamps", () => {
    const saved = {
      ...input,
      id: crypto.randomUUID(),
      serverId: crypto.randomUUID(),
      ownerId: crypto.randomUUID(),
      lastResult: null,
      lastSlot: null,
      revision: 1,
      nextRunAt: null,
    };
    assert.equal(savedScheduleSchema.safeParse(saved).success, true);
    assert.equal(savedScheduleSchema.safeParse({ ...saved, nextRunAt: 0 }).success, true);
    assert.equal(savedScheduleSchema.safeParse({ ...saved, nextRunAt: -1 }).success, false);
    assert.equal(savedScheduleSchema.safeParse({ ...saved, nextRunAt: undefined }).success, false);
    assert.equal(savedScheduleSchema.safeParse({ ...saved, revision: undefined }).success, false);
    assert.equal(savedScheduleSchema.safeParse({ ...saved, lastSlot: undefined }).success, false);
  });
});
