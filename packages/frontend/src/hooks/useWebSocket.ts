import { useCallback, useEffect, useRef, useState } from "react";

export type ConnectionStatus = "connecting" | "connected" | "disconnected";

interface UseWebSocketOptions {
  url: string;
  onMessage?: (data: string) => void;
  onOpen?: () => void;
  reconnect?: boolean;
  reconnectDelay?: number;
  maxRetries?: number;
}

interface ConnectionState {
  url: string;
  status: ConnectionStatus;
  canRetry: boolean;
  error: string | null;
  accessDenied: boolean;
}

interface ConnectionControls {
  url: string;
  send: (data: string) => boolean;
  retry: () => void;
}

const ACCESS_DENIED_CODES = new Set([1008, 4001, 4003, 4401, 4403]);
const STABLE_CONNECTION_MS = 30_000;

export function useWebSocket({
  url,
  onMessage,
  onOpen,
  reconnect = true,
  reconnectDelay = 2000,
  maxRetries = 10,
}: UseWebSocketOptions) {
  const controlsRef = useRef<ConnectionControls | null>(null);
  const onMessageRef = useRef(onMessage);
  const onOpenRef = useRef(onOpen);
  const [connection, setConnection] = useState<ConnectionState>({
    url: "",
    status: "disconnected",
    canRetry: false,
    error: null,
    accessDenied: false,
  });

  useEffect(() => {
    onMessageRef.current = onMessage;
    onOpenRef.current = onOpen;
  }, [onMessage, onOpen]);

  const send = useCallback((data: string): boolean => {
    const controls = controlsRef.current;
    return controls?.url === url ? controls.send(data) : false;
  }, [url]);

  const retry = useCallback(() => {
    const controls = controlsRef.current;
    if (controls?.url === url) controls.retry();
  }, [url]);

  useEffect(() => {
    if (!url) {
      setConnection({ url, status: "disconnected", canRetry: false, error: null, accessDenied: false });
      return;
    }

    let cancelled = false;
    let socket: WebSocket | null = null;
    let retries = 0;
    let accessDenied = false;
    let retryAllowed = false;
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
    let stableTimer: ReturnType<typeof setTimeout> | undefined;

    const current = (candidate: WebSocket) => !cancelled && socket === candidate;
    const clearTimers = () => {
      clearTimeout(reconnectTimer);
      clearTimeout(stableTimer);
      reconnectTimer = undefined;
      stableTimer = undefined;
    };

    const disconnected = (code?: number) => {
      if (cancelled) return;
      clearTimers();
      accessDenied = code !== undefined && ACCESS_DENIED_CODES.has(code);
      if (accessDenied) {
        retryAllowed = false;
        setConnection({
          url,
          status: "disconnected",
          canRetry: false,
          accessDenied: true,
          error: "Connection access is unavailable. Reload server details to check your access.",
        });
        return;
      }

      if (reconnect && retries < Math.max(0, maxRetries)) {
        retries += 1;
        retryAllowed = false;
        setConnection({
          url,
          status: "disconnected",
          canRetry: false,
          accessDenied: false,
          error: "Connection lost. Reconnecting…",
        });
        reconnectTimer = setTimeout(connect, Math.max(0, reconnectDelay));
      } else {
        retryAllowed = true;
        setConnection({
          url,
          status: "disconnected",
          canRetry: true,
          accessDenied: false,
          error: reconnect
            ? "Connection lost. Automatic retries have stopped."
            : "Connection lost.",
        });
      }
    };

    function connect() {
      if (cancelled || accessDenied) return;
      clearTimers();
      retryAllowed = false;
      setConnection({ url, status: "connecting", canRetry: false, error: null, accessDenied: false });
      let ws: WebSocket;
      try {
        ws = new WebSocket(url);
      } catch {
        disconnected();
        return;
      }
      socket = ws;

      ws.onopen = () => {
        if (!current(ws)) return;
        setConnection({ url, status: "connected", canRetry: false, error: null, accessDenied: false });
        // A server which opens and immediately closes must still exhaust its
        // retry budget. Only a sustained connection starts a fresh budget.
        stableTimer = setTimeout(() => {
          if (current(ws)) retries = 0;
        }, STABLE_CONNECTION_MS);
        onOpenRef.current?.();
      };

      ws.onmessage = (event) => {
        if (current(ws) && typeof event.data === "string") {
          onMessageRef.current?.(event.data);
        }
      };

      ws.onclose = (event) => {
        if (!current(ws)) return;
        socket = null;
        disconnected(event.code);
      };

      ws.onerror = () => {
        if (!current(ws)) return;
        // Browsers follow an error with a close event. Keep the socket current
        // until then so an access-denial close code cannot become a blind retry.
        try {
          ws.close();
        } catch {
          socket = null;
          disconnected();
        }
      };
    }

    const controls: ConnectionControls = {
      url,
      send(data) {
        const ws = socket;
        if (!ws || !current(ws) || ws.readyState !== WebSocket.OPEN) return false;
        try {
          ws.send(data);
          // This acknowledges browser transport acceptance, never execution.
          return ws.readyState === WebSocket.OPEN;
        } catch {
          try {
            ws.close();
          } catch {
            if (current(ws)) {
              socket = null;
              disconnected();
            }
          }
          return false;
        }
      },
      retry() {
        if (cancelled || accessDenied || !retryAllowed) return;
        retries = 0;
        connect();
      },
    };
    controlsRef.current = controls;
    connect();

    return () => {
      cancelled = true;
      clearTimers();
      if (controlsRef.current === controls) controlsRef.current = null;
      const ws = socket;
      socket = null;
      if (ws) {
        ws.onopen = null;
        ws.onmessage = null;
        ws.onclose = null;
        ws.onerror = null;
        try { ws.close(); } catch { /* Already detached; cleanup cannot retry. */ }
      }
    };
  }, [url, reconnect, reconnectDelay, maxRetries]);

  // A URL change must never briefly expose the old connection as sendable.
  const currentConnection = connection.url === url ? connection : {
    url,
    status: url ? "connecting" as const : "disconnected" as const,
    canRetry: false,
    error: null,
    accessDenied: false,
  };
  const { status, canRetry, error, accessDenied } = currentConnection;
  return { status, canRetry, error, accessDenied, send, retry };
}
