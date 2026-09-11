import { serversResponseSchema, serverEventSchema, type Server } from "@ludock/shared";
import { useCallback, useEffect, useRef, useState } from "react";
import { useWebSocket, type ConnectionStatus } from "./useWebSocket";
import { ApiRequestError, apiJson, authenticatedWebSocketUrl } from "../api";
import { useAuth } from "../auth-context";

interface UseServersResult {
  servers: Server[];
  loading: boolean;
  error: string | null;
  stale: boolean;
  lastUpdated: number | null;
  connectionStatus: ConnectionStatus;
  connectionError: string | null;
  accessDenied: boolean;
  canRetry: boolean;
  retry: () => void;
  refresh: () => Promise<void>;
}

interface Snapshot {
  actor: string;
  servers: Server[];
  lastUpdated: number | null;
  loading: boolean;
  error: string | null;
}

interface SnapshotRequest {
  controller: AbortController;
  invalidated: boolean;
}

export function useServers(): UseServersResult {
  const { user } = useAuth();
  const actor = `${user?.id || ""}:${user?.role || ""}`;
  const [snapshot, setSnapshot] = useState<Snapshot>({
    actor, servers: [], lastUpdated: null, loading: true, error: null,
  });
  const requestRef = useRef<SnapshotRequest | null>(null);
  const activeRef = useRef(false);

  const fetchServers = useCallback(async () => {
    if (!activeRef.current) return;
    requestRef.current?.controller.abort();
    const request: SnapshotRequest = { controller: new AbortController(), invalidated: false };
    requestRef.current = request;
    setSnapshot((previous) => ({
      ...(previous.actor === actor ? previous : { actor, servers: [], lastUpdated: null }),
      loading: true,
      error: null,
    }));
    try {
      do {
        request.invalidated = false;
        try {
          const body = await apiJson("/servers", serversResponseSchema, { signal: request.controller.signal });
          if (requestRef.current !== request || request.controller.signal.aborted || !activeRef.current) return;
          if (request.invalidated) continue;
          setSnapshot({ actor, servers: body.servers, lastUpdated: Date.now(), loading: false, error: null });
        } catch (error: unknown) {
          if (requestRef.current !== request || request.controller.signal.aborted || !activeRef.current) return;
          // Only transient transport/server failures may keep an explicitly stale
          // snapshot. Authorization and contract failures must clear its contents.
          const transient = !(error instanceof ApiRequestError) || error.status >= 500;
          const loading = request.invalidated;
          if (loading && transient) continue;
          setSnapshot((previous) => ({
            actor,
            servers: transient && previous.actor === actor ? previous.servers : [],
            lastUpdated: transient && previous.actor === actor ? previous.lastUpdated : null,
            loading,
            error: error instanceof Error ? error.message : "Failed to fetch servers",
          }));
        }
      } while (request.invalidated);
    } finally {
      if (requestRef.current === request) requestRef.current = null;
    }
  }, [actor]);

  const handleEvent = useCallback((raw: string) => {
    try {
      const event = serverEventSchema.parse(JSON.parse(raw));
      if (event.type === "container_event") {
        // Docker often emits several events for one operation. Finish the
        // in-flight read, discard its outdated result, then read once more.
        // Manual refreshes and new connections still supersede it immediately.
        if (requestRef.current) requestRef.current.invalidated = true;
        else void fetchServers();
      }
    } catch { /* Malformed events never invalidate a verified snapshot. */ }
  }, [fetchServers]);

  const { status, error: connectionError, retry, canRetry, accessDenied } = useWebSocket({
    url: authenticatedWebSocketUrl("/ws/v1/events"),
    onMessage: handleEvent,
    // Events during a disconnect are not replayed. Every connection needs a
    // fresh authorized snapshot, including the first connection after mount.
    onOpen: fetchServers,
  });

  useEffect(() => {
    activeRef.current = true;
    void fetchServers();
    return () => {
      activeRef.current = false;
      requestRef.current?.controller.abort();
      requestRef.current = null;
    };
  }, [fetchServers]);

  useEffect(() => {
    // A policy close may mean session expiry or revoked access. Hide cached
    // data immediately and let the HTTP response revalidate authentication.
    if (accessDenied) void fetchServers();
  }, [accessDenied, fetchServers]);

  const currentActor = snapshot.actor === actor && !accessDenied;
  return {
    servers: currentActor ? snapshot.servers : [],
    loading: snapshot.actor !== actor || snapshot.loading,
    error: currentActor ? snapshot.error : null,
    lastUpdated: currentActor ? snapshot.lastUpdated : null,
    stale: !currentActor || snapshot.lastUpdated === null || Boolean(snapshot.error) || status !== "connected",
    connectionStatus: status,
    connectionError,
    accessDenied,
    canRetry,
    retry,
    refresh: fetchServers,
  };
}
