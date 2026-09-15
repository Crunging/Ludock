import { attentionResponseSchema } from "@ludock/shared";
import { useEffect } from "react";
import { apiJson } from "../api";
import { usePageRead } from "./usePageRead";

const readAttention = (signal: AbortSignal) =>
  apiJson("/attention", attentionResponseSchema, { signal });

/** Keep links stable during routine reads. The owner unmounts this hook when
 * the account changes or its event stream reports denied access. */
export function useAttention() {
  const page = usePageRead(readAttention, "Unable to load attention items", {
    retainWhileRefreshing: true,
  });
  const { refresh } = page;

  useEffect(() => {
    // Schedule outcomes and operation failures do not always emit Docker events.
    const timer = window.setInterval(() => {
      void refresh(false);
    }, 30_000);
    return () => window.clearInterval(timer);
  }, [refresh]);
  return page;
}
