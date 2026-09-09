import { useCallback, useEffect, useState, type FormEvent } from "react";
import type {
  Backup,
  Operation,
  ScheduleInput,
  UpdateCapability,
} from "@ludock/shared";
import { apiJson, jsonBody } from "../api";
import { useAuth } from "../auth-context";
import { useNavigate } from "../navigation-context";
import { can } from "../permissions";
import type { ManagedContainer } from "../types";
import OperationList from "../components/OperationList";
import { operationActive } from "../operations";

interface Schedule extends ScheduleInput {
  id: string;
  ownerId?: string;
  nextRunAt?: number | null;
  lastResult?: string | null;
}
interface Availability {
  enabled: boolean;
  maintenance: boolean;
  graceSeconds: number;
  status?: string;
}
const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const defaultSchedule: ScheduleInput = {
  action: "start",
  enabled: true,
  time: "08:00",
  days: [0, 1, 2, 3, 4, 5, 6],
  timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
};
const formatSize = (size: number) =>
  size >= 1024 ** 3
    ? `${(size / 1024 ** 3).toFixed(2)} GiB`
    : `${(size / 1024 ** 2).toFixed(1)} MiB`;

export default function ServerDetail({ serverId }: { serverId: string }) {
  const { user } = useAuth();
  const navigate = useNavigate();
  const admin = user?.role === "admin";
  const [server, setServer] = useState<ManagedContainer | null>(null);
  const [operations, setOperations] = useState<Operation[]>([]);
  const [backups, setBackups] = useState<Backup[]>([]);
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [capability, setCapability] = useState<UpdateCapability | null>(null);
  const [availability, setAvailability] = useState<Availability>({
    enabled: false,
    maintenance: false,
    graceSeconds: 120,
  });
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [tab, setTab] = useState("activity");
  const [schedule, setSchedule] = useState<ScheduleInput>(defaultSchedule);
  const [createBackup, setCreateBackup] = useState(true);
  const [forceRecreate, setForceRecreate] = useState(false);
  const [updateConfirm, setUpdateConfirm] = useState(false);
  const [skipConfirmation, setSkipConfirmation] = useState("");
  const [restoreBackup, setRestoreBackup] = useState<Backup | null>(null);
  const [restoreConfirmation, setRestoreConfirmation] = useState("");
  const [bindingConfirmation, setBindingConfirmation] = useState("");
  const path = `/servers/${encodeURIComponent(serverId)}`;
  const blocked = server?.bindingStatus !== "active";
  const activeOperation = operations.find(operationActive);

  const refresh = useCallback(async () => {
    const { server: next } = await apiJson<{ server: ManagedContainer }>(path);
    setServer(next);
    const [{ operations: nextOperations }, backupResponse, scheduleResponse] =
      await Promise.all([
        apiJson<{ operations: Operation[] }>(`${path}/operations`),
        admin
          ? apiJson<{ backups: Backup[] }>(`${path}/backups`)
          : Promise.resolve({ backups: [] }),
        can(user, next, "schedules.manage")
          ? apiJson<{ schedules: Schedule[] }>(`${path}/schedules`)
          : Promise.resolve({ schedules: [] }),
      ]);
    setOperations(nextOperations);
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
      apiJson<{ capability: UpdateCapability }>(`${path}/update-capability`),
      apiJson<{ policy: Availability }>(`${path}/availability`),
    ])
      .then(([update, monitor]) => {
        setCapability(update.capability);
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
        () => apiJson(`${path}/backups`, jsonBody("POST", {})),
        "Backup queued. Follow its progress in Activity.",
      )
    )
      setTab("activity");
  }
  async function addSchedule(event: FormEvent) {
    event.preventDefault();
    await perform(
      () => apiJson(`${path}/schedules`, jsonBody("POST", schedule)),
      "Schedule created.",
    );
  }
  async function requestUpdate(event: FormEvent) {
    event.preventDefault();
    if (!admin || !updateConfirm) return;
    if (
      await perform(
        () =>
          apiJson(
            `${path}/updates`,
            jsonBody("POST", {
              createBackup,
              forceRecreate,
              ...(createBackup
                ? {}
                : { skipBackupConfirmation: skipConfirmation }),
            }),
          ),
        forceRecreate ? "Recreation queued." : "Update queued.",
      )
    ) {
      setTab("activity");
      setUpdateConfirm(false);
      setSkipConfirmation("");
    }
  }
  async function requestRestore(event: FormEvent) {
    event.preventDefault();
    if (!admin || !restoreBackup || restoreConfirmation !== server?.displayName)
      return;
    if (
      await perform(
        () =>
          apiJson(
            `${path}/restores`,
            jsonBody("POST", {
              backupId: restoreBackup.id,
              confirmation: restoreConfirmation,
            }),
          ),
        "Restore queued. The server stays stopped until its data is safe.",
      )
    ) {
      setRestoreBackup(null);
      setRestoreConfirmation("");
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
        <button className="text-link" onClick={() => navigate("/")}>
          ← Servers
        </button>
        <div className="alert alert--error" role="alert">
          {error || "Server unavailable."}
        </div>
      </div>
    );
  const tabs = [
    { id: "activity", label: "Activity" },
    ...(admin || can(user, server, "backups.create")
      ? [{ id: "backups", label: "Backups" }]
      : []),
    ...(can(user, server, "schedules.manage")
      ? [{ id: "schedules", label: "Schedules" }]
      : []),
    ...(admin
      ? [
          { id: "update", label: "Update" },
          { id: "availability", label: "Availability" },
        ]
      : []),
  ];
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
      <button className="text-link back-link" onClick={() => navigate("/")}>
        ← Servers
      </button>
      <div className="page__header page__header--actions">
        <div>
          <h1 className="page__title">{server.displayName}</h1>
          <p className="page__subtitle">{server.image}</p>
        </div>
        <div className={`server-state server-state--${server.state}`}>
          <span className="status-dot" />
          {server.state}
        </div>
      </div>
      <div className="inline-actions detail-actions">
        {(can(user, server, "logs.read") ||
          (can(user, server, "console.execute") && server.gameConsole) ||
          can(user, server, "console.shell")) && (
          <button
            className="secondary-btn"
            onClick={() => navigate(`/console/${server.id}`)}
          >
            Open console / logs
          </button>
        )}
        {can(user, server, "files.read") && server.fileRoots.length > 0 && (
          <button
            className="secondary-btn"
            onClick={() => navigate(`/files/${server.id}`)}
          >
            Browse files
          </button>
        )}
      </div>
      {blocked && (
        <div className="alert alert--error" role="alert">
          This server’s binding is {server.bindingStatus.replaceAll("_", " ")}.
          Operations are blocked until an administrator reviews the identity.
        </div>
      )}
      {admin && server.bindingStatus === "review_required" && (
        <form
          className="danger-panel stack-form"
          onSubmit={(event) => {
            event.preventDefault();
            void perform(
              () =>
                apiJson(
                  `${path}/binding-review`,
                  jsonBody("POST", { confirmation: bindingConfirmation }),
                ),
              "Server binding reviewed.",
            );
          }}
        >
          <h3>Review changed server identity</h3>
          <p>
            Verify that this is the same intended server and game data.
            Accepting the new binding re-enables existing user grants. Schedules
            for changed game data stay suspended; review and recreate them
            separately. Old backups remain subject to compatibility checks.
          </p>
          <label>
            <span>
              Type <strong>{server.displayName}</strong> to accept the changed
              binding
            </span>
            <input
              value={bindingConfirmation}
              onChange={(event) => setBindingConfirmation(event.target.value)}
              autoComplete="off"
            />
          </label>
          <button
            className="secondary-btn secondary-btn--danger"
            disabled={busy || bindingConfirmation !== server.displayName}
          >
            Accept binding
          </button>
        </form>
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
      <div
        className="section-tabs"
        role="tablist"
        aria-label="Server management"
      >
        {tabs.map((item) => (
          <button
            role="tab"
            id={`tab-${item.id}`}
            aria-controls={`panel-${item.id}`}
            aria-selected={tab === item.id}
            key={item.id}
            onClick={() => setTab(item.id)}
          >
            {item.label}
          </button>
        ))}
      </div>
      <section
        role="tabpanel"
        id={`panel-${tab}`}
        aria-labelledby={`tab-${tab}`}
      >
        {tab === "activity" && (
          <>
            <div className="section-heading">
              <h2>Recent operations</h2>
              <button
                className="secondary-btn"
                onClick={() =>
                  void refresh().catch((reason) => setError(String(reason)))
                }
              >
                Refresh
              </button>
            </div>
            <OperationList operations={operations} />
            {admin &&
              operations.some(
                (operation) =>
                  operation.kind === "update" &&
                  operation.status === "already_current",
              ) && (
                <p className="section-note">
                  The configured image is current. Game software may update
                  during startup.{" "}
                  <button
                    className="text-link"
                    onClick={() => {
                      setForceRecreate(true);
                      setUpdateConfirm(false);
                      setTab("update");
                    }}
                  >
                    Recreate anyway
                  </button>
                </p>
              )}
          </>
        )}
        {tab === "backups" &&
          (admin || can(user, server, "backups.create")) && (
            <>
              <div className="section-heading">
                <h2>Stopped-server backups</h2>
                {can(user, server, "backups.create") && (
                  <button
                    className="primary-btn"
                    onClick={() => void requestBackup()}
                    disabled={busy || blocked || Boolean(activeOperation)}
                  >
                    Create backup
                  </button>
                )}
              </div>
              <p className="section-note">
                Backups stop the server throughout copying, then restore its
                previous running state. Initially stopped servers stay stopped.
              </p>
              {admin ? (
                <div className="table-scroll">
                  <table className="data-table">
                    <thead>
                      <tr>
                        <th>Created</th>
                        <th>Size</th>
                        <th>State</th>
                        <th>Actions</th>
                      </tr>
                    </thead>
                    <tbody>
                      {backups.length === 0 && (
                        <tr>
                          <td colSpan={4} className="muted">
                            No backups yet. Configure the destination and limits
                            in Settings before the first backup.
                          </td>
                        </tr>
                      )}
                      {backups.map((backup) => (
                        <tr key={backup.id}>
                          <td>{new Date(backup.createdAt).toLocaleString()}</td>
                          <td>{formatSize(backup.size)}</td>
                          <td>{backup.state}</td>
                          <td>
                            <div className="inline-actions">
                              {backup.state === "complete" && (
                                <a
                                  className="secondary-btn"
                                  href={`/api/v1${path}/backups/${encodeURIComponent(backup.id)}/download`}
                                  download
                                >
                                  Download
                                </a>
                              )}
                              <button
                                className="secondary-btn secondary-btn--danger"
                                disabled={
                                  busy ||
                                  blocked ||
                                  Boolean(activeOperation) ||
                                  backup.state !== "complete"
                                }
                                onClick={() => {
                                  setRestoreBackup(backup);
                                  setRestoreConfirmation("");
                                }}
                              >
                                Restore…
                              </button>
                              <button
                                className="secondary-btn secondary-btn--danger"
                                disabled={busy || Boolean(activeOperation)}
                                onClick={() => {
                                  if (
                                    window.confirm(
                                      "Permanently delete this backup archive?",
                                    )
                                  )
                                    void perform(
                                      () =>
                                        apiJson(
                                          `${path}/backups/${encodeURIComponent(backup.id)}`,
                                          { method: "DELETE" },
                                        ),
                                      "Backup deleted.",
                                    );
                                }}
                              >
                                Delete
                              </button>
                            </div>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              ) : (
                <p className="section-note">
                  You can create backups using the administrator’s policy.
                  Archive access and restoration require an administrator.
                </p>
              )}
              {restoreBackup && (
                <form
                  className="danger-panel stack-form"
                  onSubmit={requestRestore}
                >
                  <h3>
                    Restore backup from{" "}
                    {new Date(restoreBackup.createdAt).toLocaleString()}
                  </h3>
                  <p>
                    Current game data will be replaced. Ludock stops the server,
                    validates the backup, and creates a safety backup before
                    replacement. If that backup cannot complete, restoration
                    aborts without replacing game data. It returns to its
                    previous running state only after the data is safe. A data
                    backup does not roll back the container image or Compose
                    settings.
                  </p>
                  <label>
                    <span>
                      Type <strong>{server.displayName}</strong> to confirm
                    </span>
                    <input
                      value={restoreConfirmation}
                      onChange={(event) =>
                        setRestoreConfirmation(event.target.value)
                      }
                      autoComplete="off"
                    />
                  </label>
                  <div className="inline-actions">
                    <button
                      className="secondary-btn secondary-btn--danger"
                      disabled={
                        busy || restoreConfirmation !== server.displayName
                      }
                    >
                      Restore game data
                    </button>
                    <button
                      type="button"
                      className="secondary-btn"
                      onClick={() => setRestoreBackup(null)}
                    >
                      Cancel
                    </button>
                  </div>
                </form>
              )}
            </>
          )}
        {tab === "schedules" && can(user, server, "schedules.manage") && (
          <>
            <h2>Schedules</h2>
            <p className="section-note">
              Schedules run in the selected time zone and only while their owner
              still has the required access.
            </p>
            <div className="table-scroll">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Action</th>
                    <th>When</th>
                    <th>State</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {schedules.length === 0 && (
                    <tr>
                      <td colSpan={4} className="muted">
                        No schedules.
                      </td>
                    </tr>
                  )}
                  {schedules.map((item) => (
                    <tr key={item.id}>
                      <td className="capitalize">{item.action}</td>
                      <td>
                        {item.time} ·{" "}
                        {item.days.map((day) => weekdays[day]).join(", ")}
                        <small className="table-detail">{item.timezone}</small>
                      </td>
                      <td>
                        {item.lastResult ||
                          (item.enabled ? "Enabled" : "Disabled")}
                      </td>
                      <td>
                        <button
                          className="secondary-btn secondary-btn--danger"
                          disabled={busy}
                          onClick={() => {
                            if (window.confirm("Delete this schedule?"))
                              void perform(
                                () =>
                                  apiJson(
                                    `${path}/schedules/${encodeURIComponent(item.id)}`,
                                    { method: "DELETE" },
                                  ),
                                "Schedule deleted.",
                              );
                          }}
                        >
                          Delete
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {scheduleActions.length > 0 ? (
              <form
                className="stack-form settings-section"
                onSubmit={addSchedule}
              >
                <h3>Add schedule</h3>
                <div className="form-columns">
                  <label>
                    Action
                    <select
                      value={
                        scheduleActions.includes(schedule.action)
                          ? schedule.action
                          : ""
                      }
                      onChange={(event) =>
                        setSchedule({
                          ...schedule,
                          action: event.target.value as ScheduleInput["action"],
                        })
                      }
                      required
                    >
                      <option value="" disabled>
                        Select action
                      </option>
                      {scheduleActions.map((action) => (
                        <option key={action} value={action}>
                          {action.charAt(0).toUpperCase() + action.slice(1)}
                        </option>
                      ))}
                    </select>
                  </label>
                  <label>
                    Time
                    <input
                      type="time"
                      required
                      value={schedule.time}
                      onChange={(event) =>
                        setSchedule({ ...schedule, time: event.target.value })
                      }
                    />
                  </label>
                  <label>
                    Time zone
                    <input
                      required
                      value={schedule.timezone}
                      onChange={(event) =>
                        setSchedule({
                          ...schedule,
                          timezone: event.target.value,
                        })
                      }
                    />
                  </label>
                </div>
                <fieldset className="weekday-input">
                  <legend>Days</legend>
                  {weekdays.map((day, index) => (
                    <label className="check-label" key={day}>
                      <input
                        type="checkbox"
                        checked={schedule.days.includes(index)}
                        onChange={(event) =>
                          setSchedule({
                            ...schedule,
                            days: event.target.checked
                              ? [...schedule.days, index].sort()
                              : schedule.days.filter(
                                  (value) => value !== index,
                                ),
                          })
                        }
                      />
                      {day}
                    </label>
                  ))}
                </fieldset>
                {schedule.action === "backup" && (
                  <p className="section-note">
                    Each backup stops the server for the copy and restores its
                    previous running state.
                  </p>
                )}
                <button
                  className="primary-btn"
                  disabled={
                    busy ||
                    blocked ||
                    schedule.days.length === 0 ||
                    !scheduleActions.includes(schedule.action)
                  }
                >
                  Add schedule
                </button>
              </form>
            ) : (
              <p className="section-note">
                No schedule actions are granted. An administrator must also
                grant the specific start, stop, restart, or backup action.
              </p>
            )}
          </>
        )}
        {tab === "update" && admin && (
          <>
            <h2>{capability?.actionLabel || "Update server"}</h2>
            {!capability?.available ? (
              <p className="section-note">
                {capability?.unavailableReason ||
                  "This server’s Compose project is not registered. Update it through its owning manager, or register the trusted project in Settings."}
              </p>
            ) : (
              <form className="stack-form" onSubmit={requestUpdate}>
                <p>
                  Pull the latest version of this service’s configured image and
                  recreate it through Docker Compose. The image’s normal startup
                  process may update the game software.
                </p>
                <dl className="metadata-list">
                  <dt>Project</dt>
                  <dd>{capability.projectName}</dd>
                  <dt>Service</dt>
                  <dd>{capability.serviceName}</dd>
                  <dt>Configured image</dt>
                  <dd>{capability.image}</dd>
                </dl>
                <p className="section-note">
                  Compose source is authoritative. Redeployment can replace
                  runtime changes that were never saved in that source. Other
                  services and dependencies are not recreated.
                </p>
                <label className="check-label">
                  <input
                    type="checkbox"
                    checked={createBackup}
                    onChange={(event) => {
                      setCreateBackup(event.target.checked);
                      setUpdateConfirm(false);
                    }}
                  />
                  Create a stopped-server backup before recreation
                </label>
                <p className="muted">
                  The server remains stopped between backup and recreation. A
                  previously stopped server stays stopped.
                </p>
                <label className="check-label">
                  <input
                    type="checkbox"
                    checked={forceRecreate}
                    onChange={(event) => {
                      setForceRecreate(event.target.checked);
                      setUpdateConfirm(false);
                    }}
                  />
                  Recreate anyway, even if the configured image is current
                </label>
                {forceRecreate && (
                  <p className="muted">
                    The service will be replaced even when its image is
                    unchanged. Startup-based game updates wait until the server
                    is started.
                  </p>
                )}
                {!createBackup && (
                  <label>
                    <span>
                      Type <strong>{server.displayName}</strong> to skip the
                      backup
                    </span>
                    <input
                      value={skipConfirmation}
                      onChange={(event) =>
                        setSkipConfirmation(event.target.value)
                      }
                      autoComplete="off"
                    />
                  </label>
                )}
                <label className="check-label">
                  <input
                    type="checkbox"
                    checked={updateConfirm}
                    onChange={(event) => setUpdateConfirm(event.target.checked)}
                  />
                  I understand that connected players will be disconnected and
                  the selected service will be recreated.
                </label>
                <button
                  className="primary-btn"
                  disabled={
                    busy ||
                    blocked ||
                    Boolean(activeOperation) ||
                    !updateConfirm ||
                    (!createBackup && skipConfirmation !== server.displayName)
                  }
                >
                  {forceRecreate ? "Recreate service" : capability.actionLabel}
                </button>
              </form>
            )}
          </>
        )}
        {tab === "availability" && admin && (
          <form
            className="stack-form"
            onSubmit={(event) => {
              event.preventDefault();
              void perform(
                () =>
                  apiJson(
                    `${path}/availability`,
                    jsonBody("PUT", {
                      enabled: availability.enabled,
                      maintenance: availability.maintenance,
                      graceSeconds: availability.graceSeconds,
                    }),
                  ),
                "Availability settings saved.",
              );
            }}
          >
            <h2>Availability monitoring</h2>
            <p>
              When enabled, this server is expected to be available 24/7. Ludock
              uses a supported game probe, Docker health, or running state.
              Stops initiated by Ludock and active operations suppress outage
              alerts.
            </p>
            <label className="check-label">
              <input
                type="checkbox"
                checked={availability.enabled}
                onChange={(event) =>
                  setAvailability({
                    ...availability,
                    enabled: event.target.checked,
                  })
                }
              />
              Monitor this server
            </label>
            <label>
              Failure grace period (seconds)
              <input
                type="number"
                min={10}
                max={86400}
                required
                value={availability.graceSeconds}
                onChange={(event) =>
                  setAvailability({
                    ...availability,
                    graceSeconds: Number(event.target.value),
                  })
                }
              />
            </label>
            <label className="check-label">
              <input
                type="checkbox"
                checked={availability.maintenance}
                onChange={(event) =>
                  setAvailability({
                    ...availability,
                    maintenance: event.target.checked,
                  })
                }
              />
              Maintenance mode — pause monitoring
            </label>
            <p className="muted">
              One notification is sent for an outage and one for recovery.
              Configure Discord delivery in Settings.
            </p>
            <button className="primary-btn" disabled={busy}>
              Save monitoring
            </button>
          </form>
        )}
      </section>
    </div>
  );
}
