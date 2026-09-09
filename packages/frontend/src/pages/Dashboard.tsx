import { okResponseSchema } from "@ludock/shared";
import { useCallback, useState } from "react";
import { useServers } from "../hooks/useServers";
import ServerCard from "../components/ServerCard";
import { apiJson } from "../api";
import { useAuth } from "../auth-context";

export default function Dashboard() {
  const { servers, loading, error, refresh } = useServers();
  const { user } = useAuth();
  const [actionError, setActionError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [showHelp, setShowHelp] = useState(false);
  const handleAction = useCallback(
    async (id: string, action: "start" | "stop" | "restart") => {
      try {
        setActionError(null);
        await apiJson(
          `/servers/${encodeURIComponent(id)}/${action}`,
          okResponseSchema,
          { method: "POST" },
        );
        refresh();
      } catch (reason) {
        setActionError(
          reason instanceof Error
            ? reason.message
            : `Failed to ${action} server`,
        );
        refresh();
      }
    },
    [refresh],
  );
  const filtered = servers.filter((server) =>
    `${server.displayName} ${server.gameType} ${server.image}`
      .toLowerCase()
      .includes(search.toLowerCase()),
  );

  return (
    <div className="page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Servers</h1>
          <p className="page__subtitle">
            {user?.role === "admin"
              ? "Recognized game servers are discovered automatically."
              : "Servers shared with your account."}
          </p>
        </div>
        <div className="inline-actions">
          <button
            className="secondary-btn"
            onClick={() => refresh()}
            disabled={loading}
          >
            Refresh
          </button>
          {user?.role === "admin" && (
            <button
              className="secondary-btn"
              onClick={() => setShowHelp(!showHelp)}
              aria-expanded={showHelp}
            >
              Game not shown?
            </button>
          )}
        </div>
      </div>
      {showHelp && (
        <section className="help-panel">
          <h2>Include an unrecognized image</h2>
          <p>
            Add this label through the manager that owns the container, then
            recreate it:
          </p>
          <pre>{'labels:\n  ludock.enable: "true"'}</pre>
          <p>
            Recognized images appear automatically. A{" "}
            <code>ludock.enable: "false"</code> label excludes a container.
            Invalid values exclude a container. Compose one-off containers need
            an explicit true label. Newly discovered servers are visible to
            administrators; assign other users access on the Users page.
          </p>
        </section>
      )}
      {actionError && (
        <div className="alert alert--error" role="alert">
          <span>{actionError}</span>
          <button
            onClick={() => setActionError(null)}
            aria-label="Dismiss error"
          >
            ×
          </button>
        </div>
      )}
      {error && (
        <div className="alert alert--error" role="alert">
          Unable to load servers: {error}
        </div>
      )}
      {servers.length > 0 && (
        <div className="list-toolbar">
          <label className="search-field">
            <span className="sr-only">Find a server</span>
            <input
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="Find a server…"
              type="search"
            />
          </label>
          <span className="muted">
            {filtered.length} of {servers.length}
          </span>
        </div>
      )}
      {loading && servers.length === 0 && (
        <p className="muted" role="status">
          Loading servers…
        </p>
      )}
      {!loading && !error && servers.length === 0 && (
        <div className="empty-state">
          <h2 className="empty-state__title">
            {user?.role === "admin"
              ? "No game servers found"
              : "No servers assigned"}
          </h2>
          <p className="empty-state__description">
            {user?.role === "admin"
              ? "Run a recognized game image, or use “Game not shown?” to include another image."
              : "Ask an administrator to share the servers and actions you need."}
          </p>
        </div>
      )}
      {servers.length > 0 && (
        <section className="server-list" aria-label="Game servers">
          <div className="server-list__header">
            <span>Server / image</span>
            <span>State</span>
            <span>Ports</span>
            <span>Actions</span>
          </div>
          {filtered.map((server) => (
            <ServerCard
              key={server.id}
              server={server}
              onAction={handleAction}
            />
          ))}
          {filtered.length === 0 && (
            <p className="table-empty">No servers match “{search}”.</p>
          )}
        </section>
      )}
    </div>
  );
}
