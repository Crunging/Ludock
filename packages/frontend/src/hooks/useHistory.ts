import type { ResponseSchema } from "@ludock/shared";
import { useCallback, useMemo, useRef } from "react";
import { apiJson } from "../api";
import { useLocation, useNavigate } from "../navigation-context";
import { usePageRead } from "./usePageRead";

const filterNames = ["serverId", "actor", "action", "status", "from", "to", "operationId"];

/** Applied filters and the page cursor travel in the URL. Drafts belong to the
 * form; switching pages or refreshing must not submit those drafts. */
export function useHistory<T>(path: string, schema: ResponseSchema<T>, fallback: string) {
  const { search } = useLocation();
  const navigate = useNavigate();
  const previousPages = useRef(new Map<string, string>());
  const query = useMemo(() => {
    const source = new URLSearchParams(search);
    const params = new URLSearchParams({ limit: "50" });
    for (const name of [...filterNames, "cursor"]) {
      const value = source.get(name);
      if (value && (name !== "operationId" || path === "/audit")) params.set(name, value);
    }
    return params.toString();
  }, [path, search]);
  const read = useCallback((signal: AbortSignal) => apiJson(`${path}?${query}`, schema, { signal }), [path, query, schema]);
  const page = usePageRead(read, fallback);
  const filters = new URLSearchParams(query);
  filters.delete("cursor");
  filters.delete("limit");
  const filterKey = filters.toString();
  const cursor = new URLSearchParams(query).get("cursor");

  function showQuery(next: string) {
    if (next === query) void page.refresh();
    else navigate(`${path}?${next}`);
  }
  function apply(next: URLSearchParams) {
    const params = new URLSearchParams({ limit: "50" });
    for (const name of filterNames) {
      const value = next.get(name);
      if (value) params.set(name, value);
    }
    showQuery(params.toString());
  }
  function older(nextCursor: string) {
    const params = new URLSearchParams(query);
    params.set("cursor", nextCursor);
    const next = params.toString();
    previousPages.current.set(next, query);
    showQuery(next);
  }
  function newer() {
    const previous = previousPages.current.get(query);
    if (previous) showQuery(previous);
    else apply(filters);
  }
  return { ...page, filters, filterKey, apply, older, newer, hasCursor: Boolean(cursor) };
}
