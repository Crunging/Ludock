import { expect, describe, it } from "bun:test";
import {
  nextScheduleRun,
  ONE_TIME_SCHEDULE_GRACE_MS,
  oneTimeScheduleRun,
  oneTimeScheduleSlot,
  scheduleLocalDate,
  scheduleSchema,
  scheduleSlot,
  type OneTimeScheduleInput,
} from "@ludock/shared";

const input = {
  kind: "recurring" as const,
  action: "restart" as const,
  createBackup: false,
  enabled: true,
  time: "09:30",
  days: [1, 3],
  timezone: "UTC",
};
const at = (value: string) => Date.parse(value);

describe("next schedule run", () => {
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

  it("handles half-hour daylight-saving transitions", () => {
    const fall = { ...input, timezone: "Australia/Lord_Howe", days: [0], time: "01:45" };
    const first = at("2026-04-04T14:45:00Z");
    const second = at("2026-04-04T15:15:00Z");
    expect(scheduleSlot(fall, first)).toBe(scheduleSlot(fall, second));
    expect(nextScheduleRun(fall, first + 60_000)).toBe(second);
    expect(nextScheduleRun(fall, first, scheduleSlot(fall, first))).toBe(at("2026-04-11T15:15:00Z"));
    expect(nextScheduleRun({ ...fall, time: "02:15" }, at("2026-10-03T14:00:00Z"))).toBe(at("2026-10-10T15:15:00Z"));
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
});

describe("one-time schedule runs", () => {
  const once: OneTimeScheduleInput = {
    kind: "once",
    action: "stop",
    createBackup: false,
    enabled: true,
    date: "2026-11-01",
    time: "09:30",
    timezone: "America/Los_Angeles",
  };

  it("resolves its local date and time once, in its own time zone", () => {
    expect(oneTimeScheduleRun(once)).toBe(at("2026-11-01T17:30:00Z"));
    expect(oneTimeScheduleRun({ ...once, timezone: "UTC" })).toBe(at("2026-11-01T09:30:00Z"));
    // A repeated fall-back hour resolves to its first occurrence.
    expect(oneTimeScheduleRun({ ...once, time: "01:30" })).toBe(at("2026-11-01T08:30:00Z"));
    // A spring-forward gap has no such local time.
    expect(oneTimeScheduleRun({ ...once, date: "2026-03-08", time: "02:30" })).toBe(null);
  });

  it("stays due through its grace period until its single slot is consumed", () => {
    const runAt = at("2026-11-01T17:30:00Z");
    const slot = oneTimeScheduleSlot(once);
    expect(nextScheduleRun(once, runAt - 86400_000)).toBe(runAt);
    expect(nextScheduleRun(once, runAt + 60_000)).toBe(runAt);
    expect(nextScheduleRun(once, runAt + ONE_TIME_SCHEDULE_GRACE_MS)).toBe(null);
    expect(nextScheduleRun(once, runAt, slot)).toBe(null);
    expect(nextScheduleRun({ ...once, enabled: false }, runAt)).toBe(null);
    expect(scheduleSlot(once, runAt - 1)).toBe(null);
    expect(scheduleSlot(once, runAt)).toBe(slot);
    expect(scheduleSlot(once, runAt + ONE_TIME_SCHEDULE_GRACE_MS - 1)).toBe(slot);
    expect(scheduleSlot(once, runAt + ONE_TIME_SCHEDULE_GRACE_MS)).toBe(null);
  });

  it("accepts only real calendar dates and requires the fields of its kind", () => {
    expect(scheduleSchema.safeParse(once).success).toBe(true);
    expect(scheduleSchema.safeParse({ ...once, date: "2026-02-30" }).success).toBe(false);
    expect(scheduleSchema.safeParse({ ...once, date: "2026-2-3" }).success).toBe(false);
    expect(scheduleSchema.safeParse({ ...once, days: [1] }).success).toBe(false);
    expect(scheduleSchema.safeParse({ ...input, date: "2026-11-01" }).success).toBe(false);
    expect(scheduleSchema.safeParse({ ...input, kind: undefined }).success).toBe(false);
  });

  it("reports today's date in a schedule's time zone", () => {
    const now = at("2026-11-01T03:00:00Z");
    expect(scheduleLocalDate("UTC", now)).toBe("2026-11-01");
    expect(scheduleLocalDate("America/Los_Angeles", now)).toBe("2026-10-31");
    expect(scheduleLocalDate("Not/AZone", now)).toBe(null);
  });
});
