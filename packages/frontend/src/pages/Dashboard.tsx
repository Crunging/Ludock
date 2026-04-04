import { useCallback } from "react";
import { useServers } from "../hooks/useServers";
import ServerCard from "../components/ServerCard";

export default function Dashboard() {
  const { servers, loading, error, refresh } = useServers();

  const handleAction = useCallback(
    async (id: string, action: "start" | "stop" | "restart") => {
      try {
        const res = await fetch(`/api/servers/${id}/${action}`, {
          method: "POST",
        });
        if (!res.ok) {
          const data = await res.json().catch(() => ({}));
          throw new Error(data.error || `Action failed (HTTP ${res.status})`);
        }
        // The Docker events WebSocket will trigger a refresh
      } catch (err: any) {
        console.error(`Failed to ${action} container:`, err);
        // Still refresh to get latest state
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
            : "Monitoring containers with game-panel.enable=true"}
        </p>
      </div>

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
            No Docker containers with the <strong>game-panel.enable=true</strong> label
            were found. Add this label to your game server containers to manage them here.
          </div>
          <div className="empty-state__code">
            <span>docker run</span> -l <span>game-panel.enable=true</span> \{"\n"}
            {"  "}-l <span>game-panel.name=</span>"My Server" \{"\n"}
            {"  "}-l <span>game-panel.game=</span>"minecraft" \{"\n"}
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
