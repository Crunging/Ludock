import type { Server } from "@ludock/shared";
import { useState } from "react";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";
import { NavLink } from "../navigation";
import { can } from "../permissions";
import LifecycleConfirmation from "./LifecycleConfirmation";
import ServerMoreActions from "./ServerMoreActions";
import { lifecycleActionForState, lifecycleStateGuidance } from "../server-lifecycle";
import "./server-list.css";

interface ServerCardProps {
  server: Server;
  actionsDisabled?: boolean;
  onAction: (id: string, action: "start" | "stop" | "restart") => Promise<void>;
}

export default function ServerCard({ server, onAction, actionsDisabled = false }: ServerCardProps) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const [confirmation, setConfirmation] = useState<"stop" | "restart" | null>(
    null,
  );
  const isRunning = server.state === "running";
  const lifecycleAction = lifecycleActionForState(server.state);
  const stateGuidance = lifecycleStateGuidance(server.state);
  const bindingBlocked = Boolean(
    server.bindingStatus && server.bindingStatus !== "active",
  );
  const handleAction = async (action: "start" | "stop" | "restart") => {
    if (
      !can(user, server, `server.${action}`) ||
      bindingBlocked ||
      actionLoading ||
      actionsDisabled ||
      (action === "start" ? lifecycleAction !== "start" : !isRunning)
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
    (can(user, server, "console.execute") && Boolean(server.gameConsole)) ||
    can(user, server, "console.shell");
  const canOpenConsole =
    consoleAvailable || can(user, server, "logs.read");
  const canReadBackupSummary =
    user?.role === "admin" ||
    can(user, server, "backups.read") ||
    can(user, server, "backups.create");
  const secondaryActions = [
    ...(can(user, server, "files.read") && server.fileRoots.length > 0
      ? [{ label: "Files", onSelect: () => navigate(`/files/${server.id}`) }]
      : []),
    ...(can(user, server, "server.restart") && isRunning
      ? [
          {
            label: actionLoading === "restart" ? "Restarting…" : "Restart…",
            disabled: actionLoading !== null || bindingBlocked || actionsDisabled,
            onSelect: () => setConfirmation("restart"),
          },
        ]
      : []),
  ];

  return (
    <article className="server-row" aria-labelledby={`server-${server.id}`}>
      <div className="server-row__identity">
        <NavLink
          className="text-link server-row__name"
          id={`server-${server.id}`}
          to={`/servers/${server.id}`}
        >
          {server.displayName}
        </NavLink>
        <span className="muted">
          <span className="server-row__game">{server.gameType}</span>
          <span className="server-row__image">{server.image}</span>
        </span>
        {canReadBackupSummary && (
          <p className="server-row__backup">
            {server.latestBackup ? (
              <>
                Latest successful backup:{" "}
                <time dateTime={new Date(server.latestBackup.createdAt).toISOString()}>
                  {new Date(server.latestBackup.createdAt).toLocaleString()}
                </time>
              </>
            ) : "No successful backup retained"}
          </p>
        )}
        {stateGuidance && <p className="muted">{stateGuidance}</p>}
        {bindingBlocked && (
          <span className="server-row__warning">
            {`Binding ${server.bindingStatus.replaceAll("_", " ")}. Administrator review required.`}
          </span>
        )}
      </div>
      <div className={`server-state server-state--${server.state}`}>
        <span className="status-dot" aria-hidden="true" />
        {server.state}
      </div>
      <div className="server-row__ports">
        <span className="server-row__ports-label">Ports</span>
        <span>
          {server.ports
            .filter((p) => p.public > 0)
            .map((p) => `${p.public}/${p.type}`)
            .join(", ") || "None published"}
        </span>
      </div>
      <div className="server-row__actions">
        {canOpenConsole && (
          <NavLink
            className="secondary-btn secondary-btn--accent"
            to={`/console/${server.id}`}
          >
            {consoleAvailable ? "Console" : "Logs"}
          </NavLink>
        )}
        {lifecycleAction && can(user, server, `server.${lifecycleAction}`) && (
          <button
            className={`secondary-btn ${isRunning ? "secondary-btn--danger" : ""}`}
            disabled={actionLoading !== null || bindingBlocked || actionsDisabled}
            onClick={() =>
              isRunning
                ? setConfirmation("stop")
                : void handleAction("start")
            }
          >
            {actionLoading === lifecycleAction
              ? "Working…"
              : isRunning ? "Stop" : "Start"}
          </button>
        )}
        {secondaryActions.length > 0 && (
          <ServerMoreActions
            serverName={server.displayName}
            actions={secondaryActions}
          />
        )}
      </div>
      {confirmation && (
        <LifecycleConfirmation
          action={confirmation}
          serverName={server.displayName}
          fallbackFocusId={`server-${server.id}`}
          onCancel={() => setConfirmation(null)}
          onConfirm={() => {
            setConfirmation(null);
            void handleAction(confirmation);
          }}
        />
      )}
    </article>
  );
}
