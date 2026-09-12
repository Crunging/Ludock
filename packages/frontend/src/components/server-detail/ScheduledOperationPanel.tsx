import { useCallback, useEffect, useRef, useState } from "react";
import { operationResponseSchema, type Operation } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../../api";
import { operationActive, operationStatusLabels } from "../../operations";

interface Props {
  operationId: string;
  serverId: string;
  onClose: () => void;
}

/** A schedule can point beyond the recent list. Read that operation separately,
 * without making historical data part of the server's current operation locks. */
export default function ScheduledOperationPanel({ operationId, serverId, onClose }: Props) {
  const [operation, setOperation] = useState<Operation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const request = useRef<AbortController | null>(null);
  const active = useRef(false);

  const refresh = useCallback(async () => {
    if (!active.current || request.current) return;
    const controller = new AbortController();
    request.current = controller;
    const ownsRequest = () => active.current && request.current === controller && !controller.signal.aborted;
    setLoading(true);
    try {
      const result = await apiJson(`/operations/${encodeURIComponent(operationId)}`, operationResponseSchema, { signal: controller.signal });
      if (!ownsRequest()) return;
      if (result.operation.id !== operationId || result.operation.serverId !== serverId)
        throw new Error("Operation unavailable.");
      setOperation(result.operation);
      setError(null);
    } catch (reason) {
      if (!ownsRequest()) return;
      setOperation(null);
      setError(reason instanceof ApiRequestError && [401, 403, 404].includes(reason.status)
        ? "This operation is no longer available or you no longer have access."
        : "Unable to load the current operation result. Try again.");
    } finally {
      if (ownsRequest()) {
        request.current = null;
        setLoading(false);
      }
    }
  }, [operationId, serverId]);

  useEffect(() => {
    active.current = true;
    void refresh();
    return () => {
      active.current = false;
      request.current?.abort();
      request.current = null;
    };
  }, [refresh]);
  const running = operation !== null && operationActive(operation);
  useEffect(() => {
    const interval = window.setInterval(() => { void refresh(); }, running ? 2000 : 10000);
    return () => window.clearInterval(interval);
  }, [refresh, running]);

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
