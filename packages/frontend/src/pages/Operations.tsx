import { operationsResponseSchema } from "@ludock/shared";
import HistoryFilters from "../components/HistoryFilters";
import HistoryPagination from "../components/HistoryPagination";
import OperationList from "../components/OperationList";
import { useHistory } from "../hooks/useHistory";
import { useHistoryServers } from "../hooks/useHistoryServers";
import "./history.css";

export default function Operations() {
  const history = useHistory("/operations", operationsResponseSchema, "Failed to load operation history");
  const names = useHistoryServers();
  const { data, error, loading, refresh } = history;
  return (
    <div className="page history-page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Operation history</h1>
          <p className="page__subtitle">Search persisted work for servers you can access.</p>
        </div>
        <button className="secondary-btn" disabled={loading} onClick={() => { void refresh(); void names.refresh(); }}>Refresh</button>
      </div>
      <HistoryFilters filters={history.filters} servers={names.servers} onSearch={history.apply} />
      {names.error && <p className="section-note">Server names are unavailable. Recorded server IDs still identify history.</p>}
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {loading && <p className="muted" role="status">Loading operation history…</p>}
      {!loading && !error && <OperationList
        operations={data?.operations ?? []}
        showContext
        serverNames={new Map(names.servers.map((server) => [server.id, server.displayName]))}
        emptyMessage={history.filterKey ? "No operations match these filters." : "No operations yet."}
      />}
      <HistoryPagination loading={loading} nextCursor={data?.nextCursor} hasCursor={history.hasCursor} onOlder={history.older} onNewer={history.newer} />
    </div>
  );
}
