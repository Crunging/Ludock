import { expect, describe, it } from "bun:test";
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
      expect(nextScheduleRun(input, at(now))).toBe(at("2026-09-07T09:30:00Z"));
    expect(nextScheduleRun(input, at("2026-09-07T09:31:00Z"))).toBe(at("2026-09-09T09:30:00Z"));
    expect(nextScheduleRun(input, at("2026-09-08T09:00:00Z"))).toBe(at("2026-09-09T09:30:00Z"));
  });

  it("excludes a consumed slot while including an unconsumed current minute", () => {
    const now = at("2026-09-07T09:30:42Z");
    const slot = scheduleSlot(input, now);
    expect(slot).toBe("2026-09-07T09:30:UTC");
    expect(nextScheduleRun(input, now)).toBe(at("2026-09-07T09:30:00Z"));
    expect(nextScheduleRun(input, now, slot)).toBe(at("2026-09-09T09:30:00Z"));
    expect(nextScheduleRun(input, now, "an older slot")).toBe(at("2026-09-07T09:30:00Z"));
  });

  it("offers both fall-back occurrences until that local slot has been consumed", () => {
    const fall = { ...input, timezone: "America/Los_Angeles", days: [0], time: "01:30" };
    const first = at("2026-11-01T08:30:00Z");
    const second = at("2026-11-01T09:30:00Z");
    const slot = scheduleSlot(fall, first);
    expect(slot).toBe(scheduleSlot(fall, second));
    expect(nextScheduleRun(fall, first - 60_000)).toBe(first);
    expect(nextScheduleRun(fall, first + 15_000)).toBe(first);
    expect(nextScheduleRun(fall, first + 60_000)).toBe(second);
    expect(nextScheduleRun(fall, second + 15_000)).toBe(second);
    expect(nextScheduleRun(fall, first, slot)).toBe(at("2026-11-08T09:30:00Z"));
    expect(nextScheduleRun(fall, second, slot)).toBe(at("2026-11-08T09:30:00Z"));
  });

  it("looks beyond seven days when the next weekly slot falls in a spring gap", () => {
    const spring = { ...input, timezone: "America/Los_Angeles", days: [0], time: "02:30" };
    expect(nextScheduleRun(spring, at("2026-03-01T10:31:00Z"))).toBe(at("2026-03-15T09:30:00Z"));
    expect(nextScheduleRun(spring, at("2026-03-08T09:59:00Z"))).toBe(at("2026-03-15T09:30:00Z"));
    expect(scheduleSlot(spring, at("2026-03-08T10:30:00Z"))).toBe(null);
  });

  it("uses local weekdays around midnight in fractional-offset timezones", () => {
    const nepal = { ...input, timezone: "Asia/Kathmandu", days: [1], time: "00:15" };
    const due = at("2026-09-06T18:30:00Z");
    expect(nextScheduleRun(nepal, at("2026-09-06T18:20:00Z"))).toBe(due);
    expect(scheduleSlot(nepal, due)).toBe("2026-09-07T00:15:Asia/Kathmandu");
    expect(nextScheduleRun(nepal, due + 30_000)).toBe(due);
    expect(nextScheduleRun(nepal, due + 60_000)).toBe(at("2026-09-13T18:30:00Z"));
  });

  it("handles half-hour daylight-saving transitions", () => {
    const fall = { ...input, timezone: "Australia/Lord_Howe", days: [0], time: "01:45" };
    const first = at("2026-04-04T14:45:00Z");
    const second = at("2026-04-04T15:15:00Z");
    expect(scheduleSlot(fall, first)).toBe(scheduleSlot(fall, second));
    expect(nextScheduleRun(fall, first + 60_000)).toBe(second);
    expect(nextScheduleRun(fall, first, scheduleSlot(fall, first))).toBe(at("2026-04-11T15:15:00Z"));
    expect(nextScheduleRun({ ...fall, time: "02:15" }, at("2026-10-03T14:00:00Z"))).toBe(at("2026-10-10T15:15:00Z"));
  });

  it("skips a local date removed by a timezone transition", () => {
    const samoa = { ...input, timezone: "Pacific/Apia", days: [5], time: "03:00" };
    expect(nextScheduleRun(samoa, at("2011-12-29T12:00:00Z"))).toBe(at("2012-01-05T13:00:00Z"));
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
    expect(nextScheduleRun(casey, now)).toBe(second);
    expect(scheduleSlot(casey, first)).toBe(scheduleSlot(casey, second));
    expect(nextScheduleRun(casey, now, scheduleSlot(casey, first))).toBe(at("2010-03-11T15:30:00Z"));
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
      expect(nextScheduleRun(invalid, now)).toBe(null);
      expect(scheduleSlot(invalid, now)).toBe(null);
    }
    for (const invalidNow of [NaN, Infinity, -1, 9e15]) {
      expect(nextScheduleRun(input, invalidNow)).toBe(null);
      expect(scheduleSlot(input, invalidNow)).toBe(null);
    }
  });
});

describe("schedule editing contracts", () => {
  it("requires a positive revision and full schedule input for edits", () => {
    expect(updateScheduleRequestSchema.safeParse({ ...input, revision: 1 }).success).toBe(true);
    expect(updateScheduleRequestSchema.safeParse(input).success).toBe(false);
    expect(updateScheduleRequestSchema.safeParse({ revision: 1, time: "12:00" }).success).toBe(false);
    expect(updateScheduleRequestSchema.safeParse({ ...input, revision: 0 }).success).toBe(false);
    expect(updateScheduleRequestSchema.safeParse({ ...input, revision: 1, ownerId: "other" }).success).toBe(false);
    expect(updateScheduleRequestSchema.safeParse({ ...input, enabled: undefined, revision: 1 }).success).toBe(false);
  });

  it("requires an explicit enabled value and revision for pause/resume", () => {
    for (const enabled of [true, false])
      expect(scheduleEnabledRequestSchema.safeParse({ enabled, revision: 1 }).success).toBe(true);
    for (const invalid of [{ enabled: true }, { revision: 1 }, { enabled: "false", revision: 1 }, { enabled: false, revision: 0 }, { enabled: false, revision: 1, time: "12:00" }])
      expect(scheduleEnabledRequestSchema.safeParse(invalid).success).toBe(false);
  });

  it("requires saved revisions and nullable nonnegative next-run timestamps", () => {
    const saved = {
      ...input,
      id: crypto.randomUUID(),
      serverId: crypto.randomUUID(),
      ownerId: crypto.randomUUID(),
      lastResult: null,
      lastOperation: null,
      lastRunAt: null,
      lastSlot: null,
      revision: 1,
      nextRunAt: null,
      nextRunUnavailableReason: null,
    };
    expect(savedScheduleSchema.safeParse(saved).success).toBe(true);
    expect(savedScheduleSchema.safeParse({ ...saved, nextRunAt: 0 }).success).toBe(true);
    expect(savedScheduleSchema.safeParse({ ...saved, nextRunAt: -1 }).success).toBe(false);
    expect(savedScheduleSchema.safeParse({ ...saved, nextRunAt: undefined }).success).toBe(false);
    expect(savedScheduleSchema.safeParse({ ...saved, revision: undefined }).success).toBe(false);
    expect(savedScheduleSchema.safeParse({ ...saved, lastSlot: undefined }).success).toBe(false);
    for (const missing of ["lastOperation", "lastRunAt", "nextRunUnavailableReason"])
      expect(savedScheduleSchema.safeParse({ ...saved, [missing]: undefined }).success).toBe(false);
    expect(savedScheduleSchema.safeParse({ ...saved, lastRunAt: -1 }).success).toBe(false);
    expect(savedScheduleSchema.safeParse({ ...saved, nextRunUnavailableReason: "private error" }).success).toBe(false);
  });
});
