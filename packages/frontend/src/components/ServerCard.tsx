import { useState } from "react";
import { useNavigate } from "react-router-dom";
import type { ManagedContainer } from "../types";
import { getGameIcon } from "../types";

interface ServerCardProps {
  server: ManagedContainer;
  onAction: (id: string, action: "start" | "stop" | "restart") => Promise<void>;
}

export default function ServerCard({ server, onAction }: ServerCardProps) {
  const navigate = useNavigate();
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const isRunning = server.state === "running";
  const stateClass = isRunning ? "running" : "stopped";

  const handleAction = async (
    e: React.MouseEvent,
    action: "start" | "stop" | "restart"
  ) => {
    e.stopPropagation();
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
      onClick={() => navigate(`/console/${server.id}`)}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter") navigate(`/console/${server.id}`);
      }}
      id={`server-card-${server.shortId}`}
    >
      <div className="server-card__header">
        <div className="server-card__info">
          <div className="server-card__icon">{getGameIcon(server.gameType)}</div>
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
          {activePorts.map((p, i) => (
            <span key={i} className="port-tag">
              {p.public}:{p.private}/{p.type}
            </span>
          ))}
        </div>
      )}

      <div className="server-card__actions">
        {isRunning ? (
          <>
            <button
              className="action-btn action-btn--stop"
              onClick={(e) => handleAction(e, "stop")}
              disabled={actionLoading !== null}
              id={`btn-stop-${server.shortId}`}
            >
              <span className="action-btn__icon">■</span>
              {actionLoading === "stop" ? "..." : "Stop"}
            </button>
            <button
              className="action-btn action-btn--restart"
              onClick={(e) => handleAction(e, "restart")}
              disabled={actionLoading !== null}
              id={`btn-restart-${server.shortId}`}
            >
              <span className="action-btn__icon">↻</span>
              {actionLoading === "restart" ? "..." : "Restart"}
            </button>
          </>
        ) : (
          <button
            className="action-btn action-btn--start"
            onClick={(e) => handleAction(e, "start")}
            disabled={actionLoading !== null}
            id={`btn-start-${server.shortId}`}
          >
            <span className="action-btn__icon">▸</span>
            {actionLoading === "start" ? "..." : "Start"}
          </button>
        )}
        <button
          className="action-btn action-btn--console"
          onClick={(e) => {
            e.stopPropagation();
            navigate(`/console/${server.id}`);
          }}
          id={`btn-console-${server.shortId}`}
        >
          <span className="action-btn__icon">&gt;_</span>
          Console
        </button>
      </div>
    </div>
  );
}
