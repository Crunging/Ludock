import type { Backup } from "@ludock/shared";

export interface RestoreSelection {
  backup: Backup | null;
  confirmation: string;
}
const formatSize = (size: number) =>
  size >= 1024 ** 3
    ? `${(size / 1024 ** 3).toFixed(2)} GiB`
    : `${(size / 1024 ** 2).toFixed(1)} MiB`;

interface Props {
  serverName: string;
  path: string;
  backups: Backup[];
  restore: RestoreSelection;
  onRestoreChange: (value: RestoreSelection) => void;
  admin: boolean;
  canCreate: boolean;
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
    restore,
    onRestoreChange,
    admin,
    canCreate,
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
        <h2>Stopped-server backups</h2>
        {canCreate && (
          <button
            className="primary-btn"
            onClick={() => onCreate()}
            disabled={busy || blocked || hasActiveOperation}
          >
            Create backup
          </button>
        )}
      </div>
      <p className="section-note">
        Backups stop the server throughout copying, then restore its previous
        running state. Initially stopped servers stay stopped.
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
                    No backups yet. Configure the destination and limits in
                    Settings before the first backup.
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
                        disabled={busy || hasActiveOperation}
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
          You can create backups using the administrator’s policy. Archive
          access and restoration require an administrator.
        </p>
      )}
      {restoreBackup && (
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
              disabled={busy || restoreConfirmation !== serverName}
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
