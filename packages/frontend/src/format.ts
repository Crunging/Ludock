const relative = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
const dateTime = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
const date = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** "5 minutes ago" and "yesterday" read faster than a full timestamp; older dates stay absolute. */
export function formatRelativeTime(timestamp: number, now = Date.now()): string {
  const elapsed = now - timestamp;
  const distance = Math.abs(elapsed);
  if (distance < MINUTE) return elapsed >= 0 ? "just now" : "in under a minute";
  if (distance < HOUR) return relative.format(-Math.round(elapsed / MINUTE), "minute");
  if (distance < DAY) return relative.format(-Math.round(elapsed / HOUR), "hour");
  if (distance < 7 * DAY) return relative.format(-Math.round(elapsed / DAY), "day");
  return date.format(timestamp);
}

/** For a relative time that stands alone, such as a table cell. */
export function formatRelativeTimeSentence(timestamp: number, now = Date.now()): string {
  const text = formatRelativeTime(timestamp, now);
  return text.charAt(0).toUpperCase() + text.slice(1);
}

export function formatDateTime(timestamp: number): string {
  return dateTime.format(timestamp);
}
