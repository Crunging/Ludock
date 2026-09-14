import { useCallback, useEffect, useRef, useState } from "react";
import { operationResponseSchema, type Operation } from "@ludock/shared";
import { ApiRequestError, apiJson } from "../api";
import { operationActive } from "../operations";

/** A historical detail read never participates in server control locks. Recheck
 * access while displaying the result, and discard it on any failed refresh. */
export function useOperation(operationId: string, serverId?: string) {
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
      if (result.operation.id !== operationId || (serverId && result.operation.serverId !== serverId))
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
    setOperation(null);
    setError(null);
    void refresh();
    return () => {
      active.current = false;
      request.current?.abort();
      request.current = null;
    };
  }, [refresh]);
  const current = operation?.id === operationId && (!serverId || operation.serverId === serverId) ? operation : null;
  const running = current !== null && operationActive(current);
  useEffect(() => {
    const interval = window.setInterval(() => { void refresh(); }, running ? 2000 : 10000);
    return () => window.clearInterval(interval);
  }, [refresh, running]);

  return { operation: current, error, loading, running, refresh };
}
