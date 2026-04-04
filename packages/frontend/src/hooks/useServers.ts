import { useCallback, useEffect, useState } from "react";
import type { ManagedContainer, ContainerEvent } from "../types";
import { useWebSocket } from "./useWebSocket";

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
      const res = await fetch("/api/servers");
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      setServers(data.servers);
    } catch (err: any) {
      setError(err.message || "Failed to fetch servers");
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
      } catch {}
    },
    [fetchServers]
  );

  const wsProtocol = window.location.protocol === "https:" ? "wss:" : "ws:";
  const wsUrl = `${wsProtocol}//${window.location.host}/ws/events`;

  useWebSocket({
    url: wsUrl,
    onMessage: handleEvent,
  });

  useEffect(() => {
    fetchServers();
  }, [fetchServers]);

  return { servers, loading, error, refresh: fetchServers };
}
