import { apiJson } from "../api";
import { usePageRead } from "../hooks/usePageRead";

import { auditResponseSchema } from "@ludock/shared";

const readAudit = (signal: AbortSignal) => apiJson("/audit", auditResponseSchema, { signal });

export default function Audit() {
  const { data, error, loading, refresh } = usePageRead(readAudit, "Failed to load audit log");
  const entries = data?.entries ?? [];

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
