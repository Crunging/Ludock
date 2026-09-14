import { attentionResponseSchema, type AttentionResponse } from "@ludock/shared";
import { useCallback, useEffect, useRef, useState } from "react";
import { apiJson } from "../api";

/** Keep links stable during routine reads. The owner unmounts this hook when
 * the account changes or its event stream reports denied access. */
export function useAttention() {
  const [data, setData] = useState<AttentionResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const active = useRef(false);
  const request = useRef<AbortController | null>(null);
  const refresh = useCallback(async () => {
    if (!active.current) return;
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    const current = () => active.current && request.current === controller && !controller.signal.aborted;
    setLoading(true);
    setError(null);
    try {
      const response = await apiJson("/attention", attentionResponseSchema, { signal: controller.signal });
      if (current()) setData(response);
    } catch (reason) {
      if (current()) {
        // Neither denied access nor failed reads may leave a healthy-looking
        // empty result or historical issues presented as a current snapshot.
        setData(null);
        setError(reason instanceof Error ? reason.message : "Unable to load attention items");
      }
    } finally {
      if (current()) {
        setLoading(false);
        request.current = null;
      }
      controller.abort();
    }
  }, []);

  useEffect(() => {
    active.current = true;
    void refresh();
    // Schedule outcomes and operation failures do not always emit Docker events.
    const timer = window.setInterval(() => {
      if (!request.current) void refresh();
    }, 30_000);
    return () => {
      active.current = false;
      request.current?.abort();
      request.current = null;
      window.clearInterval(timer);
    };
  }, [refresh]);
  return { data, loading, error, refresh };
}
