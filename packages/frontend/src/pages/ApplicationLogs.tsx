import { useCallback, useEffect, useRef, useState } from "react";
import { ApiRequestError, apiJson } from "../api";

import {
  type ApplicationLogEntry,
  applicationLogsResponseSchema,
} from "@ludock/shared";

const MAX_VISIBLE_ENTRIES = 1_000;

export default function ApplicationLogs() {
  const [entries, setEntries] = useState<ApplicationLogEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [loading, setLoading] = useState(true);
  const generation = useRef<string | null>(null);
  const lastId = useRef(0);
  const request = useRef<AbortController | null>(null);
  const logOutput = useRef<HTMLDivElement>(null);

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

  useEffect(() => {
    if (!paused && logOutput.current) {
      logOutput.current.scrollTop = logOutput.current.scrollHeight;
    }
  }, [entries, paused]);

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
          <span className="application-logs-count">
            {entries.length} entr{entries.length === 1 ? "y" : "ies"}
          </span>
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

      {error && <div className="alert alert--error" role="alert">{error}</div>}
      <div
        className="application-logs-output"
        ref={logOutput}
        role="log"
        aria-busy={loading}
        aria-live={paused ? "off" : "polite"}
        aria-label="Ludock application logs"
      >
        {entries.map((entry) => (
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
        {loading && entries.length === 0 && (
          <div className="application-logs-empty">Loading logs…</div>
        )}
      </div>
    </div>
  );
}
