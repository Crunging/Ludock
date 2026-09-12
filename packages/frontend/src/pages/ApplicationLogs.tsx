import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { ApiRequestError, apiJson } from "../api";
import "./application-logs.css";

import {
  type ApplicationLogEntry,
  applicationLogLevelSchema,
  applicationLogsResponseSchema,
} from "@ludock/shared";

const MAX_VISIBLE_ENTRIES = 1_000;

export default function ApplicationLogs() {
  const [entries, setEntries] = useState<ApplicationLogEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [followLatest, setFollowLatest] = useState(true);
  const [query, setQuery] = useState("");
  const [level, setLevel] = useState("");
  const [component, setComponent] = useState("");
  const [loading, setLoading] = useState(true);
  const generation = useRef<string | null>(null);
  const lastId = useRef(0);
  const request = useRef<AbortController | null>(null);
  const logOutput = useRef<HTMLDivElement>(null);
  const following = useRef(true);
  const scrollAnchor = useRef<{ row: HTMLElement; offset: number } | null>(null);
  const components = useMemo(() => {
    const values = new Set(entries.map((entry) => entry.component));
    // A selected component remains available when its entries leave the buffer.
    if (component) values.add(component);
    return [...values].sort((a, b) => a.localeCompare(b));
  }, [entries, component]);
  const matchingEntries = useMemo(() => {
    const search = query.trim().toLowerCase();
    return entries.filter((entry) =>
      (!level || entry.level === level) &&
      (!component || entry.component === component) &&
      (!search || [entry.message, entry.component, entry.level,
        entry.context ? JSON.stringify(entry.context) : "",
      ].join(" ").toLowerCase().includes(search)),
    );
  }, [entries, query, level, component]);
  const hasFilters = Boolean(query || level || component);

  function clearFilters() {
    setQuery("");
    setLevel("");
    setComponent("");
  }

  useEffect(() => () => {
    request.current?.abort();
    request.current = null;
  }, []);

  const loadLogs = useCallback(async () => {
    if (request.current) return;
    const controller = new AbortController();
    request.current = controller;
    setLoading(true);
    const initial = generation.current === null;
    try {
      const after = initial ? 0 : lastId.current;
      const processGeneration = generation.current
        ? `&generation=${encodeURIComponent(generation.current)}`
        : "";
      const body = await apiJson(
        `/application-logs?limit=${initial ? 250 : 1000}&after=${after}${processGeneration}`,
        applicationLogsResponseSchema,
        { signal: controller.signal },
      );
      if (controller.signal.aborted || request.current !== controller) return;

      const processRestarted =
        generation.current !== null && generation.current !== body.generation;
      scrollAnchor.current = null;
      const output = logOutput.current;
      if (!following.current && !initial && !processRestarted && body.entries.length > 0 && output) {
        const viewport = output.getBoundingClientRect();
        const row = [...output.querySelectorAll<HTMLElement>(".application-log")].find((candidate) => {
          const bounds = candidate.getBoundingClientRect();
          return bounds.bottom > viewport.top && bounds.top < viewport.bottom;
        });
        if (row) scrollAnchor.current = { row, offset: row.getBoundingClientRect().top - viewport.top };
      }
      generation.current = body.generation;
      if (processRestarted) {
        lastId.current = 0;
        setEntries([]);
      }
      if (body.entries.length > 0) {
        lastId.current = body.entries[body.entries.length - 1].id;
        setEntries((current) =>
          (initial || processRestarted
            ? body.entries
            : [...current, ...body.entries]
          ).slice(-MAX_VISIBLE_ENTRIES),
        );
      }
      setError(null);
    } catch (reason: unknown) {
      if (controller.signal.aborted || request.current !== controller) return;
      if (reason instanceof ApiRequestError && reason.status < 500) {
        generation.current = null;
        lastId.current = 0;
        setEntries([]);
        setComponent("");
      }
      setError(
        reason instanceof Error
          ? reason.message
          : "Failed to load application logs",
      );
    } finally {
      if (request.current === controller && !controller.signal.aborted) {
        request.current = null;
        setLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    if (paused) {
      setLoading(false);
      return;
    }
    void loadLogs();
    const interval = window.setInterval(() => void loadLogs(), 2_000);
    return () => {
      window.clearInterval(interval);
      request.current?.abort();
      request.current = null;
    };
  }, [loadLogs, paused]);

  useLayoutEffect(() => {
    following.current = followLatest;
    const output = logOutput.current;
    const anchor = scrollAnchor.current;
    scrollAnchor.current = null;
    if (!output) return;
    if (followLatest) {
      output.scrollTop = output.scrollHeight;
    } else if (anchor && output.contains(anchor.row)) {
      // Evicting older rows changes the offset of the entry being read. Restore
      // its position before paint while it remains in the bounded buffer.
      output.scrollTop += anchor.row.getBoundingClientRect().top - output.getBoundingClientRect().top - anchor.offset;
    }
  }, [matchingEntries, followLatest]);

  return (
    <div className="page application-logs-page">
      <div className="application-logs-header">
        <div>
          <h1 className="page__title">Ludock logs</h1>
          <p className="page__subtitle">
            Recent structured output from this Ludock process.
          </p>
        </div>
        <div className="application-logs-actions">
          <label className="application-logs-follow">
            <input
              type="checkbox"
              checked={followLatest}
              onChange={(event) => setFollowLatest(event.target.checked)}
            />
            Follow latest
          </label>
          <button
            className="secondary-btn"
            type="button"
            aria-pressed={paused}
            onClick={() => setPaused((value) => !value)}
          >
            {paused ? "Resume" : "Pause"}
          </button>
          <button
            className="secondary-btn"
            type="button"
            disabled={loading}
            onClick={() => void loadLogs()}
          >
            Refresh
          </button>
        </div>
      </div>

      <div className="application-logs-filters" role="group" aria-label="Log filters">
        <label className="application-logs-filter application-logs-filter--search">
          Search logs
          <input
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Message or context"
            aria-describedby="application-logs-scope"
          />
        </label>
        <label className="application-logs-filter">
          Severity
          <select value={level} onChange={(event) => setLevel(event.target.value)}>
            <option value="">All severities</option>
            {applicationLogLevelSchema.options.map((value) => (
              <option key={value} value={value}>{value}</option>
            ))}
          </select>
        </label>
        <label className="application-logs-filter">
          Component
          <select value={component} onChange={(event) => setComponent(event.target.value)}>
            <option value="">All components</option>
            {components.map((value) => <option key={value} value={value}>{value}</option>)}
          </select>
        </label>
        <button className="secondary-btn" type="button" disabled={!hasFilters} onClick={clearFilters}>
          Clear filters
        </button>
      </div>
      <div className="application-logs-summary">
        <span className="application-logs-count">
          {matchingEntries.length} of {entries.length} recent entries
        </span>
        <span id="application-logs-scope">
          Searches up to {MAX_VISIBLE_ENTRIES.toLocaleString()} recent entries buffered in this page.
          {paused ? " Fetching paused." : " Fetching every 2 seconds."}
        </span>
      </div>
      {error && <div className="alert alert--error" role="alert">{error}</div>}
      <div
        className="application-logs-output"
        ref={logOutput}
        role="log"
        tabIndex={0}
        aria-busy={loading}
        aria-live={paused || !followLatest ? "off" : "polite"}
        aria-label="Ludock application logs"
      >
        {matchingEntries.map((entry) => (
          <div
            className={`application-log application-log--${entry.level}`}
            key={entry.id}
          >
            <time dateTime={new Date(entry.timestamp).toISOString()}>
              {new Date(entry.timestamp).toLocaleString()}
            </time>
            <span className="application-log__level">{entry.level}</span>
            <span className="application-log__component">
              {entry.component}
            </span>
            <pre>
              {entry.message}
              {entry.context ? ` ${JSON.stringify(entry.context)}` : ""}
            </pre>
          </div>
        ))}
        {!loading && !error && entries.length === 0 && (
          <div className="application-logs-empty">No log entries yet.</div>
        )}
        {!error && entries.length > 0 && matchingEntries.length === 0 && (
          <div className="application-logs-empty">No recent log entries match these filters.</div>
        )}
        {loading && entries.length === 0 && (
          <div className="application-logs-empty">Loading logs…</div>
        )}
      </div>
    </div>
  );
}
