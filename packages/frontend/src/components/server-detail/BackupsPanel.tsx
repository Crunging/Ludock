import { type Backup, type BackupPreflight, type Server, formatByteSize } from "@ludock/shared";
import { NavLink } from "../../navigation";

export interface RestoreSelection {
  backup: Backup | null;
  confirmation: string;
}

interface Props {
  serverName: string;
  path: string;
  backups: Backup[];
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
      <p className="section-note">
        Backups stop the server throughout copying, then restore its previous
        running state. Initially stopped servers stay stopped.
        {admin && (
          <>
            {" "}Manage the destination and storage limits in{" "}
            <NavLink className="text-link" to="/settings">backup settings</NavLink>.
          </>
        )}
      </p>
      {(admin || canRead || canCreate) && (
        <p className="section-note">
          <strong>Latest successful backup: </strong>
          {latestBackup ? <>
            <time dateTime={new Date(latestBackup.createdAt).toISOString()}>
              {new Date(latestBackup.createdAt).toLocaleString()}
            </time>{" · "}{formatByteSize(latestBackup.size)}
          </> : "No successful backup retained."}
        </p>
      )}
      {canCreate && !blocked && !hasActiveOperation && (
        <section className="backup-readiness" aria-labelledby="backup-readiness-title">
          <div className="section-heading">
            <h3 id="backup-readiness-title">Backup readiness</h3>
            <button className="secondary-btn" disabled={checking || busy} onClick={onCheck}>
              Check again
            </button>
          </div>
          {checking && <p role="status">Checking backup destination and data roots…</p>}
          {preflightError && <p className="alert alert--error" role="alert">{preflightError}</p>}
          {preflight && <>
            <p><strong>{preflight.ready ? "Preflight checks passed." : "Resolve these problems before creating a backup:"}</strong></p>
            {preflight.issues.length > 0 && <ul>
              {preflight.issues.map((issue, index) => <li key={`${issue.code}-${index}`}>{issue.message}</li>)}
            </ul>}
            {!preflight.ready && <p className="section-note">
              {admin ? "Review backup settings and this server’s mounted data roots, then check again." : "Ask an administrator to resolve these problems, then check again."}
            </p>}
          </>}
          <p className="section-note">
            This check does not copy data or stop the server. Available space and
            data can change; Ludock validates them again when the backup runs.
            The full data check happens during copying.
          </p>
        </section>
      )}
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
                    No backups yet. Configure backup settings if needed, then
                    create a backup to save this server’s game data.
                  </td>
                </tr>
              )}
              {backups.map((backup) => (
                <tr key={backup.id}>
                  <td>{new Date(backup.createdAt).toLocaleString()}</td>
                  <td>{formatByteSize(backup.size)}</td>
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
                          !canRestore ||
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
                        disabled={!canDelete || busy || hasActiveOperation}
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
            Restore backup from{" "}
            {new Date(restoreBackup.createdAt).toLocaleString()}
          </h3>
          <p>
            Current game data will be replaced. Ludock stops the server,
            validates the backup, and creates a safety backup before
            replacement. If that backup cannot complete, restoration aborts
            without replacing game data. It returns to its previous running
            state only after the data is safe. A data backup does not roll back
            the container image or Compose settings.
          </p>
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
                busy || blocked || hasActiveOperation ||
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
