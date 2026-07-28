import { useState } from "react";
import type { ManagedContainer } from "../types";
import { getGameAbbreviation } from "../types";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";

interface ServerCardProps {
  server: ManagedContainer;
  onAction: (id: string, action: "start" | "stop" | "restart") => Promise<void>;
}

export default function ServerCard({ server, onAction }: ServerCardProps) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const isRunning = server.state === "running";
  const stateClass = isRunning ? "running" : "stopped";

  const handleAction = async (action: "start" | "stop" | "restart") => {
    setActionLoading(action);
    try {
      await onAction(server.id, action);
    } finally {
      setActionLoading(null);
    }
  };

  const activePorts = server.ports.filter((p) => p.public > 0);

  return (
    <div
      className={`server-card server-card--${stateClass}`}
      id={`server-card-${server.shortId}`}
    >
      <div className="server-card__header">
        <div className="server-card__info">
          <div className="server-card__icon">
            {getGameAbbreviation(server.gameType)}
          </div>
          <div>
            <div className="server-card__name">{server.displayName}</div>
            <div className="server-card__game">{server.gameType}</div>
          </div>
        </div>
        <div className={`status-badge status-badge--${server.state}`}>
          <span className="status-dot" />
          {server.state}
        </div>
      </div>

      {activePorts.length > 0 && (
        <div className="server-card__ports">
          {activePorts.map((port) => (
            <span
              key={`${port.public}:${port.private}/${port.type}`}
              className="port-tag"
            >
              {port.public}:{port.private}/{port.type}
            </span>
          ))}
        </div>
      )}

      <div className="server-card__actions">
        {user?.role !== "viewer" &&
          (isRunning ? (
            <>
              <button
                className="action-btn action-btn--stop"
                onClick={() => handleAction("stop")}
                disabled={actionLoading !== null}
                id={`btn-stop-${server.shortId}`}
              >
                {actionLoading === "stop" ? "..." : "Stop"}
              </button>
              <button
                className="action-btn action-btn--restart"
                onClick={() => handleAction("restart")}
                disabled={actionLoading !== null}
                id={`btn-restart-${server.shortId}`}
              >
                {actionLoading === "restart" ? "..." : "Restart"}
              </button>
            </>
          ) : (
            <button
              className="action-btn action-btn--start"
              onClick={() => handleAction("start")}
              disabled={actionLoading !== null}
              id={`btn-start-${server.shortId}`}
            >
              {actionLoading === "start" ? "..." : "Start"}
            </button>
          ))}
        <button
          className="action-btn action-btn--console"
          onClick={() => navigate(`/console/${server.id}`)}
          id={`btn-console-${server.shortId}`}
        >
          {server.gameConsole && user?.role !== "viewer"
            ? "Game Console"
            : "View Logs"}
        </button>
        {server.fileRoots.length > 0 && (
          <button
            className="action-btn action-btn--files"
            onClick={() => navigate(`/files/${server.id}`)}
            id={`btn-files-${server.shortId}`}
          >
            Files
          </button>
        )}
      </div>
    </div>
  );
}
