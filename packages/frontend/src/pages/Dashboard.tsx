import { okResponseSchema } from "@ludock/shared";
import { useCallback, useState } from "react";
import { useServers } from "../hooks/useServers";
import ServerCard from "../components/ServerCard";
import NeedsAttention from "../components/NeedsAttention";
import { apiJson } from "../api";
import { useAuth } from "../auth-context";
import { useViewPreferences } from "../view-preferences-context";
import { NavLink } from "../navigation";
import { canReadBackupSummary } from "../permissions";
import { serverStatus } from "../server-lifecycle";
import "./dashboard.css";

function DiscoveryHelp() {
  return (
    <section className="help-panel" id="server-discovery-help" aria-labelledby="server-discovery-title">
      <h2 id="server-discovery-title">Add a missing server</h2>
      <p>
        Ludock lists game containers on its Docker host. Images from
        {" "}<NavLink to="/diagnostics" className="text-link">supported games</NavLink>{" "}
        appear automatically. For any other image, add this label in the
        manager that owns the container, then recreate it:
      </p>
      <pre>{'labels:\n  ludock.enable: "true"'}</pre>
      <p className="muted">
        New servers are visible only to administrators until you
        {" "}<NavLink to="/users" className="text-link">share them</NavLink>.
      </p>
    </section>
  );
}

export default function Dashboard() {
  const {
    servers, loading, error, refresh, stale, lastUpdated,
    connectionStatus, connectionError, canRetry, retry, accessDenied,
  } = useServers();
  const { user } = useAuth();
  const admin = user?.role === "admin";
  const [actionError, setActionError] = useState<string | null>(null);
  const { dashboardFilters, setDashboardFilters } = useViewPreferences();
  const { search, stateFilter } = dashboardFilters;
  const setSearch = (search: string) => setDashboardFilters((current) => ({ ...current, search }));
  const setStateFilter = (stateFilter: string) => setDashboardFilters((current) => ({ ...current, stateFilter }));
  const [showHelp, setShowHelp] = useState(false);
  const [attentionRefresh, setAttentionRefresh] = useState(0);
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
    `${server.displayName} ${server.gameName} ${server.gameType} ${server.image}`
      .toLowerCase()
      .includes(search.trim().toLowerCase()),
  );
  // Keep a selected state available when a refresh changes the last matching
  // server, so the empty result remains understandable and easy to clear.
  const states = [...new Set([
    ...servers.map((server) => server.state),
    ...(stateFilter === "all" ? [] : [stateFilter]),
  ])].sort();
  const showBackup = servers.some((server) => canReadBackupSummary(user, server));

  return (
    <div className="page dashboard-page">
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">Servers</h1>
          {!admin && <p className="page__subtitle">Servers shared with your account.</p>}
        </div>
        <button
          className="secondary-btn"
          onClick={() => { setAttentionRefresh((current) => current + 1); void refresh(); }}
          disabled={loading}
        >
          {loading ? "Refreshing…" : "Refresh"}
        </button>
      </div>
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
          <div>
            <p>Unable to load servers: {error}</p>
            {admin && (
              <NavLink to="/diagnostics" className="text-link">Open diagnostics</NavLink>
            )}
          </div>
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
      {!accessDenied && user && (
        <NeedsAttention key={`${user.id}:${user.role}`} refreshKey={`${lastUpdated}:${attentionRefresh}`} />
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
                  {serverStatus({ state, health: null }).label}
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
            {admin ? "No game servers found" : "No servers shared with you yet"}
          </h2>
          <p className="empty-state__description">
            {admin
              ? "Ludock looks for game containers on the Docker host it’s connected to."
              : `You’re signed in as ${user?.username || "a limited user"}. Ask an administrator to share the servers you need.`}
          </p>
          {admin && (
            <>
              <NavLink to="/diagnostics" className="secondary-btn dashboard-empty-action">
                Check the Docker connection
              </NavLink>
              <DiscoveryHelp />
            </>
          )}
        </div>
      )}
      {servers.length > 0 && (
        <section
          className={`server-list${showBackup ? " server-list--backups" : ""}`}
          aria-label="Game servers"
        >
          <div className="server-list__header">
            <span>Server</span>
            <span>State</span>
            <span>Connect</span>
            {showBackup && <span>Last backup</span>}
            <span><span className="sr-only">Actions</span></span>
          </div>
          {filtered.map((server) => (
            <ServerCard
              key={server.id}
              server={server}
              showBackup={showBackup}
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
      {admin && servers.length > 0 && (
        <div className="dashboard-help">
          <button
            className="text-link"
            onClick={() => setShowHelp(!showHelp)}
            aria-expanded={showHelp}
            aria-controls="server-discovery-help"
          >
            Missing a server?
          </button>
          {showHelp && <DiscoveryHelp />}
        </div>
      )}
    </div>
  );
}
