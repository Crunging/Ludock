import { expect, describe, it } from "bun:test";
import {
  nextScheduleRun,
  scheduleSlot,
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
