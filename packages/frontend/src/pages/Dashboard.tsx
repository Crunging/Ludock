import { useCallback, useState } from "react";
import { useServers } from "../hooks/useServers";
import ServerCard from "../components/ServerCard";
import { apiFetch } from "../api";

export default function Dashboard() {
  const { servers, loading, error, refresh } = useServers();
  const [actionError, setActionError] = useState<string | null>(null);

  const handleAction = useCallback(
    async (id: string, action: "start" | "stop" | "restart") => {
      try {
        setActionError(null);
        const response = await apiFetch(`/api/servers/${id}/${action}`, {
          method: "POST",
        });
        if (!response.ok) {
          const body = await response.json().catch(() => ({}));
          throw new Error(
            body.error || `Action failed (HTTP ${response.status})`
          );
        }
        refresh();
      } catch (error: unknown) {
        setActionError(
          error instanceof Error ? error.message : `Failed to ${action} server`
        );
        refresh();
      }
    },
    [refresh]
  );

  return (
    <div className="page">
      <div className="page__header">
        <h1 className="page__title">Game Servers</h1>
        <p className="page__subtitle">
          {servers.length > 0
            ? `Managing ${servers.length} server${servers.length !== 1 ? "s" : ""} via Docker`
            : "Monitoring containers with ludock.enable=true"}
        </p>
      </div>

      {actionError && (
        <div className="alert alert--error" role="alert">
          <span>{actionError}</span>
          <button onClick={() => setActionError(null)} aria-label="Dismiss error">
            ×
          </button>
        </div>
      )}

      {loading && servers.length === 0 && (
        <div className="loading-spinner">
          <div className="loading-spinner__ring" />
        </div>
      )}

      {error && (
        <div className="empty-state">
          <div className="empty-state__icon">!</div>
          <div className="empty-state__title">Connection Error</div>
          <div className="empty-state__description">{error}</div>
        </div>
      )}

      {!loading && !error && servers.length === 0 && (
        <div className="empty-state">
          <div className="empty-state__icon">?</div>
          <div className="empty-state__title">No Servers Found</div>
          <div className="empty-state__description">
            No Docker containers with the{" "}
            <strong>ludock.enable=true</strong> label were found. Add this
            label to your game server containers to manage them here.
          </div>
          <div className="empty-state__code">
            <span>docker run</span> -l <span>ludock.enable=true</span> \{"\n"}
            {"  "}-l <span>ludock.name=</span>"My Server" \{"\n"}
            {"  "}-l <span>ludock.game=</span>"minecraft" \{"\n"}
            {"  "}your/game-image
          </div>
        </div>
      )}

      {servers.length > 0 && (
        <div className="server-grid">
          {servers.map((server) => (
            <ServerCard
              key={server.id}
              server={server}
              onAction={handleAction}
            />
          ))}
        </div>
      )}
    </div>
  );
}
