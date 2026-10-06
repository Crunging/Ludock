import type { Server } from "@ludock/shared";
import { useState } from "react";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";
import { NavLink } from "../navigation";
import { can, canReadBackupSummary } from "../permissions";
import { formatDateTime, formatRelativeTimeSentence } from "../format";
import { useNow } from "../hooks/useNow";
import LifecycleConfirmation from "./LifecycleConfirmation";
import ActionMenu from "./ActionMenu";
import ServerState from "./ServerState";
import CopyAddress from "./CopyAddress";
import {
  connectAddress,
  lifecycleActionForState,
  lifecycleStateGuidance,
  serverStatus,
  stateSinceText,
} from "../server-lifecycle";
import "./server-list.css";

interface ServerCardProps {
  server: Server;
  actionsDisabled?: boolean;
  /** The list shows a backup column when any listed server has a backup summary. */
  showBackup?: boolean;
  onAction: (id: string, action: "start" | "stop" | "restart") => Promise<void>;
}

export default function ServerCard({ server, onAction, actionsDisabled = false, showBackup = false }: ServerCardProps) {
  const navigate = useNavigate();
  const { user } = useAuth();
  const now = useNow();
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
  const address = connectAddress(server);
  const stopped = lifecycleAction === "start";
  const failed = serverStatus(server).tone === "failed";

  return (
    <article
      className={`server-row${stopped ? " server-row--stopped" : ""}${failed ? " server-row--failed" : ""}`}
      aria-labelledby={`server-${server.id}`}
    >
      <div className="server-row__identity">
        <NavLink
          className="server-row__name"
          id={`server-${server.id}`}
          to={`/servers/${server.id}`}
        >
          {server.displayName}
        </NavLink>
        <span className="server-row__game">{server.gameName}</span>
        {stateGuidance && <p className="server-row__note">{stateGuidance}</p>}
        {bindingBlocked && (
          <p className="server-row__warning">
            {`Binding ${server.bindingStatus.replaceAll("_", " ")}. Administrator review required.`}
          </p>
        )}
      </div>
      <div className="server-row__state">
        <ServerState server={server} />
        {server.stateSince !== null && (
          <time
            className="server-row__since"
            dateTime={new Date(server.stateSince).toISOString()}
            title={formatDateTime(server.stateSince)}
          >
            {stateSinceText(server, { standalone: true, now })}
          </time>
        )}
      </div>
      <div className="server-row__address">
        <span className="server-row__label">Connect</span>
        {address
          ? <CopyAddress address={address} serverName={server.displayName} />
          : <span className="muted">No public port</span>}
      </div>
      {showBackup && (
        <div className="server-row__backup">
          <span className="server-row__label">Last backup</span>
          {!canReadBackupSummary(user, server) ? (
            <span className="muted">—</span>
          ) : server.latestBackup ? (
            <time
              dateTime={new Date(server.latestBackup.createdAt).toISOString()}
              title={formatDateTime(server.latestBackup.createdAt)}
            >
              {formatRelativeTimeSentence(server.latestBackup.createdAt, now)}
            </time>
          ) : (
            <span className="server-row__never">Never</span>
          )}
        </div>
      )}
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
          <ActionMenu
            label={`More actions for ${server.displayName}`}
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
