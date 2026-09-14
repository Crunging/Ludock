import type { NotificationDelivery } from "@ludock/shared";
import { getDatabase } from "./database.js";
import { getSetting, setSetting } from "./settings.js";
import { AppError } from "./errors.js";

interface NotificationSettings {
  enabled: boolean;
  webhookUrl: string;
}
export function notificationConfiguration(): {
  enabled: boolean;
  configured: boolean;
} {
  const config = getSetting<NotificationSettings>("notifications");
  return {
    enabled: config?.enabled ?? false,
    configured: Boolean(config?.webhookUrl),
  };
}
export function configureNotifications(
  enabled: boolean,
  webhookUrl?: string,
): void {
  const url =
    webhookUrl ??
    getSetting<NotificationSettings>("notifications")?.webhookUrl ??
    "";
  if (url) {
    let parsed: URL;
    try {
      parsed = new URL(url);
    } catch {
      throw new AppError(
        "INVALID_WEBHOOK",
        400,
        "Enter a valid Discord webhook URL",
      );
    }
    if (
      parsed.protocol !== "https:" ||
      parsed.hostname !== "discord.com" ||
      parsed.port ||
      parsed.username ||
      parsed.password ||
      parsed.search ||
      parsed.hash ||
      !/^\/api\/webhooks\/\d+\/[A-Za-z0-9_-]+$/.test(parsed.pathname)
    ) {
      throw new AppError(
        "INVALID_WEBHOOK",
        400,
        "Use a Discord HTTPS webhook URL",
      );
    }
  }
  if (enabled && !url)
    throw new AppError(
      "WEBHOOK_REQUIRED",
      400,
      "Configure a webhook before enabling notifications",
    );
  setSetting("notifications", { enabled, webhookUrl: url });
}
const failureMessages = {
  invalid_request: "Discord rejected the notification. Check the webhook configuration.",
  unauthorized: "Discord refused webhook access. Replace the saved webhook URL.",
  not_found: "Discord could not find the webhook. It may have been deleted; replace the saved URL.",
  rate_limited: "Discord rate limited delivery. Wait before trying again.",
  unavailable: "Discord is temporarily unavailable. Try again later.",
  rejected: "Discord rejected delivery. Check the webhook configuration and try again.",
  timeout: "Discord did not respond in time. Check network access and try again.",
  network: "Unable to reach Discord. Check network access and try again.",
} as const;
type FailureCode = keyof typeof failureMessages;
interface DeliveryRow {
  id: string;
  kind: NotificationDelivery["kind"];
  state: NotificationDelivery["state"];
  attempts: number;
  retry_attempts: number;
  created_at: number;
  last_attempt_at: number | null;
  delivered_at: number | null;
  next_attempt_at: number;
  failure_code: string | null;
}
const deliveryColumns = `id,kind,state,attempts,retry_attempts,created_at,
  last_attempt_at,delivered_at,next_attempt_at,failure_code`;
const inFlight = new Set<string>();

function publicDelivery(row: DeliveryRow): NotificationDelivery {
  const lastFailure = row.failure_code && Object.hasOwn(failureMessages, row.failure_code)
    ? failureMessages[row.failure_code as FailureCode]
    : row.state !== "delivered" && row.attempts > 0
      ? "Failure details are unavailable for this delivery."
      : null;
  return {
    id: row.id,
    kind: row.kind,
    state: row.state,
    attempts: row.attempts,
    createdAt: row.created_at,
    lastAttemptAt: row.last_attempt_at,
    deliveredAt: row.delivered_at,
    nextAttemptAt: row.state === "queued" ? row.next_attempt_at : null,
    lastFailure,
    retryable: notificationConfiguration().enabled && !inFlight.has(row.id) &&
      (row.state === "failed" || (row.state === "queued" && row.retry_attempts > 0)),
  };
}

function deliveryRow(id: string): DeliveryRow {
  const row = getDatabase().prepare(`SELECT ${deliveryColumns} FROM notification_deliveries WHERE id=?`)
    .get(id) as DeliveryRow | null;
  if (!row) throw new AppError("NOTIFICATION_NOT_FOUND", 404, "Notification delivery not found");
  return row;
}

export function listNotificationDeliveries(): NotificationDelivery[] {
  const rows = getDatabase().prepare(`SELECT ${deliveryColumns} FROM notification_deliveries
    ORDER BY created_at DESC, id DESC LIMIT 50`).all() as DeliveryRow[];
  return rows.map(publicDelivery);
}

function assertDeliveryEnabled(): void {
  const configuration = notificationConfiguration();
  if (!configuration.enabled || !configuration.configured)
    throw new AppError("NOTIFICATIONS_DISABLED", 400, "Save an enabled Discord webhook before sending or retrying a notification");
}

function enqueueNotification(key: string, message: string, kind: NotificationDelivery["kind"]): string {
  const id = crypto.randomUUID();
  const now = Date.now();
  getDatabase().prepare(`INSERT OR IGNORE INTO notification_deliveries
    (id,event_key,payload_json,next_attempt_at,created_at,kind) VALUES(?,?,?,?,?,?)`)
    .run(id, key, JSON.stringify({ content: message.slice(0, 1800), allowed_mentions: { parse: [] } }), now, now, kind);
  return id;
}

export function notifyEvent(key: string, message: string): void {
  if (!notificationConfiguration().enabled) return;
  enqueueNotification(key, message, "event");
}

export function queueTestNotification(): NotificationDelivery {
  assertDeliveryEnabled();
  const id = enqueueNotification(`test:${crypto.randomUUID()}`,
    "Ludock test notification: Discord delivery is working.", "test");
  return publicDelivery(deliveryRow(id));
}

export function retryNotificationDelivery(id: string): NotificationDelivery {
  assertDeliveryEnabled();
  const row = deliveryRow(id);
  if (!publicDelivery(row).retryable)
    throw new AppError("NOTIFICATION_NOT_RETRYABLE", 409, "This delivery is already queued, being sent, or has been delivered");
  // Preserve lifetime attempts and the previous failure while granting a fresh
  // bounded retry cycle. Clearing its cycle count also rejects duplicate clicks.
  getDatabase().prepare(`UPDATE notification_deliveries
    SET state='queued',retry_attempts=0,next_attempt_at=? WHERE id=?`).run(Date.now(), id);
  return publicDelivery(deliveryRow(id));
}

function responseFailure(status: number): FailureCode {
  if (status === 400) return "invalid_request";
  if (status === 401 || status === 403) return "unauthorized";
  if (status === 404) return "not_found";
  if (status === 429) return "rate_limited";
  if (status >= 500) return "unavailable";
  return "rejected";
}

let delivering = false;
export async function deliverNotifications(
  fetcher: typeof fetch = fetch,
): Promise<void> {
  if (delivering || !notificationConfiguration().enabled) return;
  delivering = true;
  try {
    const rows = getDatabase().prepare(`SELECT id FROM notification_deliveries
      WHERE state='queued' AND next_attempt_at<=? ORDER BY created_at, rowid LIMIT 10`)
      .all(Date.now()) as { id: string }[];
    for (const { id } of rows) {
      const config = getSetting<NotificationSettings>("notifications");
      if (!config?.enabled) break;
      // A previous send can yield while an administrator retries another row.
      // Read its current counters immediately before sending, not from the batch.
      const row = deliveryRow(id);
      if (row.state !== "queued" || row.next_attempt_at > Date.now()) continue;
      const payload = getDatabase().prepare("SELECT payload_json FROM notification_deliveries WHERE id=?")
        .get(id) as { payload_json: string };
      inFlight.add(id);
      try {
        const startedAt = Date.now();
        getDatabase().prepare("UPDATE notification_deliveries SET last_attempt_at=? WHERE id=?")
          .run(startedAt, id);
        let failure: FailureCode | null = null;
        const signal = AbortSignal.timeout(10_000);
        try {
          // Discord only confirms that it saved the message when wait=true.
          // Saved URLs reject query strings, so this option is controlled here.
          const response = await fetcher(`${config.webhookUrl}?wait=true`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: payload.payload_json,
            redirect: "error",
            signal,
          });
          failure = response.ok ? null : responseFailure(response.status);
          // Neither the response body/status text nor a fetch exception is safe
          // to store: any of them could include the webhook secret.
          await response.body?.cancel().catch(() => {});
        } catch {
          failure = signal.aborted ? "timeout" : "network";
        }
        const attempts = row.attempts + 1;
        const retryAttempts = row.retry_attempts + 1;
        const finishedAt = Date.now();
        getDatabase().prepare(`UPDATE notification_deliveries
          SET attempts=?,retry_attempts=?,state=?,next_attempt_at=?,delivered_at=?,failure_code=? WHERE id=?`)
          .run(attempts, retryAttempts, failure === null ? "delivered" : retryAttempts >= 5 ? "failed" : "queued",
            finishedAt + Math.min(3600_000, 30_000 * 2 ** retryAttempts),
            failure === null ? finishedAt : null, failure, id);
      } finally {
        inFlight.delete(id);
      }
    }
    // Newly delivered older retries get the same retention as fresh deliveries.
    getDatabase().prepare(`DELETE FROM notification_deliveries
      WHERE state='delivered' AND COALESCE(delivered_at,created_at)<?`)
      .run(Date.now() - 30 * 86400_000);
  } finally {
    delivering = false;
  }
}
