import { useCallback, useEffect, useState } from "react";
import type { ManagedContainer, ContainerEvent } from "../types";
import { useWebSocket } from "./useWebSocket";
import { apiFetch, authenticatedWebSocketUrl } from "../api";

interface UseServersResult {
  servers: ManagedContainer[];
  loading: boolean;
  error: string | null;
  refresh: () => void;
}

export function useServers(): UseServersResult {
  const [servers, setServers] = useState<ManagedContainer[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const fetchServers = useCallback(async () => {
    try {
      setLoading(true);
      setError(null);
      const response = await apiFetch("/api/servers");
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const body = await response.json();
      setServers(body.servers);
    } catch (error: unknown) {
      setError(
        error instanceof Error ? error.message : "Failed to fetch servers"
      );
    } finally {
      setLoading(false);
    }
  }, []);

  const handleEvent = useCallback(
    (raw: string) => {
      try {
        const event: ContainerEvent = JSON.parse(raw);
        if (event.type === "container_event") {
          fetchServers();
        }
      } catch {
        return;
      }
    },
    [fetchServers]
  );

  const wsUrl = authenticatedWebSocketUrl("/ws/events");

  useWebSocket({
    url: wsUrl,
    onMessage: handleEvent,
  });

  useEffect(() => {
    fetchServers();
  }, [fetchServers]);

  return { servers, loading, error, refresh: fetchServers };
}
