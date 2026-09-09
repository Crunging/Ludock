import { randomUUID } from "node:crypto";
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
export function notifyEvent(key: string, message: string): void {
  if (!notificationConfiguration().enabled) return;
  getDatabase()
    .prepare(
      "INSERT OR IGNORE INTO notification_deliveries(id,event_key,payload_json,next_attempt_at,created_at) VALUES(?,?,?,?,?)",
    )
    .run(
      randomUUID(),
      key,
      JSON.stringify({
        content: message.slice(0, 1800),
        allowed_mentions: { parse: [] },
      }),
      Date.now(),
      Date.now(),
    );
}
let delivering = false;
export async function deliverNotifications(
  fetcher: typeof fetch = fetch,
): Promise<void> {
  if (delivering) return;
  if (!notificationConfiguration().enabled) return;
  delivering = true;
  try {
    const rows = getDatabase()
      .prepare(
        "SELECT id,payload_json,attempts FROM notification_deliveries WHERE state='queued' AND next_attempt_at<=? ORDER BY created_at LIMIT 10",
      )
      .all(Date.now()) as Array<{
      id: string;
      payload_json: string;
      attempts: number;
    }>;
    for (const row of rows) {
      const config = getSetting<NotificationSettings>("notifications");
      if (!config?.enabled) break;
      let succeeded = false;
      try {
        const response = await fetcher(config.webhookUrl, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: row.payload_json,
          redirect: "error",
          signal: AbortSignal.timeout(10_000),
        });
        succeeded = response.ok;
        await response.body?.cancel();
      } catch {
        /* Never persist a fetch exception: it may contain the webhook secret. */
      }
      const attempts = row.attempts + 1;
      getDatabase()
        .prepare(
          "UPDATE notification_deliveries SET attempts=?,state=?,next_attempt_at=? WHERE id=?",
        )
        .run(
          attempts,
          succeeded ? "delivered" : attempts >= 5 ? "failed" : "queued",
          Date.now() + Math.min(3600_000, 30_000 * 2 ** attempts),
          row.id,
        );
    }
    getDatabase()
      .prepare(
        "DELETE FROM notification_deliveries WHERE state='delivered' AND created_at<?",
      )
      .run(Date.now() - 30 * 86400_000);
  } finally {
    delivering = false;
  }
}
