import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  backupSettingsSchema,
  backupSettingsResponseSchema,
  type BackupSettings,
  type DeploymentSettings,
} from "@ludock/shared";
import { apiJson, jsonBody } from "../../api";
import { usePageRead } from "../../hooks/usePageRead";
import { NavLink } from "../../navigation";
import BackupStorageSummary from "../BackupStorageSummary";
import DeploymentGuidance from "../DeploymentGuidance";

const readSettings = (signal: AbortSignal) =>
  apiJson("/settings/backups", backupSettingsResponseSchema, { signal });

export default function BackupSettingsSection({
  deployment,
}: {
  deployment: DeploymentSettings | null;
}) {
  const page = usePageRead(readSettings, "Unable to load backup settings.");
  return (
    <section
      className="settings-section settings-section--divided"
      aria-labelledby="backup-settings-title"
    >
      <h2 id="backup-settings-title" tabIndex={-1}>Backup storage</h2>
      {page.loading && (
        <p className="muted" role="status">
          Loading backup settings…
        </p>
      )}
      {page.error && (
        <div className="alert alert--error" role="alert">
          <p>{page.error}</p>
          <button className="secondary-btn" onClick={() => void page.refresh()}>
            Retry backup settings
          </button>
        </div>
      )}
      {page.data && (
        <BackupSettingsForm
          settings={page.data.settings}
          deployment={deployment}
        />
      )}
    </section>
  );
}

const gib = 1024 ** 3;

interface BackupDraft {
  // null uses the single suggested root; an explicitly cleared field stays empty.
  destination: string | null;
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

function BackupSettingsForm({
  settings,
  deployment,
}: {
  settings: BackupSettings | null;
  deployment: DeploymentSettings | null;
}) {
  const [backup, setBackup] = useState(() =>
    ({
      ...backupDraft(settings ?? {
        destination: "",
        retentionCount: 10,
        maxBytes: 100 * gib,
        reserveBytes: 5 * gib,
      }),
      destination: settings?.destination ?? null,
    }),
  );
  const destination = backup.destination ??
    (deployment?.backupRoots.length === 1 ? deployment.backupRoots[0] : "");
  const [backupConfigured, setBackupConfigured] = useState(Boolean(settings));
  const [backupStorageRevision, setBackupStorageRevision] = useState(0);
  const backupDestination = useRef<HTMLInputElement>(null);
  const mutation = useRef<AbortController | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState(false);
  useEffect(() => () => mutation.current?.abort(), []);

  async function saveBackup(event: FormEvent) {
    event.preventDefault();
    if (mutation.current) return;
    if (
      [backup.retentionCount, backup.maxGiB, backup.reserveGiB].some(
        (value) => !value.trim(),
      )
    ) {
      setError("Complete all backup limits before saving.");
      setNotice(false);
      return;
    }
    const parsed = backupSettingsSchema.safeParse({
      destination,
      retentionCount: Number(backup.retentionCount),
      maxBytes: Math.round(Number(backup.maxGiB) * gib),
      reserveBytes: Math.round(Number(backup.reserveGiB) * gib),
    });
    if (!parsed.success) {
      setError(
        "Check the backup settings: enter a destination, 1–1,000 backups per server, a positive total limit, and a free-space reserve of zero or more.",
      );
      setNotice(false);
      return;
    }
    const submittedDraft = backup;
    const controller = new AbortController();
    mutation.current = controller;
    setBusy(true);
    setError(null);
    setNotice(false);
    try {
      const { settings } = await apiJson(
        "/settings/backups",
        backupSettingsResponseSchema,
        {
          ...jsonBody("PUT", parsed.data),
          signal: controller.signal,
        },
      );
      if (controller.signal.aborted) return;
      setNotice(true);
      setBackupStorageRevision((value) => value + 1);
      setBackupConfigured(Boolean(settings));
      if (!settings) return;
      const savedDraft = backupDraft(settings);
      if (settings.maxBytes === parsed.data.maxBytes)
        savedDraft.maxGiB = submittedDraft.maxGiB;
      if (settings.reserveBytes === parsed.data.reserveBytes)
        savedDraft.reserveGiB = submittedDraft.reserveGiB;
      setBackup((current) =>
        current === submittedDraft ? savedDraft : {
          ...current,
          destination: current.destination ?? savedDraft.destination,
        },
      );
    } catch (reason) {
      if (!controller.signal.aborted)
        setError(
          reason instanceof Error
            ? reason.message
            : "Unable to save backup settings.",
        );
    } finally {
      mutation.current = null;
      if (!controller.signal.aborted) setBusy(false);
    }
  }
  return (
    <form className="stack-form" onSubmit={saveBackup}>
      <p>
        {backupConfigured
          ? "Backups use the configured destination and limits."
          : "Review the destination and limits, then save to enable backups."}{" "}
        Every backup stops its server for the entire copy.
      </p>
      {backupConfigured && <BackupStorageSummary key={backupStorageRevision} />}
      {deployment && (
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
          value={destination}
          onChange={(event) =>
            setBackup({ ...backup, destination: event.target.value })
          }
          placeholder="/backups"
          autoCapitalize="none"
          spellCheck={false}
          aria-describedby="backup-destination-help"
          required
        />
      </label>
      <p className="muted" id="backup-destination-help">
        An existing folder inside Ludock, separate from game data.
      </p>
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
            Combined size of backups across all servers. Leave room for the next
            backup before older backups are removed.
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
            Space to leave free on the backup disk and on game-data disks during
            restores. Use 0 for no reserve.
          </p>
        </div>
      </div>
      <p className="muted">
        Allow extra disk space for temporary files during backups and restores.
      </p>
      {error && (
        <div className="alert alert--error" role="alert">
          {error}
        </div>
      )}
      {notice && (
        <div
          className="alert alert--success admin-settings-feedback"
          role="status"
        >
          <p>Backup settings saved.</p>
          <NavLink to="/">
            Choose a server and open Backups to create a backup.
          </NavLink>
        </div>
      )}
      <button className="primary-btn" disabled={busy}>
        {busy ? "Saving backup settings…" : "Save backup settings"}
      </button>
    </form>
  );
}
