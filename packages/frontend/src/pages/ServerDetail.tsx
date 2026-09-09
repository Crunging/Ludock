import { useCallback, useEffect, useState } from "react";
import {
  availabilityResponseSchema,
  backupsResponseSchema,
  bindingReviewResponseSchema,
  okResponseSchema,
  operationResponseSchema,
  operationsResponseSchema,
  scheduleResponseSchema,
  schedulesResponseSchema,
  serverResponseSchema,
  updateCapabilityResponseSchema,
  type AvailabilityPolicy,
  type Backup,
  type Operation,
  type Schedule,
  type ScheduleInput,
  type UpdateCapability,
} from "@ludock/shared";
import { apiJson, jsonBody } from "../api";
import { useAuth } from "../auth-context";
import { NavLink } from "../navigation";
import { can } from "../permissions";
import type { ManagedContainer } from "../types";
import { operationActive } from "../operations";
import SectionTabs from "../components/SectionTabs";
import LifecycleConfirmation from "../components/LifecycleConfirmation";
import ActivityPanel from "../components/server-detail/ActivityPanel";
import BackupsPanel, {
  type RestoreSelection,
} from "../components/server-detail/BackupsPanel";
import SchedulesPanel from "../components/server-detail/SchedulesPanel";
import UpdatePanel, {
  type UpdateOptions,
} from "../components/server-detail/UpdatePanel";
import AvailabilityPanel from "../components/server-detail/AvailabilityPanel";
import BindingReviewPanel from "../components/server-detail/BindingReviewPanel";
import "./server-detail.css";

const defaultSchedule: ScheduleInput = {
  action: "start",
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
};

export default function ServerDetail({ serverId }: { serverId: string }) {
  const { user } = useAuth();
  const admin = user?.role === "admin";
  const [server, setServer] = useState<ManagedContainer | null>(null);
  const [operations, setOperations] = useState<Operation[]>([]);
  const [backups, setBackups] = useState<Backup[]>([]);
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [capability, setCapability] = useState<UpdateCapability | null>(null);
  const [availability, setAvailability] = useState<AvailabilityPolicy>({
    enabled: false,
    maintenance: false,
    graceSeconds: 120,
  });
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [tab, setTab] = useState("activity");
  const [lifecycleAction, setLifecycleAction] = useState<
    "start" | "stop" | "restart" | null
  >(null);
  const [confirmLifecycle, setConfirmLifecycle] = useState<
    "stop" | "restart" | null
  >(null);
  // Keep drafts above the panels so changing tabs never discards a selection.
  const [schedule, setSchedule] = useState<ScheduleInput>(defaultSchedule);
  const [update, setUpdate] = useState<UpdateOptions>({
    createBackup: true,
    forceRecreate: false,
    confirmed: false,
    skipConfirmation: "",
  });
  const [restore, setRestore] = useState<RestoreSelection>({
    backup: null,
    confirmation: "",
  });
  const [bindingConfirmation, setBindingConfirmation] = useState("");
  const path = `/servers/${encodeURIComponent(serverId)}`;
  const blocked = server?.bindingStatus !== "active";
  const activeOperation = operations.find(operationActive);

  const refresh = useCallback(async () => {
    const { server: next } = await apiJson(path, serverResponseSchema);
    setServer(next);
    const [activity, backupResponse, scheduleResponse] = await Promise.all([
      apiJson(`${path}/operations`, operationsResponseSchema),
      admin
        ? apiJson(`${path}/backups`, backupsResponseSchema)
        : Promise.resolve({ backups: [] }),
      can(user, next, "schedules.manage")
        ? apiJson(`${path}/schedules`, schedulesResponseSchema)
        : Promise.resolve({ schedules: [] }),
    ]);
    setOperations(activity.operations);
    setBackups(backupResponse.backups);
    setSchedules(scheduleResponse.schedules);
  }, [admin, path, user]);
  useEffect(() => {
    setLoading(true);
    refresh()
      .catch((reason) =>
        setError(
          reason instanceof Error ? reason.message : "Unable to load server.",
        ),
      )
      .finally(() => setLoading(false));
  }, [refresh]);
  useEffect(() => {
    if (!admin) return;
    Promise.all([
      apiJson(`${path}/update-capability`, updateCapabilityResponseSchema),
      apiJson(`${path}/availability`, availabilityResponseSchema),
    ])
      .then(([nextCapability, monitor]) => {
        setCapability(nextCapability.capability);
        setAvailability(monitor.policy);
      })
      .catch((reason) =>
        setError(
          reason instanceof Error
            ? reason.message
            : "Unable to load server settings.",
        ),
      );
  }, [admin, path]);
  useEffect(() => {
    const interval = window.setInterval(
      () => {
        void refresh().catch((reason) =>
          setError(
            reason instanceof Error
              ? reason.message
              : "Unable to refresh server.",
          ),
        );
      },
      activeOperation ? 2000 : 10000,
    );
    return () => window.clearInterval(interval);
  }, [activeOperation, refresh]);

  async function perform(action: () => Promise<unknown>, success: string) {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      await action();
      setNotice(success);
      await refresh();
      return true;
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Request failed.");
      return false;
    } finally {
      setBusy(false);
    }
  }
  async function requestBackup() {
    if (!can(user, server, "backups.create")) return;
    if (
      !window.confirm(
        `Create a backup of ${server?.displayName}? The server stops for the entire copy and returns to its previous running state afterward.`,
      )
    )
      return;
    if (
      await perform(
        () =>
          apiJson(
            `${path}/backups`,
            operationResponseSchema,
            jsonBody("POST", {}),
          ),
        "Backup queued. Follow its progress in Activity.",
      )
    )
      setTab("activity");
  }
  async function requestLifecycle(action: "start" | "stop" | "restart") {
    setConfirmLifecycle(null);
    if (
      !can(user, server, `server.${action}`) ||
      blocked ||
      loading ||
      busy ||
      activeOperation ||
      (action === "start"
        ? server?.state === "running"
        : server?.state !== "running")
    )
      return;
    setLifecycleAction(action);
    await perform(
      () => apiJson(`${path}/${action}`, okResponseSchema, { method: "POST" }),
      `Server ${action === "start" ? "started" : action === "stop" ? "stopped" : "restarted"}.`,
    );
    setLifecycleAction(null);
  }
  async function requestUpdate() {
    if (!admin || !update.confirmed) return;
    if (
      await perform(
        () =>
          apiJson(
            `${path}/updates`,
            operationResponseSchema,
            jsonBody("POST", {
              createBackup: update.createBackup,
              forceRecreate: update.forceRecreate,
              ...(update.createBackup
                ? {}
                : { skipBackupConfirmation: update.skipConfirmation }),
            }),
          ),
        update.forceRecreate ? "Recreation queued." : "Update queued.",
      )
    ) {
      setTab("activity");
      setUpdate((current) => ({
        ...current,
        confirmed: false,
        skipConfirmation: "",
      }));
    }
  }
  async function requestRestore() {
    if (
      !admin ||
      !restore.backup ||
      restore.confirmation !== server?.displayName
    )
      return;
    if (
      await perform(
        () =>
          apiJson(
            `${path}/restores`,
            operationResponseSchema,
            jsonBody("POST", {
              backupId: restore.backup!.id,
              confirmation: restore.confirmation,
            }),
          ),
        "Restore queued. The server stays stopped until its data is safe.",
      )
    ) {
      setRestore({ backup: null, confirmation: "" });
      setTab("activity");
    }
  }

  if (loading && !server)
    return (
      <p className="muted" role="status">
        Loading server…
      </p>
    );
  if (!server)
    return (
      <div className="page">
        <NavLink className="text-link" to="/" end>
          ← Servers
        </NavLink>
        <div className="alert alert--error" role="alert">
          {error || "Server unavailable."}
        </div>
      </div>
    );
  const canCreateBackup = can(user, server, "backups.create");
  const canManageSchedules = can(user, server, "schedules.manage");
  const tabs = [
    { id: "activity", label: "Activity" },
    ...(admin || canCreateBackup ? [{ id: "backups", label: "Backups" }] : []),
    ...(canManageSchedules ? [{ id: "schedules", label: "Schedules" }] : []),
    ...(admin
      ? [
          { id: "update", label: "Update" },
          { id: "availability", label: "Availability" },
        ]
      : []),
  ];
  const activeTab = tabs.some((item) => item.id === tab) ? tab : "activity";
  const consoleAvailable =
    (can(user, server, "console.execute") && Boolean(server.gameConsole)) ||
    can(user, server, "console.shell");
  const canOpenConsole = consoleAvailable || can(user, server, "logs.read");
  const canOpenFiles =
    can(user, server, "files.read") && server.fileRoots.length > 0;
  const lifecycleActions = (["start", "stop", "restart"] as const).filter(
    (action) =>
      can(user, server, `server.${action}`) &&
      (action === "start"
        ? server.state !== "running"
        : server.state === "running"),
  );
  const publishedPorts = server.ports
    .filter((port) => port.public > 0)
    .map((port) => `${port.public}/${port.type}`)
    .join(", ");
  const scheduleActions = (
    ["start", "stop", "restart", "backup"] as const
  ).filter((action) =>
    can(
      user,
      server,
      action === "backup" ? "backups.create" : `server.${action}`,
    ),
  );

  return (
    <div className="page server-detail">
      <NavLink className="text-link back-link" to="/" end>
        <span aria-hidden="true">←</span> All servers
      </NavLink>
      <header className="detail-header">
        <div className="detail-header__title">
          <h1 className="page__title" id="server-detail-title" tabIndex={-1}>
            {server.displayName}
          </h1>
          <div className={`server-state server-state--${server.state}`}>
            <span className="status-dot" aria-hidden="true" />
            {server.state}
          </div>
        </div>
        <p className="detail-header__identity">
          <span className="detail-header__game">{server.gameType}</span>
          {server.name !== server.displayName && (
            <>
              <span aria-hidden="true">·</span>
              <span>{server.name}</span>
            </>
          )}
        </p>
        <dl className="detail-metadata">
          <div>
            <dt>Image</dt>
            <dd>
              <code>{server.image}</code>
            </dd>
          </div>
          <div>
            <dt>Published ports</dt>
            <dd>{publishedPorts ? <code>{publishedPorts}</code> : "None"}</dd>
          </div>
        </dl>
        {(canOpenConsole || canOpenFiles || lifecycleActions.length > 0) && (
          <div className="inline-actions detail-actions">
            {(canOpenConsole || canOpenFiles) && (
              <div className="detail-actions__group">
                {canOpenConsole && (
                  <NavLink
                    className="primary-btn"
                    to={`/console/${encodeURIComponent(server.id)}`}
                  >
                    {consoleAvailable ? "Console" : "Logs"}
                  </NavLink>
                )}
                {canOpenFiles && (
                  <NavLink
                    className="secondary-btn"
                    to={`/files/${encodeURIComponent(server.id)}`}
                  >
                    Files
                  </NavLink>
                )}
              </div>
            )}
            {lifecycleActions.length > 0 && (
              <div className="detail-actions__group">
                {lifecycleActions.map((action) => (
                  <button
                    type="button"
                    className={`secondary-btn${action === "stop" ? " secondary-btn--danger" : ""}`}
                    key={action}
                    disabled={loading || busy || blocked || Boolean(activeOperation)}
                    onClick={() => {
                      if (action === "start") void requestLifecycle(action);
                      else setConfirmLifecycle(action);
                    }}
                  >
                    {lifecycleAction === action
                      ? "Working…"
                      : action.charAt(0).toUpperCase() + action.slice(1)}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
      </header>
      {confirmLifecycle && (
        <LifecycleConfirmation
          action={confirmLifecycle}
          serverName={server.displayName}
          fallbackFocusId="server-detail-title"
          onCancel={() => setConfirmLifecycle(null)}
          onConfirm={() => void requestLifecycle(confirmLifecycle)}
        />
      )}
      {blocked && (
        <div className="alert alert--error" role="alert">
          This server’s binding is {server.bindingStatus.replaceAll("_", " ")}.
          Operations are blocked until an administrator reviews the identity.
        </div>
      )}
      {admin && server.bindingStatus === "review_required" && (
        <BindingReviewPanel
          serverName={server.displayName}
          busy={busy}
          confirmation={bindingConfirmation}
          onConfirmationChange={setBindingConfirmation}
          onAccept={() =>
            void perform(
              () =>
                apiJson(
                  `${path}/binding-review`,
                  bindingReviewResponseSchema,
                  jsonBody("POST", { confirmation: bindingConfirmation }),
                ),
              "Server binding reviewed.",
            )
          }
        />
      )}
      {error && (
        <div className="alert alert--error" role="alert">
          <span>{error}</span>
          <button onClick={() => setError(null)} aria-label="Dismiss error">
            ×
          </button>
        </div>
      )}
      {notice && (
        <div className="alert alert--success" role="status">
          {notice}
        </div>
      )}
      <SectionTabs
        label="Server management"
        tabs={tabs}
        activeId={activeTab}
        onChange={setTab}
      >
        {activeTab === "activity" && (
          <ActivityPanel
            operations={operations}
            admin={admin}
            onRefresh={() =>
              void refresh().catch((reason) => setError(String(reason)))
            }
            onRecreate={() => {
              setUpdate((current) => ({
                ...current,
                forceRecreate: true,
                confirmed: false,
              }));
              setTab("update");
            }}
          />
        )}
        {activeTab === "backups" && (admin || canCreateBackup) && (
          <BackupsPanel
            serverName={server.displayName}
            path={path}
            backups={backups}
            restore={restore}
            onRestoreChange={setRestore}
            admin={admin}
            canCreate={canCreateBackup}
            busy={busy}
            blocked={blocked}
            hasActiveOperation={Boolean(activeOperation)}
            onCreate={() => void requestBackup()}
            onRestore={() => void requestRestore()}
            onDelete={(backup) =>
              void perform(
                () =>
                  apiJson(
                    `${path}/backups/${encodeURIComponent(backup.id)}`,
                    okResponseSchema,
                    { method: "DELETE" },
                  ),
                "Backup deleted.",
              )
            }
          />
        )}
        {activeTab === "schedules" && canManageSchedules && (
          <SchedulesPanel
            schedules={schedules}
            draft={schedule}
            onDraftChange={setSchedule}
            scheduleActions={scheduleActions}
            busy={busy}
            blocked={blocked}
            onCreate={() =>
              void perform(
                () =>
                  apiJson(
                    `${path}/schedules`,
                    scheduleResponseSchema,
                    jsonBody("POST", schedule),
                  ),
                "Schedule created.",
              )
            }
            onDelete={(item) =>
              void perform(
                () =>
                  apiJson(
                    `${path}/schedules/${encodeURIComponent(item.id)}`,
                    okResponseSchema,
                    { method: "DELETE" },
                  ),
                "Schedule deleted.",
              )
            }
          />
        )}
        {activeTab === "update" && admin && (
          <UpdatePanel
            serverName={server.displayName}
            capability={capability}
            value={update}
            onChange={setUpdate}
            busy={busy}
            blocked={blocked}
            hasActiveOperation={Boolean(activeOperation)}
            onSubmit={() => void requestUpdate()}
          />
        )}
        {activeTab === "availability" && admin && (
          <AvailabilityPanel
            value={availability}
            onChange={setAvailability}
            busy={busy}
            onSave={() =>
              void perform(
                () =>
                  apiJson(
                    `${path}/availability`,
                    availabilityResponseSchema,
                    jsonBody("PUT", {
                      enabled: availability.enabled,
                      maintenance: availability.maintenance,
                      graceSeconds: availability.graceSeconds,
                    }),
                  ),
                "Availability settings saved.",
              )
            }
          />
        )}
      </SectionTabs>
    </div>
  );
}
