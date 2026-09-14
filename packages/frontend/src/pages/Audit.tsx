import { auditResponseSchema } from "@ludock/shared";
import HistoryFilters from "../components/HistoryFilters";
import HistoryPagination from "../components/HistoryPagination";
import { useHistory } from "../hooks/useHistory";
import { useHistoryServers } from "../hooks/useHistoryServers";
import { NavLink } from "../navigation";
import { historyActorLabel, operationStatusLabels } from "../operations";
import "./history.css";

export default function Audit() {
  const history = useHistory("/audit", auditResponseSchema, "Failed to load audit log");
  const names = useHistoryServers();
  const serverNames = new Map(names.servers.map((server) => [server.id, server.displayName]));
  const { data, error, loading, refresh } = history;
  const entries = data?.entries ?? [];

  return (
    <div className="page history-page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Audit log</h1>
          <p className="page__subtitle">Authentication, account, lifecycle, and shell activity.</p>
        </div>
        <button className="secondary-btn" disabled={loading} onClick={() => { void refresh(); void names.refresh(); }}>
          Refresh
        </button>
      </div>
      <HistoryFilters filters={history.filters} servers={names.servers} audit onSearch={history.apply} />
      {names.error && <p className="section-note">Server names are unavailable. Recorded server IDs still identify history.</p>}
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {loading && <p className="muted" role="status">Loading audit log…</p>}
      <div className="audit-list">
        {entries.map((entry) => (
          <article className="audit-entry" key={entry.id}>
            <div>
              <strong>{entry.action}</strong>
              <span>{historyActorLabel(entry.actor ?? (entry.username ? { id: null, name: entry.username } : null))}</span>
              {entry.status && <span>{operationStatusLabels[entry.status]}</span>}
            </div>
            <div>
              {entry.targetType === "server" && entry.targetId ? (
                <NavLink className="text-link" to={`/operations?serverId=${encodeURIComponent(entry.targetId)}`}>
                  {serverNames.get(entry.targetId) ?? `server: ${entry.targetId}`}
                </NavLink>
              ) : <span>{entry.targetType && entry.targetId ? `${entry.targetType}: ${entry.targetId}` : "—"}</span>}
              <time dateTime={new Date(entry.createdAt).toISOString()}>{new Date(entry.createdAt).toLocaleString()}</time>
              {entry.operationId && <div className="history-entry-links">
                <NavLink className="text-link" to={`/operations/${encodeURIComponent(entry.operationId)}`}>Operation details</NavLink>
              </div>}
            </div>
          </article>
        ))}
        {!loading && !error && entries.length === 0 && (
          <div className="empty-state">
            <div className="empty-state__title">{history.filterKey ? "No audit activity matches these filters" : "No audit activity yet"}</div>
          </div>
        )}
      </div>
      <HistoryPagination loading={loading} nextCursor={data?.nextCursor} hasCursor={history.hasCursor} onOlder={history.older} onNewer={history.newer} />
    </div>
  );
}
