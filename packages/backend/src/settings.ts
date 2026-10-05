import { connectionHostSchema } from "@ludock/shared";
import { getDatabase } from "./database.js";

export function getSetting<T>(key: string): T | null {
  const row = getDatabase()
    .query("SELECT value_json FROM settings WHERE key=?")
    .get(key) as { value_json: string } | null;
  return row ? (JSON.parse(row.value_json) as T) : null;
}
export function setSetting(key: string, value: unknown): void {
  getDatabase()
    .query(
      "INSERT INTO settings(key,value_json) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json",
    )
    .run(key, JSON.stringify(value));
}

const CONNECTION_HOST = "connection.host";

/** A stored value that no longer validates is treated as unset. */
export function connectionHost(): string | null {
  const parsed = connectionHostSchema.safeParse(getSetting<unknown>(CONNECTION_HOST));
  return parsed.success ? parsed.data : null;
}
export function setConnectionHost(host: string | null): void {
  if (host === null)
    getDatabase().query("DELETE FROM settings WHERE key=?").run(CONNECTION_HOST);
  else setSetting(CONNECTION_HOST, host);
}
