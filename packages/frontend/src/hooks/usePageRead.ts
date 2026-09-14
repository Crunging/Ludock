import { useCallback, useEffect, useRef, useState } from "react";

interface PageRead<T> {
  data: T | null;
  loading: boolean;
  error: string | null;
}

/** A page read with no retained snapshot while loading or after failure.
 * Keep read stable; a new reader replaces the current request. */
export function usePageRead<T>(read: (signal: AbortSignal) => Promise<T>, fallbackError: string) {
  const [result, setResult] = useState<PageRead<T>>({ data: null, loading: true, error: null });
  const request = useRef<AbortController | null>(null);
  const active = useRef(false);

  const refresh = useCallback(async () => {
    if (!active.current) return;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    const ownsRequest = () => active.current && request.current === controller && !controller.signal.aborted;
    setResult({ data: null, loading: true, error: null });
    try {
      const data = await read(controller.signal);
      if (ownsRequest()) setResult({ data, loading: false, error: null });
    } catch (reason) {
      if (ownsRequest()) setResult({
        data: null,
        loading: false,
        error: reason instanceof Error ? reason.message : fallbackError,
      });
    } finally {
      // A grouped read may fail while another request is still pending.
      controller.abort();
      if (request.current === controller) request.current = null;
    }
  }, [read, fallbackError]);

  useEffect(() => {
    active.current = true;
    void refresh();
    return () => {
      active.current = false;
      request.current?.abort();
      request.current = null;
    };
  }, [refresh]);

  return { ...result, refresh };
}
