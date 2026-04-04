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
  const retriesRef = useRef(0);
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");

  const connect = useCallback(() => {
    if (wsRef.current?.readyState === WebSocket.OPEN) return;

    setStatus("connecting");
    const ws = new WebSocket(url);
    wsRef.current = ws;

    ws.onopen = () => {
      setStatus("connected");
      retriesRef.current = 0;
    };

    ws.onmessage = (event) => {
      onMessage?.(event.data);
    };

    ws.onclose = () => {
      setStatus("disconnected");
      wsRef.current = null;

      if (reconnect && retriesRef.current < maxRetries) {
        retriesRef.current++;
        setTimeout(connect, reconnectDelay);
      }
    };

    ws.onerror = () => {};
  }, [url, onMessage, reconnect, reconnectDelay, maxRetries]);

  const send = useCallback((data: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(data);
    }
  }, []);

  const disconnect = useCallback(() => {
    retriesRef.current = maxRetries;
    wsRef.current?.close();
  }, [maxRetries]);

  useEffect(() => {
    connect();
    return () => {
      disconnect();
    };
  }, [connect, disconnect]);

  return { status, send, disconnect };
}
