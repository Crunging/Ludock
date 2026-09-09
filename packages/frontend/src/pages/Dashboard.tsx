import { okResponseSchema } from "@ludock/shared";
import { useCallback, useState } from "react";
import { useServers } from "../hooks/useServers";
import ServerCard from "../components/ServerCard";
import { apiJson } from "../api";
import { useAuth } from "../auth-context";
import { useViewPreferences } from "../view-preferences-context";

export default function Dashboard() {
  const {
    servers, loading, error, refresh, stale, lastUpdated,
    connectionStatus, connectionError, canRetry, retry, accessDenied,
  } = useServers();
  const { user } = useAuth();
  const [actionError, setActionError] = useState<string | null>(null);
  const { dashboardFilters, setDashboardFilters } = useViewPreferences();
  const { search, stateFilter } = dashboardFilters;
  const setSearch = (search: string) => setDashboardFilters((current) => ({ ...current, search }));
  const setStateFilter = (stateFilter: string) => setDashboardFilters((current) => ({ ...current, stateFilter }));
  const [showHelp, setShowHelp] = useState(false);
  const handleAction = useCallback(
    async (id: string, action: "start" | "stop" | "restart") => {
      if (stale || loading) return;
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
    [refresh, stale, loading],
  );
  const filtered = servers.filter((server) =>
    (stateFilter === "all" || server.state === stateFilter) &&
    `${server.displayName} ${server.gameType} ${server.image}`
      .toLowerCase()
      .includes(search.trim().toLowerCase()),
  );
  // Keep a selected state available when a refresh changes the last matching
  // server, so the empty result remains understandable and easy to clear.
  const states = [...new Set([
    ...servers.map((server) => server.state),
    ...(stateFilter === "all" ? [] : [stateFilter]),
  ])].sort();

  return (
    <div className="page dashboard-page">
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
      {stale && (lastUpdated !== null || !loading) && (
        <div className="server-connection" role="status">
          <div>
            <p>
              {connectionStatus === "connected"
                ? "Server state needs to be refreshed."
                : connectionError || (connectionStatus === "connecting"
                  ? "Connecting to live updates…"
                  : "Live updates are disconnected.")}
            </p>
            {lastUpdated !== null && (
              <p className="muted">
                Showing the last known state. Server controls resume after a fresh connection and refresh.
              </p>
            )}
          </div>
          {canRetry && connectionStatus !== "connected" && (
            <button className="secondary-btn" onClick={retry}>Retry connection</button>
          )}
          {accessDenied && (
            <button className="secondary-btn" onClick={() => window.location.reload()}>Reload page</button>
          )}
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
          <label className="state-filter">
            <span>State</span>
            <select
              value={stateFilter}
              onChange={(event) => setStateFilter(event.target.value)}
            >
              <option value="all">All states</option>
              {states.map((state) => (
                <option key={state} value={state}>
                  {state.charAt(0).toUpperCase() + state.slice(1)}
                </option>
              ))}
            </select>
          </label>
          <span className="list-toolbar__count muted" role="status">
            {filtered.length} of {servers.length} servers
          </span>
        </div>
      )}
      {loading && servers.length === 0 && (
        <p className="muted" role="status">
          Loading servers…
        </p>
      )}
      {!loading && !error && !accessDenied && servers.length === 0 && (
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
            <span>Server</span>
            <span>State</span>
            <span>Ports</span>
            <span>Actions</span>
          </div>
          {filtered.map((server) => (
            <ServerCard
              key={server.id}
              server={server}
              actionsDisabled={stale || loading}
              onAction={handleAction}
            />
          ))}
          {filtered.length === 0 && (
            <div className="server-list__empty">
              <p>No servers match your filters.</p>
              <button
                className="text-link"
                onClick={() => { setSearch(""); setStateFilter("all"); }}
              >
                Clear filters
              </button>
            </div>
          )}
        </section>
      )}
    </div>
  );
}
