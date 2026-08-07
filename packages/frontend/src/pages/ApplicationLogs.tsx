import { useCallback, useEffect, useRef, useState } from "react";
import { apiFetch } from "../api";

type LogLevel = "debug" | "info" | "warn" | "error";

interface LogEntry {
  id: number;
  timestamp: number;
  level: LogLevel;
  component: string;
  message: string;
  context?: Record<string, string | number | boolean | null>;
}

const MAX_VISIBLE_ENTRIES = 1_000;

export default function ApplicationLogs() {
  const [entries, setEntries] = useState<LogEntry[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [paused, setPaused] = useState(false);
  const [loading, setLoading] = useState(true);
  const generation = useRef<string | null>(null);
  const lastId = useRef(0);
  const requestInFlight = useRef(false);
  const logOutput = useRef<HTMLDivElement>(null);

  const loadLogs = useCallback(async (initial = false) => {
    if (requestInFlight.current) return;
    requestInFlight.current = true;
    try {
      const after = initial ? 0 : lastId.current;
      const processGeneration = generation.current
        ? `&generation=${encodeURIComponent(generation.current)}`
        : "";
      const response = await apiFetch(
        `/api/application-logs?limit=${initial ? 250 : 1000}&after=${after}${processGeneration}`
      );
      const body = (await response.json().catch(() => ({}))) as {
        generation?: string;
        entries?: LogEntry[];
        error?: string;
      };
      if (!response.ok || !body.generation || !body.entries) {
        throw new Error(body.error || "Failed to load application logs");
      }

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
            ? body.entries!
            : [...current, ...body.entries!]
          ).slice(-MAX_VISIBLE_ENTRIES)
        );
      }
      setError(null);
    } catch (reason: unknown) {
      setError(
        reason instanceof Error
          ? reason.message
          : "Failed to load application logs"
      );
    } finally {
      requestInFlight.current = false;
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadLogs(true);
  }, [loadLogs]);

  useEffect(() => {
    if (paused) return;
    const interval = window.setInterval(() => void loadLogs(), 2_000);
    return () => window.clearInterval(interval);
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
            onClick={() => setPaused((value) => !value)}
          >
            {paused ? "Resume" : "Pause"}
          </button>
          <button
            className="secondary-btn"
            type="button"
            onClick={() => void loadLogs()}
          >
            Refresh
          </button>
        </div>
      </div>

      {error && <div className="alert alert--error">{error}</div>}
      <div
        className="application-logs-output"
        ref={logOutput}
        role="log"
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
            <span className="application-log__component">{entry.component}</span>
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
