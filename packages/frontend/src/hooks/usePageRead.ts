import { useCallback, useEffect, useRef, useState } from "react";

type Reader<T> = ((signal: AbortSignal) => Promise<T>) | null;
interface PageRead<T> {
  source: Reader<T>;
  data: T | null;
  loading: boolean;
  error: string | null;
}

/** A null reader disables the read. Changing readers hides old data immediately.
 * Polling panels may retain their last snapshot while refreshing; errors clear it. */
export function usePageRead<T>(
  read: Reader<T>,
  fallbackError: string,
  { retainWhileRefreshing = false }: { retainWhileRefreshing?: boolean } = {},
) {
  const [result, setResult] = useState<PageRead<T>>({
    source: read,
    data: null,
    loading: Boolean(read),
    error: null,
  });
  const request = useRef<AbortController | null>(null);
  const activeReader = useRef<Reader<T>>(null);

  const refresh = useCallback(
    async (replacePending = true) => {
      if (activeReader.current !== read || !read || (!replacePending && request.current))
        return;
      request.current?.abort();
      const controller = new AbortController();
      request.current = controller;
      const ownsRequest = () =>
        activeReader.current === read &&
        request.current === controller &&
        !controller.signal.aborted;
      setResult((previous) => ({
        source: read,
        data:
          retainWhileRefreshing && previous.source === read
            ? previous.data
            : null,
        loading: true,
        error: null,
      }));
      try {
        const data = await read(controller.signal);
        if (ownsRequest())
          setResult({ source: read, data, loading: false, error: null });
      } catch (reason) {
        if (ownsRequest())
          setResult({
            source: read,
            data: null,
            loading: false,
            error: reason instanceof Error ? reason.message : fallbackError,
          });
      } finally {
        controller.abort();
        if (request.current === controller) request.current = null;
      }
    },
    [read, fallbackError, retainWhileRefreshing],
  );

  useEffect(() => {
    activeReader.current = read;
    if (read) void refresh();
    else setResult({ source: null, data: null, loading: false, error: null });
    return () => {
      activeReader.current = null;
      request.current?.abort();
      request.current = null;
    };
  }, [read, refresh]);

  const current =
    result.source === read
      ? result
      : { data: null, loading: Boolean(read), error: null };
  return {
    data: current.data,
    loading: current.loading,
    error: current.error,
    refresh,
  };
}
