import { useEffect, useMemo, useState, type FormEvent } from "react";
import type { Server } from "@ludock/shared";
import { operationStatusLabels } from "../operations";

const fieldNames = ["serverId", "actor", "action", "status", "from", "to", "operationId"] as const;
type Field = (typeof fieldNames)[number];

function localDate(value: string | null): string {
  if (!value) return "";
  const date = new Date(Number(value));
  if (!Number.isFinite(date.getTime())) return "";
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, -1)
    .replace(/\.000$/, "").replace(/:00$/, "");
}

const dateStep = (value: string) => value.includes(".") ? "0.001" : value.length > 16 ? "1" : "60";

interface Props {
  filters: URLSearchParams;
  servers: Pick<Server, "id" | "displayName">[];
  audit?: boolean;
  onSearch: (filters: URLSearchParams) => void;
}

export default function HistoryFilters({ filters, servers, audit = false, onSearch }: Props) {
  const filterKey = filters.toString();
  const applied = useMemo(() => {
    const params = new URLSearchParams(filterKey);
    return Object.fromEntries(fieldNames.map((name) => [
      name, name === "from" || name === "to" ? localDate(params.get(name)) : params.get(name) ?? "",
    ])) as Record<Field, string>;
  }, [filterKey]);
  const [draft, setDraft] = useState(applied);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => { setDraft(applied); setError(null); }, [applied]);
  const update = (field: Field, value: string) => setDraft((current) => ({ ...current, [field]: value }));
  const hasFilters = fieldNames.some((field) => draft[field] || filters.get(field));
  const hasServer = servers.some((server) => server.id === draft.serverId);
  function search(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const params = new URLSearchParams();
    for (const name of fieldNames) {
      if (name === "operationId" && !audit) continue;
      const value = draft[name].trim();
      if (!value) continue;
      if (name === "from" || name === "to") {
        // A local time repeats when daylight saving time ends. Keep the exact
        // shared-link instant until the user changes this field.
        params.set(name, value === applied[name] && filters.has(name)
          ? filters.get(name)!
          : String(new Date(value).getTime()));
      } else params.set(name, value);
    }
    if (params.has("from") && params.has("to") && Number(params.get("from")) > Number(params.get("to"))) {
      setError("From must be before or equal to To.");
      return;
    }
    setError(null);
    onSearch(params);
  }
  return (
    <form className="history-search stack-form" aria-label={audit ? "Search audit log" : "Search operation history"} onSubmit={search}>
      <div className="history-filters">
        <label>
          <span>Server</span>
          <select value={draft.serverId} onChange={(event) => update("serverId", event.target.value)}>
            <option value="">All servers</option>
            {draft.serverId && !hasServer && <option value={draft.serverId}>{draft.serverId}</option>}
            {servers.map((server) => <option key={server.id} value={server.id}>{server.displayName}</option>)}
          </select>
        </label>
        <label>
          <span>Actor</span>
          <input type="search" value={draft.actor} maxLength={200} onChange={(event) => update("actor", event.target.value)} placeholder="Name or actor ID" />
        </label>
        <label>
          <span>Action</span>
          <input type="search" value={draft.action} maxLength={200} onChange={(event) => update("action", event.target.value)} placeholder={audit ? "e.g. server.restart" : "e.g. restart"} />
        </label>
        <label>
          <span>Status</span>
          <select value={draft.status} onChange={(event) => update("status", event.target.value)}>
            <option value="">All statuses</option>
            {draft.status && !(draft.status in operationStatusLabels) && <option value={draft.status}>{draft.status}</option>}
            {Object.entries(operationStatusLabels).map(([value, label]) => <option value={value} key={value}>{label}</option>)}
          </select>
        </label>
        <label>
          <span>From</span>
          <input type="datetime-local" step={dateStep(draft.from)} value={draft.from} onChange={(event) => update("from", event.target.value)} aria-describedby="history-filter-help" />
        </label>
        <label>
          <span>To</span>
          <input type="datetime-local" step={dateStep(draft.to)} value={draft.to} onChange={(event) => update("to", event.target.value)} aria-describedby="history-filter-help" />
        </label>
        {audit && <label>
          <span>Operation ID</span>
          <input value={draft.operationId} onChange={(event) => update("operationId", event.target.value)} placeholder="Operation UUID" maxLength={36} pattern="[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}" />
        </label>}
      </div>
      <p className="section-note" id="history-filter-help">Dates use your local time and include both endpoints. Actor and status filters match only recorded values.</p>
      {error && <p role="alert" className="alert alert--error">{error}</p>}
      <div className="inline-actions">
        <button className="primary-btn" type="submit">Search</button>
        <button className="secondary-btn" type="button" disabled={!hasFilters} onClick={() => {
          setDraft(Object.fromEntries(fieldNames.map((name) => [name, ""])) as Record<Field, string>);
          setError(null);
          onSearch(new URLSearchParams());
        }}>Clear filters</button>
      </div>
    </form>
  );
}
