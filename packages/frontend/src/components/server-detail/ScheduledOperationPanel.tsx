import { operationStatusLabels } from "../../operations";
import { NavLink } from "../../navigation";
import { useOperation } from "../../hooks/useOperation";

interface Props {
  operationId: string;
  serverId: string;
  onClose: () => void;
}

/** A schedule can point beyond the recent list. Read that operation separately,
 * without making historical data part of the server's current operation locks. */
export default function ScheduledOperationPanel({ operationId, serverId, onClose }: Props) {
  const { operation, error, loading, running, refresh } = useOperation(operationId, serverId);

  return (
    <section className="scheduled-operation" aria-labelledby="scheduled-operation-title">
      <div className="section-heading">
        <h2 id="scheduled-operation-title" tabIndex={-1}>Scheduled operation</h2>
        <button type="button" className="secondary-btn" onClick={onClose}>Close operation</button>
      </div>
      {!operation && loading && <p role="status">Loading operation…</p>}
      {error && <div>
        <p role="alert">{error}</p>
        <button type="button" className="secondary-btn" disabled={loading} onClick={() => void refresh()}>Retry operation</button>
      </div>}
      {operation && <>
        <p><NavLink className="text-link" to={`/operations/${encodeURIComponent(operationId)}`}>Open operation details</NavLink></p>
        <dl className="metadata-list">
          <dt>Action</dt><dd className="capitalize">{operation.kind.replaceAll("_", " ")}</dd>
          <dt>Status</dt><dd role={running ? "status" : undefined}>{operationStatusLabels[operation.status]}</dd>
          <dt>Queued at</dt><dd><time dateTime={new Date(operation.createdAt).toISOString()}>{new Date(operation.createdAt).toLocaleString()}</time></dd>
          <dt>Last updated</dt><dd><time dateTime={new Date(operation.updatedAt).toISOString()}>{new Date(operation.updatedAt).toLocaleString()}</time></dd>
          <dt>Progress</dt><dd>{operation.phase.replaceAll("_", " ")}</dd>
        </dl>
        {operation.error && <p className="operation-error">{operation.error}</p>}
        {operation.result && typeof operation.result.guidance === "string" && <p className="section-note">{operation.result.guidance}</p>}
      </>}
    </section>
  );
}
