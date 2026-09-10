import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  type BackupSettings,
  type ComposeProject,
  type DeploymentSettings,
  backupSettingsSchema,
  backupSettingsResponseSchema,
  composeProjectsResponseSchema,
  composeProjectResponseSchema,
  notificationSettingsResponseSchema,
  okResponseSchema,
  deploymentSettingsResponseSchema,
} from "@ludock/shared";
import { apiJson, jsonBody } from "../api";
import { NavLink } from "../navigation";
import DeploymentGuidance from "../components/DeploymentGuidance";
import "../styles/admin-setup.css";

const gib = 1024 ** 3;
type SettingsSection = "backups" | "projects" | "registration" | "notifications";

interface BackupDraft {
  destination: string;
  retentionCount: string;
  maxGiB: string;
  reserveGiB: string;
}

function backupDraft(settings: BackupSettings): BackupDraft {
  return {
    destination: settings.destination,
    retentionCount: String(settings.retentionCount),
    // Keep the full value so saving another field preserves the exact byte limit.
    maxGiB: String(settings.maxBytes / gib),
    reserveGiB: String(settings.reserveBytes / gib),
  };
}

export default function Settings() {
  const [backup, setBackup] = useState(() =>
    backupDraft({
      destination: "",
      retentionCount: 10,
      maxBytes: 100 * gib,
      reserveBytes: 5 * gib,
    }),
  );
  const [backupConfigured, setBackupConfigured] = useState(false);
  const [projects, setProjects] = useState<ComposeProject[]>([]);
  const [projectName, setProjectName] = useState("");
  const [projectDirectory, setProjectDirectory] = useState("");
  const [composeFiles, setComposeFiles] = useState("");
  const [envFiles, setEnvFiles] = useState("");
  const [webhookUrl, setWebhookUrl] = useState("");
  const [notificationConfigured, setNotificationConfigured] = useState(false);
  const [notificationEnabled, setNotificationEnabled] = useState(false);
  const [loading, setLoading] = useState(true);
  const [loaded, setLoaded] = useState(false);
  const [loadAttempt, setLoadAttempt] = useState(0);
  const [deployment, setDeployment] = useState<DeploymentSettings | null>(null);
  const [deploymentLoading, setDeploymentLoading] = useState(true);
  const [deploymentError, setDeploymentError] = useState<string | null>(null);
  const [deploymentAttempt, setDeploymentAttempt] = useState(0);
  const backupDestination = useRef<HTMLInputElement>(null);
  const saving = useRef(false);
  const active = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [feedbackSection, setFeedbackSection] = useState<SettingsSection | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    setDeploymentLoading(true);
    setDeploymentError(null);
    apiJson("/settings/deployment", deploymentSettingsResponseSchema, {
      signal: controller.signal,
    })
      .then((response) => {
        if (!controller.signal.aborted) setDeployment(response);
      })
      .catch((reason) => {
        if (!controller.signal.aborted)
          setDeploymentError(reason instanceof Error ? reason.message : "Unable to load deployment guidance.");
      })
      .finally(() => {
        if (!controller.signal.aborted) setDeploymentLoading(false);
      });
    return () => controller.abort();
  }, [deploymentAttempt]);
  useEffect(() => {
    const controller = new AbortController();
    active.current = true;
    setLoading(true);
    setLoaded(false);
    setError(null);
    setFeedbackSection(null);
    Promise.all([
      apiJson("/settings/backups", backupSettingsResponseSchema, {
        signal: controller.signal,
      }),
      apiJson("/compose-projects", composeProjectsResponseSchema, {
        signal: controller.signal,
      }),
      apiJson("/notifications", notificationSettingsResponseSchema, {
        signal: controller.signal,
      }),
    ])
      .then(([backupResponse, projectResponse, notificationResponse]) => {
        if (controller.signal.aborted) return;
        if (backupResponse.settings) {
          setBackup(backupDraft(backupResponse.settings));
          setBackupConfigured(true);
        }
        setProjects(projectResponse.projects);
        setNotificationConfigured(notificationResponse.configured);
        setNotificationEnabled(notificationResponse.enabled);
        setLoaded(true);
      })
      .catch((reason) => {
        if (!controller.signal.aborted)
          setError(
            reason instanceof Error ? reason.message : "Unable to load settings.",
          );
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => {
      active.current = false;
      controller.abort();
    };
  }, [loadAttempt]);
  async function save<T,>(
    section: SettingsSection,
    action: () => Promise<T>,
    message: string,
    apply: (result: T) => void,
  ) {
    if (!loaded || saving.current) return;
    saving.current = true;
    setBusy(true);
    setError(null);
    setNotice(null);
    setFeedbackSection(section);
    try {
      const result = await action();
      if (!active.current) return;
      apply(result);
      setNotice(message);
    } catch (reason) {
      if (active.current)
        setError(
          reason instanceof Error ? reason.message : "Unable to save settings.",
        );
    } finally {
      saving.current = false;
      if (active.current) setBusy(false);
    }
  }
  async function saveBackup(event: FormEvent) {
    event.preventDefault();
    if (!loaded || saving.current) return;
    setFeedbackSection("backups");
    if (
      [backup.retentionCount, backup.maxGiB, backup.reserveGiB].some(
        (value) => !value.trim(),
      )
    ) {
      setError("Complete all backup limits before saving.");
      setNotice(null);
      return;
    }
    const parsed = backupSettingsSchema.safeParse({
      destination: backup.destination,
      retentionCount: Number(backup.retentionCount),
      maxBytes: Math.round(Number(backup.maxGiB) * gib),
      reserveBytes: Math.round(Number(backup.reserveGiB) * gib),
    });
    if (!parsed.success) {
      setError(
        "Check the backup settings: enter a destination, 1–1,000 backups per server, a positive total limit, and a free-space reserve of zero or more.",
      );
      setNotice(null);
      return;
    }
    const submittedDraft = backup;
    await save(
      "backups",
      () => apiJson(
        "/settings/backups",
        backupSettingsResponseSchema,
        jsonBody("PUT", parsed.data),
      ),
      "Backup settings saved.",
      ({ settings }) => {
        setBackupConfigured(Boolean(settings));
        if (!settings) return;
        const savedDraft = backupDraft(settings);
        if (settings.maxBytes === parsed.data.maxBytes)
          savedDraft.maxGiB = submittedDraft.maxGiB;
        if (settings.reserveBytes === parsed.data.reserveBytes)
          savedDraft.reserveGiB = submittedDraft.reserveGiB;
        setBackup((current) =>
          current === submittedDraft ? savedDraft : current,
        );
      },
    );
  }
  async function registerProject(event: FormEvent) {
    event.preventDefault();
    const lines = (value: string) =>
      value.split("\n").map((line) => line.trim()).filter(Boolean);
    await save(
      "registration",
      () => apiJson(
        "/compose-projects",
        composeProjectResponseSchema,
        jsonBody("POST", {
          projectName,
          projectDirectory,
          composeFiles: lines(composeFiles),
          envFiles: lines(envFiles),
        }),
      ),
      "Compose project registered.",
      ({ project }) => {
        setProjects((current) => [
          ...current.filter((item) => item.id !== project.id),
          project,
        ]);
        setProjectName((current) => current === projectName ? "" : current);
        setProjectDirectory((current) => current === projectDirectory ? "" : current);
        setComposeFiles((current) => current === composeFiles ? "" : current);
        setEnvFiles((current) => current === envFiles ? "" : current);
      },
    );
  }
  async function saveNotifications(event: FormEvent) {
    event.preventDefault();
    await save(
      "notifications",
      () => apiJson(
        "/notifications",
        notificationSettingsResponseSchema,
        jsonBody("PUT", {
          enabled: notificationEnabled,
          ...(webhookUrl.trim() ? { webhookUrl: webhookUrl.trim() } : {}),
        }),
      ),
      "Notification settings saved.",
      (response) => {
        setNotificationConfigured(response.configured);
        setNotificationEnabled((current) =>
          current === notificationEnabled ? response.enabled : current,
        );
        setWebhookUrl((current) => current === webhookUrl ? "" : current);
      },
    );
  }
  function feedbackFor(section: SettingsSection | null) {
    if (feedbackSection !== section) return null;
    return (
      <>
        {error && <div className="alert alert--error" role="alert">{error}</div>}
        {notice && (
          <div className="alert alert--success admin-settings-feedback" role="status">
            <p>{notice}</p>
            {section === "backups" && <NavLink to="/">Choose a server and open Backups to create a backup.</NavLink>}
            {section === "registration" && <NavLink to="/">Choose a server and open Update to use this registration.</NavLink>}
          </div>
        )}
      </>
    );
  }
  return (
    <div className="page settings-page">
      <div className="page__header">
        <h1 className="page__title">Settings</h1>
        <p className="page__subtitle">
          Storage, trusted Compose projects, and notification delivery.
        </p>
      </div>
      {feedbackFor(null)}
      {loading ? (
        <p className="muted" role="status">
          Loading settings…
        </p>
      ) : !loaded ? (
        <button
          className="secondary-btn"
          onClick={() => setLoadAttempt((value) => value + 1)}
        >
          Try again
        </button>
      ) : (
        <>
          {deploymentLoading && <p className="muted" role="status">Loading setup guidance…</p>}
          {deploymentError && (
            <div className="admin-setup-help">
              <p>Setup guidance is unavailable: {deploymentError} Your settings can still be edited below.</p>
              <button className="secondary-btn" onClick={() => setDeploymentAttempt((value) => value + 1)}>Reload setup guidance</button>
            </div>
          )}
          <section
            className="settings-section settings-section--divided"
            aria-labelledby="backup-settings-title"
          >
            <form className="stack-form" onSubmit={saveBackup}>
              <h2 id="backup-settings-title">Backup storage</h2>
              <p>
                {backupConfigured
                  ? "Backups use the configured destination and limits."
                  : "Backups are disabled until an approved mounted destination and limits are saved."}{" "}
                Every backup stops its server for the entire copy.
              </p>
              {deployment && !deploymentLoading && !deploymentError && (
                <DeploymentGuidance
                  section="backups"
                  deployment={deployment}
                  onUseBackupRoot={(destination) => {
                    setBackup((current) => ({ ...current, destination }));
                    backupDestination.current?.focus();
                  }}
                />
              )}
              <label>
                Mounted destination path
                <input
                  ref={backupDestination}
                  value={backup.destination}
                  onChange={(event) =>
                    setBackup({ ...backup, destination: event.target.value })
                  }
                  placeholder="/backups"
                  required
                />
              </label>
              <div className="form-columns">
                <div>
                  <label>
                    Backups per server
                    <input
                      type="number"
                      min={1}
                      max={1000}
                      inputMode="numeric"
                      value={backup.retentionCount}
                      aria-describedby="backup-retention-help"
                      onChange={(event) =>
                        setBackup({
                          ...backup,
                          retentionCount: event.target.value,
                        })
                      }
                      required
                    />
                  </label>
                  <p className="muted" id="backup-retention-help">
                    Older backups are removed after a new backup succeeds.
                  </p>
                </div>
                <div>
                  <label>
                    Total backup limit (GiB)
                    <input
                      type="number"
                      min={1 / gib}
                      step="any"
                      inputMode="decimal"
                      value={backup.maxGiB}
                      aria-describedby="backup-limit-help"
                      onChange={(event) =>
                        setBackup({
                          ...backup,
                          maxGiB: event.target.value,
                        })
                      }
                      required
                    />
                  </label>
                  <p className="muted" id="backup-limit-help">
                    Combined size of backups across all servers. Leave room for
                    the next backup before older backups are removed.
                  </p>
                </div>
                <div>
                  <label>
                    Minimum free space (GiB)
                    <input
                      type="number"
                      min={0}
                      step="any"
                      inputMode="decimal"
                      value={backup.reserveGiB}
                      aria-describedby="backup-reserve-help"
                      onChange={(event) =>
                        setBackup({
                          ...backup,
                          reserveGiB: event.target.value,
                        })
                      }
                      required
                    />
                  </label>
                  <p className="muted" id="backup-reserve-help">
                    Space to leave free on the backup disk and on game-data disks
                    during restores. Use 0 for no reserve.
                  </p>
                </div>
              </div>
              <p className="muted">
                The destination must be inside an approved backup root and
                separate from game data. Space is also required for temporary
                archives and restore staging.
              </p>
              {feedbackFor("backups")}
              <button className="primary-btn" disabled={busy}>
                Save backup settings
              </button>
            </form>
          </section>
          <section
            className="settings-section settings-section--divided"
            aria-labelledby="compose-settings-title"
          >
            <h2 id="compose-settings-title">Trusted Compose projects</h2>
            <p className="section-note">
              Register existing read-only source files to enable service
              updates. Ludock never edits the source. Projects that are
              inaccessible here remain managed through Portainer, Dockge, or
              their owning manager.
            </p>
            {deployment && !deploymentLoading && !deploymentError && (
              <DeploymentGuidance section="compose" deployment={deployment} />
            )}
            {feedbackFor("projects")}
            <div className="table-scroll">
              <table className="data-table">
                <thead>
                  <tr>
                    <th>Project</th>
                    <th>Source files in order</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {projects.length === 0 && (
                    <tr>
                      <td colSpan={3} className="muted">
                        No registered projects.
                      </td>
                    </tr>
                  )}
                  {projects.map((project) => (
                    <tr key={project.id}>
                      <td>
                        {project.projectName}
                        <small className="table-detail">
                          {project.projectDirectory}
                        </small>
                        {project.disabled && (
                          <small className="table-detail">Disabled</small>
                        )}
                      </td>
                      <td>
                        {project.composeFiles.map((file) => (
                          <code className="table-detail" key={file}>
                            {file}
                          </code>
                        ))}
                      </td>
                      <td>
                        <button
                          className="secondary-btn secondary-btn--danger"
                          disabled={busy}
                          onClick={() => {
                            if (
                              window.confirm(
                                `Unregister ${project.projectName}? This disables Ludock updates for its services; it does not stop or delete containers.`,
                              )
                            )
                              void save("projects", () => apiJson(
                                `/compose-projects/${encodeURIComponent(project.id)}`,
                                okResponseSchema,
                                { method: "DELETE" },
                              ), "Project unregistered.", () => {
                                setProjects((current) => current.filter((value) => value.id !== project.id));
                              });
                          }}
                        >
                          Unregister
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <form
              className="stack-form settings-section"
              onSubmit={registerProject}
            >
              <h3>Register project</h3>
              <p className="muted">Use the project that already runs your servers. Find its exact project name with <code>docker compose ls</code> on the Docker host or in its owning manager.</p>
              <div className="form-columns">
                <label>
                  Compose project name
                  <input
                    value={projectName}
                    onChange={(event) => setProjectName(event.target.value)}
                    pattern={"[a-z0-9][a-z0-9_\\-]*"}
                    maxLength={128}
                    autoCapitalize="none"
                    spellCheck={false}
                    title="Use lowercase letters, numbers, hyphens, or underscores, starting with a letter or number."
                    required
                  />
                </label>
                <label>
                  Project directory
                  <input
                    value={projectDirectory}
                    onChange={(event) =>
                      setProjectDirectory(event.target.value)
                    }
                    placeholder="/compose/my-project"
                    aria-describedby="compose-directory-help"
                    required
                  />
                </label>
              </div>
              <p className="muted" id="compose-directory-help">The project directory is its existing folder on the Docker host. Mount that folder into Ludock read-only at the same absolute path.</p>
              <label>
                Compose files — one path per line, in merge order
                <textarea
                  value={composeFiles}
                  onChange={(event) => setComposeFiles(event.target.value)}
                  placeholder={
                    "/compose/my-project/compose.yaml\n/compose/my-project/compose.override.yaml"
                  }
                  rows={3}
                  aria-describedby="compose-files-help"
                  required
                />
              </label>
              <p className="muted" id="compose-files-help">Usually this is one <code>compose.yaml</code> file. Add any override files after the base file, in the same order used to start the project.</p>
              <label>
                CLI environment files (optional) — one path per line, in order
                <textarea
                  value={envFiles}
                  onChange={(event) => setEnvFiles(event.target.value)}
                  rows={2}
                  aria-describedby="compose-env-help"
                />
              </label>
              <p className="muted" id="compose-env-help">List any <code>.env</code> or <code>--env-file</code> files this project needs. Ludock does not load <code>.env</code> automatically.</p>
              <p className="muted">
                All files and transitive reads must be within the deployment’s
                approved Compose roots. Source validation can reject unsupported
                Compose features. Enter file paths only; file contents and
                credentials are never shown here.
              </p>
              {feedbackFor("registration")}
              <button className="primary-btn" disabled={busy}>
                Validate and register
              </button>
            </form>
          </section>
          <section
            className="settings-section settings-section--divided"
            aria-labelledby="notification-settings-title"
          >
            <form className="stack-form" onSubmit={saveNotifications}>
              <h2 id="notification-settings-title">Discord notifications</h2>
              <p>
                Receive outage and recovery alerts, backup or schedule failures,
                and restore and update results. Enable availability monitoring
                separately for each server.
              </p>
              <label className="check-label">
                <input
                  type="checkbox"
                  checked={notificationEnabled}
                  onChange={(event) =>
                    setNotificationEnabled(event.target.checked)
                  }
                />
                Enable Discord delivery
              </label>
              <label>
                {notificationConfigured ? "Replace webhook URL" : "Webhook URL"}
                <input
                  type="password"
                  value={webhookUrl}
                  onChange={(event) => setWebhookUrl(event.target.value)}
                  autoComplete="new-password"
                  placeholder={
                    notificationConfigured
                      ? "Saved — leave blank to keep existing URL"
                      : "https://discord.com/api/webhooks/…"
                  }
                />
              </label>
              <p className="muted">
                The saved webhook is write-only. It is never returned to the
                browser.
              </p>
              {feedbackFor("notifications")}
              <button
                className="primary-btn"
                disabled={
                  busy ||
                  (notificationEnabled &&
                    !notificationConfigured &&
                    !webhookUrl.trim())
                }
              >
                Save notifications
              </button>
            </form>
          </section>
        </>
      )}
    </div>
  );
}
