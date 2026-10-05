import { type Backup, type BackupPreflight, type Server, formatByteSize } from "@ludock/shared";
import { NavLink } from "../../navigation";
import { formatDateTime, formatRelativeTime, formatRelativeTimeSentence } from "../../format";
import StatusPip from "../StatusPip";

export interface RestoreSelection {
  backup: Backup | null;
  confirmation: string;
}

interface Props {
  serverName: string;
  path: string;
  backups: Backup[];
  historyLoading?: boolean;
  historyUnavailable?: boolean;
  latestBackup: Server["latestBackup"];
  preflight: BackupPreflight | null;
  checking: boolean;
  preflightError: string | null;
  onCheck: () => void;
  restore: RestoreSelection;
  onRestoreChange: (value: RestoreSelection) => void;
  admin: boolean;
  canRead: boolean;
  canCreate: boolean;
  canRestore: boolean;
  canDelete: boolean;
  busy: boolean;
  blocked: boolean;
  hasActiveOperation: boolean;
  onCreate: () => void;
  onDelete: (backup: Backup) => void;
  onRestore: () => void;
}

export default function BackupsPanel(props: Props) {
  const {
    serverName,
    path,
    backups,
    latestBackup,
    preflight,
    checking,
    preflightError,
    onCheck,
    restore,
    onRestoreChange,
    admin,
    canRead,
    canCreate,
    canRestore,
    canDelete,
    busy,
    blocked,
    hasActiveOperation,
    onCreate,
    onDelete,
    onRestore,
  } = props;
  const historyReady = !props.historyLoading && !props.historyUnavailable;
  const { backup: restoreBackup, confirmation: restoreConfirmation } = restore;
  return (
    <>
      <div className="section-heading">
        <h2>Backups</h2>
        {canCreate && (
          <button
            className="primary-btn"
            onClick={() => onCreate()}
            disabled={busy || blocked || hasActiveOperation || checking || !preflight?.ready}
          >
            Create backup
          </button>
        )}
      </div>
      <p className="section-lede">
        The server stops while a backup copies, then goes back to how it was.
      </p>
      {canCreate && !blocked && !hasActiveOperation && (
        <section
          className={`backup-readiness${preflight && !checking ? (preflight.ready ? " backup-readiness--ready" : " backup-readiness--blocked") : ""}`}
          aria-labelledby="backup-readiness-title"
        >
          <div className="backup-readiness__status">
            <h3 id="backup-readiness-title" className="sr-only">Backup readiness</h3>
            {checking ? (
              <p role="status"><StatusPip tone="active" />Checking backup destination and data roots…</p>
            ) : preflight ? (
              <p><StatusPip tone={preflight.ready ? "ok" : "attention"} /><strong>
                {preflight.ready ? "Ready to back up." : "Not ready to back up."}
              </strong></p>
            ) : null}
            <button className="text-link" disabled={checking || busy} onClick={onCheck}>
              Check again
            </button>
          </div>
          {preflightError && <p className="alert alert--error" role="alert">{preflightError}</p>}
          {preflight && !checking && preflight.issues.length > 0 && <ul>
            {preflight.issues.map((issue, index) => <li key={`${issue.code}-${index}`}>{issue.message}</li>)}
          </ul>}
          {preflight && !checking && !preflight.ready && <p className="muted">
            {admin ? <>Fix these in <NavLink className="text-link" to="/settings">backup settings</NavLink> or the server’s mounts, then check again.</> : "Ask an administrator to fix these, then check again."}
          </p>}
        </section>
      )}
      {!(admin && canRead) && (
        <p className="section-note">
          Last backup:{" "}
          {latestBackup ? <>
            <time dateTime={new Date(latestBackup.createdAt).toISOString()} title={formatDateTime(latestBackup.createdAt)}>
              {formatRelativeTime(latestBackup.createdAt)}
            </time>{" ("}{formatByteSize(latestBackup.size)}{")"}
          </> : "never"}
        </p>
      )}
      <details className="disclosure">
        <summary>How backups work</summary>
        <p>
          A backup stops the server for the whole copy, then restores its previous
          running state. A server that was already stopped stays stopped.
        </p>
        <p>
          The readiness check doesn’t copy anything or stop the server. Space and
          data can change before the backup runs, so Ludock checks them again then.
        </p>
        {admin && (
          <p>
            Choose where backups go and how many to keep in
            {" "}<NavLink className="text-link" to="/settings">backup settings</NavLink>.
          </p>
        )}
      </details>
      {admin && canRead ? (
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
                    {props.historyLoading ? "Loading backups…" : props.historyUnavailable
                      ? "Backup history is unavailable."
                      : "No backups yet."}
                  </td>
                </tr>
              )}
              {backups.map((backup) => (
                <tr key={backup.id}>
                  <td>
                    <time dateTime={new Date(backup.createdAt).toISOString()}>{formatDateTime(backup.createdAt)}</time>
                    <small className="table-detail">{formatRelativeTimeSentence(backup.createdAt)}</small>
                  </td>
                  <td>{formatByteSize(backup.size)}</td>
                  <td><StatusPip tone={backup.state === "complete" ? "ok" : "failed"} />{backup.state}</td>
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
                          !canRestore || !historyReady ||
                          busy ||
                          blocked ||
                          hasActiveOperation ||
                          backup.state !== "complete"
                        }
                        onClick={() => {
                          onRestoreChange({ backup, confirmation: "" });
                        }}
                      >
                        Restore…
                      </button>
                      <button
                        className="secondary-btn secondary-btn--danger"
                        disabled={!canDelete || !historyReady || busy || hasActiveOperation}
                        onClick={() => {
                          if (
                            window.confirm(
                              "Permanently delete this backup archive?",
                            )
                          )
                            onDelete(backup);
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
          {admin
            ? "Archive access is unavailable for this server."
            : "You can create backups using the administrator’s policy. Archive access and restoration require an administrator."}
        </p>
      )}
      {admin && canRead && canRestore && restoreBackup && (
        <form
          className="danger-panel stack-form"
          onSubmit={(event) => {
            event.preventDefault();
            onRestore();
          }}
        >
          <h3>
            Restore backup from {formatDateTime(restoreBackup.createdAt)}
          </h3>
          <p>
            This replaces the server’s current game data. Ludock stops the
            server and takes a safety backup first; if that fails, nothing is
            replaced.
          </p>
          <details className="disclosure">
            <summary>More about restoring</summary>
            <p>
              Ludock checks the backup before replacing anything. The server
              returns to its previous running state only after the data is safe.
              Restoring game data doesn’t roll back the container image or
              Compose settings.
            </p>
          </details>
          <label>
            <span>
              Type <strong>{serverName}</strong> to confirm
            </span>
            <input
              value={restoreConfirmation}
              onChange={(event) =>
                onRestoreChange({
                  ...restore,
                  confirmation: event.target.value,
                })
              }
              autoComplete="off"
            />
          </label>
          <div className="inline-actions">
            <button
              className="secondary-btn secondary-btn--danger"
              disabled={
                busy || !historyReady || blocked || hasActiveOperation ||
                restoreConfirmation !== serverName ||
                !backups.some((item) =>
                  item.id === restoreBackup.id && item.state === "complete",
                )
              }
            >
              Restore game data
            </button>
            <button
              type="button"
              className="secondary-btn"
              onClick={() => onRestoreChange({ ...restore, backup: null })}
            >
              Cancel
            </button>
          </div>
        </form>
      )}
    </>
  );
}
