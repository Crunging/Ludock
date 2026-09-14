import { serversResponseSchema } from "@ludock/shared";
import { apiJson } from "../api";
import { usePageRead } from "./usePageRead";

const readServers = (signal: AbortSignal) => apiJson("/servers", serversResponseSchema, { signal });

/** Server names enrich history without making old records depend on discovery. */
export function useHistoryServers() {
  const result = usePageRead(readServers, "Unable to load server names");
  return { ...result, servers: result.data?.servers ?? [] };
}
