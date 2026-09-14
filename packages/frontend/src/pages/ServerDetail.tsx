import { useCallback, useEffect, useRef, useState } from "react";
import {
  availabilityResponseSchema,
  backupsResponseSchema,
  bindingReviewResponseSchema,
  okResponseSchema,
  operationResponseSchema,
  operationsResponseSchema,
  scheduleResponseSchema,
  scheduleSchema,
  schedulesResponseSchema,
  serverResponseSchema,
  updateCapabilityResponseSchema,
  type AvailabilityPolicy,
  type Backup,
  type Operation,
  type Schedule,
  type ScheduleInput,
  type UpdateCapability,
  type Server,
} from "@ludock/shared";
import { ApiRequestError, apiJson, jsonBody } from "../api";
import { useAuth } from "../auth-context";
import { NavLink } from "../navigation";
import { can } from "../permissions";
import { operationActive } from "../operations";
import { lifecycleActionForState, lifecycleStateGuidance } from "../server-lifecycle";
import SectionTabs from "../components/SectionTabs";
import LifecycleConfirmation from "../components/LifecycleConfirmation";
import ActivityPanel, { type ActivityFilters } from "../components/server-detail/ActivityPanel";
import BackupsPanel, {
  type RestoreSelection,
} from "../components/server-detail/BackupsPanel";
import SchedulesPanel, { type ScheduleEdit } from "../components/server-detail/SchedulesPanel";
import UpdatePanel, {
  type UpdateOptions,
} from "../components/server-detail/UpdatePanel";
import AvailabilityPanel from "../components/server-detail/AvailabilityPanel";
import BindingReviewPanel from "../components/server-detail/BindingReviewPanel";
import "./server-detail.css";
import { useViewPreferences } from "../view-preferences-context";
import { useBackupPreflight } from "../use-backup-preflight";

const defaultSchedule: ScheduleInput = {
  action: "start",
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
};

function scheduleInput(item: Schedule): ScheduleInput {
  return {
    action: item.action, enabled: item.enabled, time: item.time,
    days: [...item.days], timezone: item.timezone,
  };
}

export default function ServerDetail({ serverId }: { serverId: string }) {
  const { user } = useAuth();
  return (
    <ServerDetailSession
      key={`${serverId}-${user?.id}-${user?.role}`}
      serverId={serverId}
    />
  );
}

function ServerDetailSession({ serverId }: { serverId: string }) {
  const { user } = useAuth();
  const admin = user?.role === "admin";
  const [server, setServer] = useState<Server | null>(null);
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
  const [snapshotReady, setSnapshotReady] = useState(false);
  const [settingsState, setSettingsState] = useState<"loading" | "ready" | "error">(
    "loading",
  );
  const [settingsAttempt, setSettingsAttempt] = useState(0);
  const [capabilityState, setCapabilityState] = useState<"loading" | "ready" | "error">(
    "loading",
  );
  const [capabilityAttempt, setCapabilityAttempt] = useState(0);
  const capabilityFocusPending = useRef(false);
  const detailRoot = useRef<HTMLDivElement>(null);
  const refreshRequest = useRef<AbortController | null>(null);
  const mutationPending = useRef(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const { serverTabs, rememberServerTab } = useViewPreferences();
  const pageActive = useRef(false);
  useEffect(() => {
    pageActive.current = true;
    return () => {
      pageActive.current = false;
      refreshRequest.current?.abort();
    };
  }, []);
  const tab = serverTabs[serverId] || "activity";
  const setTab = (next: string) => {
    // Remembered tabs outlive this page. A completed request from a previous
    // visit must not change the tab chosen during a later visit.
    if (pageActive.current) rememberServerTab(serverId, next);
  };
  const [lifecycleAction, setLifecycleAction] = useState<
    "start" | "stop" | "restart" | null
  >(null);
  const [confirmLifecycle, setConfirmLifecycle] = useState<
    "stop" | "restart" | null
  >(null);
  // Keep drafts above the panels so changing tabs never discards a selection.
  const [activityFilters, setActivityFilters] = useState<ActivityFilters>({ status: "all", kind: "all" });
  const [schedule, setSchedule] = useState<ScheduleInput>(defaultSchedule);
  const [scheduleEdit, setScheduleEdit] = useState<ScheduleEdit | null>(null);
  const scheduleFocusTarget = useRef<string | null>(null);
  const [selectedOperationId, setSelectedOperationId] = useState<string | null>(null);
  const activityFocusTarget = useRef<"scheduled-operation-title" | "recent-operations-title" | null>(null);
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
  const startable = Boolean(server && lifecycleActionForState(server.state) === "start");
  const activeOperation = operations.find(operationActive);
  const backupReadiness = useBackupPreflight(
    path,
    tab === "backups" && can(user, server, "backups.create") && !blocked && !activeOperation,
    `${server?.shortId}:${server?.state}:${server?.bindingStatus}`,
  );

  useEffect(() => {
    if (scheduleEdit || busy || !snapshotReady || tab !== "schedules" || !scheduleFocusTarget.current) return;
    document.getElementById(`schedule-edit-${scheduleFocusTarget.current}`)?.focus();
    scheduleFocusTarget.current = null;
  }, [scheduleEdit, busy, snapshotReady, tab]);

  useEffect(() => {
    if (tab !== "activity" || !activityFocusTarget.current) return;
    document.getElementById(activityFocusTarget.current)?.focus();
    activityFocusTarget.current = null;
  }, [selectedOperationId, tab]);

  useEffect(() => {
    if (capabilityState === "loading" || !capabilityFocusPending.current) return;
    capabilityFocusPending.current = false;
    if (tab === "update" && document.activeElement === document.body)
      detailRoot.current?.querySelector<HTMLElement>('[role="tabpanel"]:not([hidden])')?.focus();
  }, [capabilityState, tab]);

  const refresh = useCallback(async (replacePending = true, afterMutation = false) => {
    if (
      !pageActive.current ||
      (mutationPending.current && !afterMutation) ||
      (!replacePending && refreshRequest.current)
    ) return;
    refreshRequest.current?.abort();
    const controller = new AbortController();
    refreshRequest.current = controller;
    setSnapshotReady(false);
    const ownsRequest = () =>
      pageActive.current &&
      refreshRequest.current === controller &&
      !controller.signal.aborted;
    const init = { signal: controller.signal };
    try {
      const { server: next } = await apiJson(path, serverResponseSchema, init);
      if (!ownsRequest()) return;
      setServer(next);
      const [activity, backupResponse, scheduleResponse] = await Promise.all([
        apiJson(`${path}/operations`, operationsResponseSchema, init),
        admin && can(user, next, "backups.read")
          ? apiJson(`${path}/backups`, backupsResponseSchema, init)
          : Promise.resolve({ backups: [] }),
        can(user, next, "schedules.manage")
          ? apiJson(`${path}/schedules`, schedulesResponseSchema, init)
          : Promise.resolve({ schedules: [] }),
      ]);
      if (!ownsRequest()) return;
      setOperations(activity.operations);
      setBackups(backupResponse.backups);
      setSchedules(scheduleResponse.schedules);
      setSnapshotReady(true);
      setError(null);
    } catch (reason) {
      if (!ownsRequest()) return;
      setSnapshotReady(false);
      if (
        reason instanceof ApiRequestError &&
        (reason.status < 400 || [401, 403, 404].includes(reason.status))
      ) {
        setServer(null);
        setOperations([]);
        setBackups([]);
        setSchedules([]);
      }
      setError(
        reason instanceof Error ? reason.message : "Unable to refresh server.",
      );
    } finally {
      if (ownsRequest()) {
        refreshRequest.current = null;
        setLoading(false);
      }
    }
  }, [admin, path, user]);
  useEffect(() => {
    void refresh();
  }, [refresh]);
  useEffect(() => {
    if (!admin) return;
    const controller = new AbortController();
    setCapabilityState("loading");
    apiJson(`${path}/update-capability`, updateCapabilityResponseSchema, {
      signal: controller.signal,
    })
      .then(({ capability: next }) => {
        if (controller.signal.aborted) return;
        setCapability(next);
        setCapabilityState("ready");
      })
      .catch(() => {
        if (!controller.signal.aborted) setCapabilityState("error");
      });
    return () => controller.abort();
  }, [admin, path, capabilityAttempt]);
  useEffect(() => {
    if (!admin) return;
    const controller = new AbortController();
    setSettingsState("loading");
    apiJson(`${path}/availability`, availabilityResponseSchema, {
      signal: controller.signal,
    })
      .then((monitor) => {
        if (controller.signal.aborted) return;
        setAvailability(monitor.policy);
        setSettingsState("ready");
      })
      .catch(() => {
        if (!controller.signal.aborted) setSettingsState("error");
      });
    return () => controller.abort();
  }, [admin, path, settingsAttempt]);
  const hasActiveOperation = Boolean(activeOperation);
  useEffect(() => {
    const interval = window.setInterval(
      () => { void refresh(false); },
      hasActiveOperation ? 2000 : 10000,
    );
    return () => window.clearInterval(interval);
  }, [hasActiveOperation, refresh]);

  async function perform(
    action: () => Promise<unknown>,
    success: string,
    onFailure?: (reason: unknown) => Promise<void>,
  ) {
    if (
      mutationPending.current ||
      refreshRequest.current ||
      !snapshotReady ||
      !pageActive.current
    ) return false;
    mutationPending.current = true;
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      const result = await action();
      if (result === false) return false;
      if (!pageActive.current) return false;
      setNotice(success);
      await refresh(true, true);
      // The action succeeded even if reading its updated state failed.
      return pageActive.current;
    } catch (reason) {
      if (pageActive.current) await onFailure?.(reason);
      if (pageActive.current)
        setError(reason instanceof Error ? reason.message : "Request failed.");
      return false;
    } finally {
      mutationPending.current = false;
      if (pageActive.current) setBusy(false);
    }
  }

  async function reconcileScheduleFailure(reason: unknown) {
    if (reason instanceof ApiRequestError && [403, 404, 409].includes(reason.status)) {
      await refresh(true, true);
    }
  }

  function cancelScheduleEdit() {
    if (busy || !scheduleEdit) return;
    scheduleFocusTarget.current = scheduleEdit.id;
    setScheduleEdit(null);
  }

  async function saveSchedule() {
    const draft = scheduleEdit?.draft ?? schedule;
    const current = scheduleEdit ? schedules.find((item) => item.id === scheduleEdit.id) : undefined;
    if (
      blocked || !can(user, server, "schedules.manage") ||
      !can(user, server, draft.action === "backup" ? "backups.create" : `server.${draft.action}`) ||
      !scheduleSchema.safeParse(draft).success ||
      (scheduleEdit && (!current || current.revision !== scheduleEdit.revision))
    ) return;
    if (await perform(
      () => apiJson(
        `${path}/schedules${scheduleEdit ? `/${encodeURIComponent(scheduleEdit.id)}` : ""}`,
        scheduleResponseSchema,
        jsonBody(scheduleEdit ? "PUT" : "POST", scheduleEdit ? { ...draft, revision: scheduleEdit.revision } : draft),
      ),
      scheduleEdit ? "Schedule saved." : "Schedule created.",
      reconcileScheduleFailure,
    ) && scheduleEdit) {
      scheduleFocusTarget.current = scheduleEdit.id;
      setScheduleEdit(null);
    }
  }

  async function toggleSchedule(item: Schedule) {
    const current = schedules.find((candidate) => candidate.id === item.id);
    if (
      scheduleEdit || !can(user, server, "schedules.manage") || !current || current.revision !== item.revision ||
      (!item.enabled && (blocked || !can(user, server, item.action === "backup" ? "backups.create" : `server.${item.action}`)))
    ) return;
    await perform(
      () => apiJson(`${path}/schedules/${encodeURIComponent(item.id)}`, scheduleResponseSchema,
        jsonBody("PATCH", { enabled: !item.enabled, revision: item.revision })),
      item.enabled ? "Schedule paused." : "Schedule resumed.",
      reconcileScheduleFailure,
    );
  }
  async function requestBackup() {
    if (
      !can(user, server, "backups.create") ||
      blocked ||
      activeOperation ||
      mutationPending.current ||
      !snapshotReady
    ) return;
    if (
      await perform(
        async () => {
          const preflight = await backupReadiness.check();
          if (!pageActive.current || !preflight?.ready) return false;
          if (!window.confirm(
            `Create a backup of ${server?.displayName}? The server stops for the entire copy and returns to its previous running state afterward.`,
          )) return false;
          return apiJson(
            `${path}/backups`,
            operationResponseSchema,
            jsonBody("POST", {}),
          );
        },
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
        ? !startable
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
    if (
      !admin ||
      !can(user, server, update.forceRecreate ? "server.recreate" : "server.update") ||
      !capability?.available ||
      capabilityState !== "ready" ||
      blocked ||
      activeOperation ||
      !update.confirmed ||
      (!update.createBackup && update.skipConfirmation !== server?.displayName)
    ) return;
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
      !can(user, server, "backups.restore") ||
      blocked ||
      activeOperation ||
      !restore.backup ||
      !backups.some((item) =>
        item.id === restore.backup?.id && item.state === "complete",
      ) ||
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
        <button className="secondary-btn" onClick={() => void refresh()}>
          Try again
        </button>
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
  const activeSettingsState = activeTab === "update" ? capabilityState : settingsState;
  const stateGuidance = lifecycleStateGuidance(server.state);
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
        ? startable
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
    <div className="page server-detail" ref={detailRoot}>
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
                    disabled={loading || busy || !snapshotReady || blocked || Boolean(activeOperation)}
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
      {!blocked && stateGuidance && (
        <p className="section-note" role="status">
          {stateGuidance}
        </p>
      )}
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
          busy={busy || !snapshotReady}
          confirmation={bindingConfirmation}
          onConfirmationChange={setBindingConfirmation}
          onAccept={() => {
            if (
              bindingConfirmation !== server.displayName ||
              server.bindingStatus !== "review_required"
            ) return;
            void perform(
              () =>
                apiJson(
                  `${path}/binding-review`,
                  bindingReviewResponseSchema,
                  jsonBody("POST", { confirmation: bindingConfirmation }),
                ),
              "Server binding reviewed.",
            );
          }}
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
      {activeOperation && activeTab !== "activity" && (
        <div className="detail-operation-notice">
          <p role="status">
            <strong className="capitalize">{activeOperation.kind.replaceAll("_", " ")}</strong>
            {activeOperation.status === "queued" ? " queued. " : " in progress. "}
            Server controls, backups, and updates are paused until it finishes.
          </p>
          <button className="text-link" onClick={() => {
            setActivityFilters({ status: "all", kind: "all" });
            setTab("activity");
          }}>
            View progress
          </button>
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
            serverId={serverId}
            filters={activityFilters}
            onFiltersChange={setActivityFilters}
            selectedOperationId={selectedOperationId}
            onCloseOperation={() => {
              activityFocusTarget.current = "recent-operations-title";
              setSelectedOperationId(null);
            }}
            operations={operations}
            admin={admin}
            onRefresh={() => void refresh()}
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
            latestBackup={server.latestBackup}
            preflight={backupReadiness.preflight}
            checking={backupReadiness.checking}
            preflightError={backupReadiness.error}
            onCheck={() => void backupReadiness.check()}
            restore={restore}
            onRestoreChange={setRestore}
            admin={admin}
            canRead={can(user, server, "backups.read")}
            canRestore={can(user, server, "backups.restore")}
            canDelete={can(user, server, "backups.delete")}
            canCreate={canCreateBackup}
            busy={busy || !snapshotReady}
            blocked={blocked}
            hasActiveOperation={Boolean(activeOperation)}
            onCreate={() => void requestBackup()}
            onRestore={() => void requestRestore()}
            onDelete={(backup) => {
              if (
                !can(user, server, "backups.delete") ||
                activeOperation ||
                !backups.some((item) => item.id === backup.id)
              ) return;
              void perform(
                () =>
                  apiJson(
                    `${path}/backups/${encodeURIComponent(backup.id)}`,
                    okResponseSchema,
                    { method: "DELETE" },
                  ),
                "Backup deleted.",
              );
            }}
          />
        )}
        {activeTab === "schedules" && canManageSchedules && (
          <SchedulesPanel
            schedules={schedules}
            draft={scheduleEdit?.draft ?? schedule}
            editing={scheduleEdit}
            conflict={Boolean(scheduleEdit && schedules.some((item) => item.id === scheduleEdit.id && item.revision !== scheduleEdit.revision))}
            onDraftChange={(draft) => {
              if (scheduleEdit) setScheduleEdit((current) => current ? { ...current, draft } : current);
              else setSchedule(draft);
            }}
            scheduleActions={scheduleActions}
            busy={busy || !snapshotReady}
            saving={busy}
            blocked={blocked}
            onSave={() => void saveSchedule()}
            onEdit={(item) => {
              if (busy || !snapshotReady || blocked || scheduleEdit || scheduleActions.length === 0) return;
              setScheduleEdit({ id: item.id, revision: item.revision, draft: scheduleInput(item) });
            }}
            onCancelEdit={cancelScheduleEdit}
            onResolveConflict={(useSaved) => {
              const latest = schedules.find((item) => item.id === scheduleEdit?.id);
              if (!scheduleEdit || !latest || busy || !snapshotReady) return;
              setScheduleEdit({
                id: latest.id,
                revision: latest.revision,
                draft: useSaved ? scheduleInput(latest) : { ...scheduleEdit.draft, enabled: latest.enabled },
              });
              setError(null);
            }}
            onToggle={(item) => void toggleSchedule(item)}
            onViewActivity={(item) => {
              if (!item.lastOperation || !can(user, server, "server.view")) return;
              activityFocusTarget.current = "scheduled-operation-title";
              setSelectedOperationId(item.lastOperation.id);
              setTab("activity");
            }}
            onDelete={(item) => {
              if (
                !canManageSchedules ||
                !schedules.some((current) => current.id === item.id)
              ) return;
              void perform(
                () =>
                  apiJson(
                    `${path}/schedules/${encodeURIComponent(item.id)}`,
                    okResponseSchema,
                    { method: "DELETE" },
                  ),
                "Schedule deleted.",
              );
            }}
          />
        )}
        {admin &&
          (activeTab === "update" || activeTab === "availability") &&
          activeSettingsState !== "ready" && (
            activeSettingsState === "loading" ? (
              <p role="status">Loading server settings…</p>
            ) : (
              <div>
                <p role="alert">
                  Unable to load server settings. Reload them before making changes.
                </p>
                <button
                  className="secondary-btn"
                  onClick={() => {
                    if (activeTab === "update") setCapabilityAttempt((value) => value + 1);
                    else setSettingsAttempt((value) => value + 1);
                  }}
                >
                  Reload server settings
                </button>
              </div>
            )
          )}
        {activeTab === "update" && admin && capabilityState === "ready" && (
          <UpdatePanel
            serverName={server.displayName}
            capability={capability}
            value={update}
            onChange={setUpdate}
            busy={busy || !snapshotReady || !can(
              user,
              server,
              update.forceRecreate ? "server.recreate" : "server.update",
            )}
            blocked={blocked}
            hasActiveOperation={Boolean(activeOperation)}
            onRecheck={() => {
              capabilityFocusPending.current = true;
              setCapabilityAttempt((value) => value + 1);
            }}
            onSubmit={() => void requestUpdate()}
          />
        )}
        {activeTab === "availability" && admin && settingsState === "ready" && (
          <AvailabilityPanel
            value={availability}
            onChange={setAvailability}
            busy={busy || !snapshotReady}
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
