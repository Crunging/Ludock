import { useState } from "react";
import type { ManagedContainer } from "../types";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";
import { can } from "../permissions";

interface ServerCardProps {
  server: ManagedContainer;
  onAction: (id: string, action: "start" | "stop" | "restart") => Promise<void>;
}

export default function ServerCard({ server, onAction }: ServerCardProps) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const isRunning = server.state === "running";
  const bindingBlocked = Boolean(
    server.bindingStatus && server.bindingStatus !== "active",
  );
  const handleAction = async (action: "start" | "stop" | "restart") => {
    if (!can(user, server, `server.${action}`) || bindingBlocked) return;
    if (
      action !== "start" &&
      !window.confirm(
        `${action === "stop" ? "Stop" : "Restart"} ${server.displayName}? Connected players will be disconnected.`,
      )
    )
      return;
    setActionLoading(action);
    try {
      await onAction(server.id, action);
    } finally {
      setActionLoading(null);
    }
  };
  const consoleAvailable =
    can(user, server, "console.execute") && Boolean(server.gameConsole);
  const canOpenConsole =
    consoleAvailable ||
    can(user, server, "logs.read") ||
    can(user, server, "console.shell");

  return (
    <article className="server-row" aria-labelledby={`server-${server.id}`}>
      <div className="server-row__identity">
        <button
          className="text-link server-row__name"
          id={`server-${server.id}`}
          onClick={() => navigate(`/servers/${server.id}`)}
        >
          {server.displayName}
        </button>
        <span className="muted">
          {server.gameType} · {server.image}
        </span>
        {bindingBlocked && (
          <span className="server-row__warning">
            {`Binding ${server.bindingStatus.replaceAll("_", " ")}. Administrator review required.`}
          </span>
        )}
      </div>
      <div className={`server-state server-state--${server.state}`}>
        <span className="status-dot" />
        {server.state}
      </div>
      <div className="server-row__ports">
        {server.ports
          .filter((p) => p.public > 0)
          .map((p) => `${p.public}/${p.type}`)
          .join(", ") || "—"}
      </div>
      <div className="server-row__actions">
        {(["start", "stop", "restart"] as const)
          .filter(
            (action) =>
              can(user, server, `server.${action}`) &&
              (action === "start" ? !isRunning : isRunning),
          )
          .map((action) => (
            <button
              className={`secondary-btn ${action === "stop" ? "secondary-btn--danger" : ""}`}
              key={action}
              disabled={actionLoading !== null || bindingBlocked}
              onClick={() => void handleAction(action)}
            >
              {actionLoading === action
                ? "Working…"
                : action.charAt(0).toUpperCase() + action.slice(1)}
            </button>
          ))}
        {canOpenConsole && (
          <button
            className="secondary-btn"
            onClick={() => navigate(`/console/${server.id}`)}
          >
            {consoleAvailable ? "Console" : "Logs"}
          </button>
        )}
        {can(user, server, "files.read") && server.fileRoots.length > 0 && (
          <button
            className="secondary-btn"
            onClick={() => navigate(`/files/${server.id}`)}
          >
            Files
          </button>
        )}
        <button
          className="secondary-btn"
          aria-label={`Manage ${server.displayName}`}
          onClick={() => navigate(`/servers/${server.id}`)}
        >
          Manage
        </button>
      </div>
    </article>
  );
}
