import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  type BackupSettings,
  type ComposeProject,
  backupSettingsResponseSchema,
  composeProjectsResponseSchema,
  composeProjectResponseSchema,
  notificationSettingsResponseSchema,
  okResponseSchema,
} from "@ludock/shared";
import { apiJson, jsonBody } from "../api";

const gib = 1024 ** 3;
export default function Settings() {
  const [backup, setBackup] = useState<BackupSettings>({
    destination: "",
    retentionCount: 10,
    maxBytes: 100 * gib,
    reserveBytes: 5 * gib,
  });
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
  const saving = useRef(false);
  const active = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    active.current = true;
    setLoading(true);
    setLoaded(false);
    setError(null);
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
          setBackup(backupResponse.settings);
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
    action: () => Promise<T>,
    message: string,
    apply: (result: T) => void,
  ) {
    if (!loaded || saving.current) return;
    saving.current = true;
    setBusy(true);
    setError(null);
    setNotice(null);
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
    const submitted = backup;
    await save(
      () => apiJson(
        "/settings/backups",
        backupSettingsResponseSchema,
        jsonBody("PUT", submitted),
      ),
      "Backup settings saved.",
      ({ settings }) => {
        setBackupConfigured(Boolean(settings));
        if (settings)
          setBackup((current) => current === submitted ? settings : current);
      },
    );
  }
  async function registerProject(event: FormEvent) {
    event.preventDefault();
    const lines = (value: string) =>
      value.split("\n").map((line) => line.trim()).filter(Boolean);
    await save(
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
  return (
    <div className="page settings-page">
      <div className="page__header">
        <h1 className="page__title">Settings</h1>
        <p className="page__subtitle">
          Storage, trusted Compose projects, and notification delivery.
        </p>
      </div>
      {error && (
        <div className="alert alert--error" role="alert">
          {error}
        </div>
      )}
      {notice && (
        <div className="alert alert--success" role="status">
          {notice}
        </div>
      )}
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
              <label>
                Mounted destination path
                <input
                  value={backup.destination}
                  onChange={(event) =>
                    setBackup({ ...backup, destination: event.target.value })
                  }
                  placeholder="/backups"
                  required
                />
              </label>
              <div className="form-columns">
                <label>
                  Retain per server
                  <input
                    type="number"
                    min={1}
                    max={1000}
                    value={backup.retentionCount}
                    onChange={(event) =>
                      setBackup({
                        ...backup,
                        retentionCount: Number(event.target.value),
                      })
                    }
                    required
                  />
                </label>
                <label>
                  Global limit (GiB)
                  <input
                    type="number"
                    min={0.01}
                    step="0.01"
                    value={backup.maxBytes / gib}
                    onChange={(event) =>
                      setBackup({
                        ...backup,
                        maxBytes: Math.round(Number(event.target.value) * gib),
                      })
                    }
                    required
                  />
                </label>
                <label>
                  Keep free (GiB)
                  <input
                    type="number"
                    min={0}
                    step="0.01"
                    value={backup.reserveBytes / gib}
                    onChange={(event) =>
                      setBackup({
                        ...backup,
                        reserveBytes: Math.round(
                          Number(event.target.value) * gib,
                        ),
                      })
                    }
                    required
                  />
                </label>
              </div>
              <p className="muted">
                The destination must be inside an approved backup root and
                separate from game data. Space is also required for temporary
                archives and restore staging.
              </p>
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
                              void save(() => apiJson(
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
              <div className="form-columns">
                <label>
                  Compose project name
                  <input
                    value={projectName}
                    onChange={(event) => setProjectName(event.target.value)}
                    pattern="[a-z0-9][a-z0-9_-]*"
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
                    required
                  />
                </label>
              </div>
              <label>
                Compose files — one path per line, in merge order
                <textarea
                  value={composeFiles}
                  onChange={(event) => setComposeFiles(event.target.value)}
                  placeholder={
                    "/compose/my-project/compose.yaml\n/compose/my-project/compose.override.yaml"
                  }
                  rows={3}
                  required
                />
              </label>
              <label>
                CLI environment files — one path per line, in order
                <textarea
                  value={envFiles}
                  onChange={(event) => setEnvFiles(event.target.value)}
                  rows={2}
                />
              </label>
              <p className="muted">
                All files and transitive reads must be within the deployment’s
                approved Compose roots. Source validation can reject unsupported
                Compose features. Enter file paths only; file contents and
                credentials are never shown here.
              </p>
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
