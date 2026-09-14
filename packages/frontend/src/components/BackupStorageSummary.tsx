import { backupStorageResponseSchema, formatByteSize } from "@ludock/shared";
import { apiJson } from "../api";
import { usePageRead } from "../hooks/usePageRead";
import "../styles/backup-storage.css";

const readStorage = (signal: AbortSignal) =>
  apiJson("/settings/backups/status", backupStorageResponseSchema, { signal });

export default function BackupStorageSummary() {
  const { data, loading, error, refresh } = usePageRead(readStorage, "Unable to load backup storage.");
  const storage = data?.storage;

  return (
    <section className="backup-storage" aria-labelledby="backup-storage-summary-title">
      <div className="backup-storage__header">
        <h3 id="backup-storage-summary-title">Current storage</h3>
        <button type="button" className="secondary-btn" disabled={loading} onClick={() => void refresh()}>
          Refresh storage
        </button>
      </div>
      <p className="muted">Uses saved settings. Unsaved changes below do not affect these values.</p>
      {loading ? <p className="muted" role="status">Checking backup storage…</p> : storage ? (
        <>
          <dl className="backup-storage__values">
            <div><dt>Archive usage</dt><dd>{formatByteSize(storage.archiveBytes)}</dd></div>
            <div><dt>Configured limit</dt><dd>{storage.maxBytes === null ? "Not configured" : formatByteSize(storage.maxBytes)}</dd></div>
            <div><dt>Available disk space</dt><dd>{storage.availableBytes === null ? "Unavailable" : formatByteSize(storage.availableBytes)}</dd></div>
            <div><dt>Free-space reserve</dt><dd>{storage.reserveBytes === null ? "Not configured" : formatByteSize(storage.reserveBytes)}</dd></div>
          </dl>
          {storage.issues.length > 0 && (
            <ul className="backup-storage__issues">
              {storage.issues.map((issue, index) => <li key={`${issue.code}-${index}`}>{issue.message}</li>)}
            </ul>
          )}
          <p className="muted">Available disk space is measured before the reserve. Storage and server data roots are checked again when a backup runs.</p>
        </>
      ) : <p className="muted">Backup storage is unavailable{error ? `: ${error}` : "."}</p>}
    </section>
  );
}
