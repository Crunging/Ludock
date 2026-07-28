import { useEffect, useState } from "react";
import { apiFetch } from "../api";

interface AuditEntry {
  id: number;
  username: string | null;
  action: string;
  targetType: string | null;
  targetId: string | null;
  createdAt: number;
}

export default function Audit() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    apiFetch("/api/audit")
      .then(async (response) => {
        const body = (await response.json().catch(() => ({}))) as {
          entries?: AuditEntry[];
          error?: string;
        };
        if (!response.ok || !body.entries) {
          throw new Error(body.error || "Failed to load audit log");
        }
        setEntries(body.entries);
      })
      .catch((reason: unknown) => {
        setError(
          reason instanceof Error ? reason.message : "Failed to load audit log"
        );
      });
  }, []);

  return (
    <div className="page">
      <div className="page__header">
        <h1 className="page__title">Audit log</h1>
        <p className="page__subtitle">
          Authentication, account, lifecycle, and shell activity.
        </p>
      </div>
      {error && <div className="alert alert--error">{error}</div>}
      <div className="audit-list">
        {entries.map((entry) => (
          <article className="audit-entry" key={entry.id}>
            <div>
              <strong>{entry.action}</strong>
              <span>{entry.username || "System/API token"}</span>
            </div>
            <div>
              <span>
                {entry.targetType && entry.targetId
                  ? `${entry.targetType}: ${entry.targetId}`
                  : "—"}
              </span>
              <time dateTime={new Date(entry.createdAt).toISOString()}>
                {new Date(entry.createdAt).toLocaleString()}
              </time>
            </div>
          </article>
        ))}
        {!error && entries.length === 0 && (
          <div className="empty-state">
            <div className="empty-state__title">No audit activity yet</div>
          </div>
        )}
      </div>
    </div>
  );
}
