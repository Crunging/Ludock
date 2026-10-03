import { useCallback, useEffect, useRef, useState } from "react";
import {
  operationsResponseSchema,
  serverResponseSchema,
  type Operation,
  type Server,
} from "@ludock/shared";
import { ApiRequestError, apiJson } from "../api";

export interface ServerMutationOptions {
  requiresLive?: boolean;
  onFailure?: (reason: unknown) => Promise<void>;
}

/** Coordinates reads and writes for a mounted server/account session. Saved
 * settings remain manageable when Docker cannot provide a live snapshot. */
export function useServerDetail(path: string) {
  const [server, setServer] = useState<Server | null>(null);
  const [operations, setOperations] = useState<Operation[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [snapshotReady, setSnapshotReady] = useState(false);
  const [discoveryUnavailable, setDiscoveryUnavailable] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const pageActive = useRef(false);
  const refreshRequest = useRef<AbortController | null>(null);
  const mutationPending = useRef(false);

  const refresh = useCallback(async (replacePending = true, afterMutation = false) => {
    if (
      !pageActive.current ||
      (mutationPending.current && !afterMutation) ||
      (!replacePending && refreshRequest.current)
    ) return;
    refreshRequest.current?.abort();
    const controller = new AbortController();
    refreshRequest.current = controller;
    setSnapshotReady(false);
    const ownsRequest = () =>
      pageActive.current &&
      refreshRequest.current === controller &&
      !controller.signal.aborted;
    const init = { signal: controller.signal };
    try {
      const { server: next, discoveryUnavailable: unavailable } = await apiJson(path, serverResponseSchema, init);
      if (!ownsRequest()) return;
      setServer(next);
      setDiscoveryUnavailable(unavailable);
      const activity = await apiJson(`${path}/operations`, operationsResponseSchema, init);
      if (!ownsRequest()) return;
      setOperations(activity.operations);
      setSnapshotReady(true);
      setError(null);
    } catch (reason) {
      if (!ownsRequest()) return;
      setSnapshotReady(false);
      if (
        reason instanceof ApiRequestError &&
        (reason.status < 400 || [401, 403, 404].includes(reason.status))
      ) {
        setServer(null);
        setOperations([]);
      }
      setError(reason instanceof Error ? reason.message : "Unable to refresh server.");
    } finally {
      if (ownsRequest()) {
        refreshRequest.current = null;
        setLoading(false);
      }
    }
  }, [path]);

  useEffect(() => {
    pageActive.current = true;
    void refresh();
    return () => {
      pageActive.current = false;
      refreshRequest.current?.abort();
    };
  }, [refresh]);

  const liveReady = snapshotReady && !discoveryUnavailable;
  async function perform(
    action: () => Promise<unknown>,
    success: string,
    { requiresLive = true, onFailure }: ServerMutationOptions = {},
  ) {
    if (
      mutationPending.current ||
      refreshRequest.current ||
      !snapshotReady ||
      (requiresLive && !liveReady) ||
      !pageActive.current
    ) return false;
    mutationPending.current = true;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const result = await action();
      if (result === false || !pageActive.current) return false;
      setNotice(success);
      await refresh(true, true);
      // The action succeeded even if reading its updated state failed.
      return pageActive.current;
    } catch (reason) {
      if (pageActive.current) await onFailure?.(reason);
      if (pageActive.current)
        setError(reason instanceof Error ? reason.message : "Request failed.");
      return false;
    } finally {
      mutationPending.current = false;
      if (pageActive.current) setBusy(false);
    }
  }

  return {
    server, operations, loading, busy, snapshotReady, liveReady,
    discoveryUnavailable, error, notice, setError, refresh, perform,
    pageActive, mutationPending,
  };
}
