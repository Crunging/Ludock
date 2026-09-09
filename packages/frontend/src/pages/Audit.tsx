import { useCallback, useEffect, useRef, useState } from "react";
import { apiJson } from "../api";

import { type AuditEntry, auditResponseSchema } from "@ludock/shared";

export default function Audit() {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const request = useRef<AbortController | null>(null);

  const refresh = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    setError(null);
    setEntries([]);
    try {
      const body = await apiJson("/audit", auditResponseSchema, { signal: controller.signal });
      if (!controller.signal.aborted && request.current === controller) setEntries(body.entries);
    } catch (reason) {
      if (!controller.signal.aborted && request.current === controller)
        setError(reason instanceof Error ? reason.message : "Failed to load audit log");
    } finally {
      if (!controller.signal.aborted && request.current === controller) {
        request.current = null;
        setLoading(false);
      }
    }
  }, []);
  useEffect(() => {
    void refresh();
    return () => request.current?.abort();
  }, [refresh]);

  return (
    <div className="page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Audit log</h1>
          <p className="page__subtitle">
            Authentication, account, lifecycle, and shell activity.
          </p>
        </div>
        <button className="secondary-btn" disabled={loading} onClick={() => void refresh()}>
          Refresh
        </button>
      </div>
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {loading && <p className="muted" role="status">Loading audit log…</p>}
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
        {!loading && !error && entries.length === 0 && (
          <div className="empty-state">
            <div className="empty-state__title">No audit activity yet</div>
          </div>
        )}
      </div>
    </div>
  );
}
