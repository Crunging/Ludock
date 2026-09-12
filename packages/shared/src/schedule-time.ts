import type { ScheduleInput } from "./schedules.js";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const MAX_FORMATTERS = 32;
const MAX_OFFSET_DAYS = 256;
const formatters = new Map<string, Intl.DateTimeFormat>();
const offsetDays = new Map<string, number[]>();

function cache<K, V>(entries: Map<K, V>, key: K, value: V, limit: number): V {
  if (entries.size >= limit) entries.delete(entries.keys().next().value!);
  entries.set(key, value);
  return value;
}

function formatter(timezone: string): Intl.DateTimeFormat {
  return (
    formatters.get(timezone) ??
    cache(
      formatters,
      timezone,
      new Intl.DateTimeFormat("en-US", {
        timeZone: timezone,
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        hourCycle: "h23",
      }),
      MAX_FORMATTERS,
    )
  );
}

function localClock(timezone: string, now: number) {
  const parts = formatter(timezone).formatToParts(now);
  const part = (type: string) => parts.find((item) => item.type === type)!.value;
  const date = `${part("year")}-${part("month")}-${part("day")}`;
  const dateEpoch = Date.UTC(
    Number(part("year")),
    Number(part("month")) - 1,
    Number(part("day")),
  );
  const second = Number(part("second"));
  const minuteEpoch =
    dateEpoch + Number(part("hour")) * HOUR + Number(part("minute")) * MINUTE;
  return {
    date,
    dateEpoch,
    time: `${part("hour")}:${part("minute")}`,
    minuteStart: Math.floor(now / 1_000) * 1_000 - second * 1_000,
    offset: minuteEpoch + second * 1_000 - Math.floor(now / 1_000) * 1_000,
  };
}

function validClockInput(input: ScheduleInput, now: number): boolean {
  return (
    input.enabled === true &&
    Number.isFinite(now) &&
    now >= 0 &&
    Number.isFinite(new Date(now).getTime()) &&
    typeof input.time === "string" &&
    /^(?:[01]\d|2[0-3]):[0-5]\d$/.test(input.time) &&
    Array.isArray(input.days) &&
    input.days.length > 0 &&
    input.days.length <= 7 &&
    input.days.every((day) => Number.isInteger(day) && day >= 0 && day <= 6) &&
    typeof input.timezone === "string" &&
    input.timezone.length > 0 &&
    input.timezone.length <= 100
  );
}

function clockSlot(
  input: ScheduleInput,
  clock: ReturnType<typeof localClock>,
): string | null {
  if (
    !input.days.includes(new Date(clock.dateEpoch).getUTCDay()) ||
    clock.time !== input.time
  )
    return null;
  // Repeated fall-back clock times share a slot, so destructive work runs once.
  return `${clock.date}T${input.time}:${input.timezone}`;
}

/** The matching local minute, without an offset so fall-back repetitions deduplicate. */
export function scheduleSlot(input: ScheduleInput, now: number): string | null {
  if (!validClockInput(input, now)) return null;
  try {
    return clockSlot(input, localClock(input.timezone, now));
  } catch {
    return null;
  }
}

function dayOffsets(timezone: string, dateEpoch: number): number[] {
  const key = `${timezone}:${dateEpoch}`;
  const existing = offsetDays.get(key);
  if (existing) return existing;
  const offsets = new Set<number>();
  // Sample the surrounding days to include offsets before and after a clock
  // change. Resolve and verify exact wall times below; never scan every minute.
  for (let hour = -24; hour <= 48; hour += 12) {
    const sample = dateEpoch + hour * HOUR;
    if (Number.isFinite(new Date(sample).getTime()))
      offsets.add(localClock(timezone, sample).offset);
  }
  return cache(offsetDays, key, [...offsets], MAX_OFFSET_DAYS);
}

/**
 * Next eligible local minute, including the current minute when it is unconsumed.
 * Missed minutes and spring-forward gaps are skipped, never caught up.
 */
export function nextScheduleRun(
  input: ScheduleInput,
  now: number,
  lastSlot?: string | null,
): number | null {
  if (!validClockInput(input, now)) return null;
  try {
    const current = localClock(input.timezone, now);
    const [hour, minute] = input.time.split(":").map(Number);
    // A rollback can return to yesterday, and a weekly slot can disappear in a
    // spring gap. Cover both yesterday and the week after a skipped occurrence.
    for (let day = -1; day <= 14; day += 1) {
      const dateEpoch = current.dateEpoch + day * DAY;
      const date = new Date(dateEpoch);
      if (!input.days.includes(date.getUTCDay())) continue;
      const wallTime = dateEpoch + hour * HOUR + minute * MINUTE;
      const candidates = dayOffsets(input.timezone, dateEpoch)
        .map((offset) => wallTime - offset)
        .filter((candidate) => candidate >= current.minuteStart && candidate >= 0)
        .sort((left, right) => left - right);
      for (const candidate of candidates) {
        const clock = localClock(input.timezone, candidate);
        const slot = clockSlot(input, clock);
        if (clock.dateEpoch === dateEpoch && slot && slot !== lastSlot)
          return candidate;
      }
    }
  } catch {
    // Invalid persisted or draft values must not break schedule lists/previews.
  }
  return null;
}
