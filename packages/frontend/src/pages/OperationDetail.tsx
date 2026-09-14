import { useAuth } from "../auth-context";
import { useOperation } from "../hooks/useOperation";
import { useHistoryServers } from "../hooks/useHistoryServers";
import { NavLink } from "../navigation";
import { historyActorLabel, operationStatusLabels } from "../operations";
import "./history.css";

export default function OperationDetail({ operationId }: { operationId: string }) {
  const { user } = useAuth();
  const { operation, error, loading, running, refresh } = useOperation(operationId);
  const names = useHistoryServers();
  const server = names.servers.find((candidate) => candidate.id === operation?.serverId);
  return (
    <div className="page history-page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Operation details</h1>
          <p className="page__subtitle">Progress and the recorded result of this operation.</p>
        </div>
        <button className="secondary-btn" disabled={loading} onClick={() => void refresh()}>Refresh</button>
      </div>
      {!operation && loading && <p role="status">Loading operation…</p>}
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      {operation && <>
        <dl className="metadata-list operation-detail-summary">
          <dt>Action</dt><dd className="capitalize">{operation.kind.replaceAll("_", " ")}</dd>
          <dt>Status</dt><dd role={running ? "status" : undefined}>{operationStatusLabels[operation.status]}</dd>
          <dt>Server</dt><dd><NavLink className="text-link" to={`/servers/${encodeURIComponent(operation.serverId)}`}>{server?.displayName ?? operation.serverId}</NavLink></dd>
          <dt>Actor</dt><dd>{historyActorLabel(operation.actor)}</dd>
          <dt>Operation ID</dt><dd>{operation.id}</dd>
          <dt>Queued at</dt><dd><time dateTime={new Date(operation.createdAt).toISOString()}>{new Date(operation.createdAt).toLocaleString()}</time></dd>
          <dt>Last updated</dt><dd><time dateTime={new Date(operation.updatedAt).toISOString()}>{new Date(operation.updatedAt).toLocaleString()}</time></dd>
          <dt>Progress</dt><dd>{operation.phase.replaceAll("_", " ")}</dd>
        </dl>
        {operation.error && <p className="operation-error">{operation.error}</p>}
        {operation.result && typeof operation.result.guidance === "string" && <p className="section-note">{operation.result.guidance}</p>}
        <div className="inline-actions operation-detail-actions history-entry-links">
          <NavLink className="text-link" to={`/operations?serverId=${encodeURIComponent(operation.serverId)}`}>Server operation history</NavLink>
          {user?.role === "admin" && <NavLink className="text-link" to={`/audit?operationId=${encodeURIComponent(operation.id)}`}>Related audit events</NavLink>}
        </div>
      </>}
      {!operation && !loading && <p><NavLink className="text-link" to="/operations">Operation history</NavLink></p>}
    </div>
  );
}
