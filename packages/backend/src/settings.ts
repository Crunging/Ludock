import { getDatabase } from "./database.js";

export function getSetting<T>(key: string): T | null {
  const row = getDatabase()
    .prepare("SELECT value_json FROM settings WHERE key=?")
    .get(key) as { value_json: string } | null;
  return row ? (JSON.parse(row.value_json) as T) : null;
}
export function setSetting(key: string, value: unknown): void {
  getDatabase()
    .prepare(
      "INSERT INTO settings(key,value_json) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value_json=excluded.value_json",
    )
    .run(key, JSON.stringify(value));
}
