import { useCallback, useEffect, useRef, useState } from "react";

export type ConnectionStatus = "connecting" | "connected" | "disconnected";

interface UseWebSocketOptions {
  url: string;
  onMessage?: (data: string) => void;
  reconnect?: boolean;
  reconnectDelay?: number;
  maxRetries?: number;
}

export function useWebSocket({
  url,
  onMessage,
  reconnect = true,
  reconnectDelay = 2000,
  maxRetries = 10,
}: UseWebSocketOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const onMessageRef = useRef(onMessage);
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");

  useEffect(() => {
    onMessageRef.current = onMessage;
  }, [onMessage]);

  const send = useCallback((data: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(data);
    }
  }, []);

  useEffect(() => {
    if (!url) {
      return;
    }

    let cancelled = false;
    let retries = 0;
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined;

    function connect() {
      if (cancelled) return;

      setStatus("connecting");
      const ws = new WebSocket(url);
      wsRef.current = ws;

      ws.onopen = () => {
        if (cancelled) {
          ws.close();
          return;
        }
        setStatus("connected");
        retries = 0;
      };

      ws.onmessage = (event) => {
        onMessageRef.current?.(event.data);
      };

      ws.onclose = () => {
        if (wsRef.current === ws) wsRef.current = null;
        if (cancelled) return;

        setStatus("disconnected");
        if (reconnect && retries < maxRetries) {
          retries += 1;
          reconnectTimer = setTimeout(connect, reconnectDelay);
        }
      };

      ws.onerror = () => {
        ws.close();
      };
    }

    connect();
    return () => {
      cancelled = true;
      if (reconnectTimer) clearTimeout(reconnectTimer);
      wsRef.current?.close();
      wsRef.current = null;
    };
  }, [url, reconnect, reconnectDelay, maxRetries]);

  return { status, send };
}
