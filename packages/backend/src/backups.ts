import path from "node:path";
import { z } from "zod";
import {
  backupSettingsSchema,
  type Backup,
  type BackupPreflight,
  type BackupReadinessIssue,
  type BackupSettings,
  type BackupStorageStatus,
} from "@ludock/shared";
import { getDatabase } from "./database.js";
import { getSetting } from "./settings.js";
import { getDockerInstance, getManagedContainerObservation } from "./docker.js";
import {
  assertObservedServerBinding,
  getLogicalServer,
  type ServerObservation,
} from "./identity.js";
import { jobActor } from "./jobs.js";
import { assertServerCapability } from "./authorization.js";
import {
  resolveAuthorizedServer,
  serverLockKeys,
  type ServerContext,
} from "./servers.js";
import { withLocks } from "./operation-locks.js";
import type { JobContext } from "./operations.js";
import { suppressMonitoring } from "./monitoring.js";
import { notifyEvent } from "./notifications.js";
import { RESTORE_HELPER_SCRIPT } from "./restore-helper-script.js";
import { AppError } from "./errors.js";
import {
  approvedBackupDirectory,
  availableBackupDestinationBytes,
  archiveReadStream,
  assertDestinationSpace,
  createDataHelper,
  extractRootToStage,
  failBackup,
  helperExec,
  helperRoot,
  newBackupId,
  planBackupRoots,
  removeArchive,
  removePartialArchive,
  validateArchive,
  validateBackupSettings,
  writeSnapshot,
  type BackupRoot,
  type DataHelper,
} from "./backup-storage.js";
export { validateBackupSettings } from "./backup-storage.js";

interface BackupRow {
  id: string;
  server_id: string;
  binding_fingerprint: string;
  destination: string;
  roots_json: string;
  size: number;
  checksum: string;
  created_at: number;
  state: "complete" | "failed";
}
const restoreJournalsSchema = z.array(z.object({
  root: z.object({ id: z.string(), path: z.string() }),
  phase: z.enum(["staging", "moving_old", "old_moved", "replaced", "rolling_back", "rolled_back"]),
})).max(8);
type RestoreJournal = z.infer<typeof restoreJournalsSchema>[number];
const toBackup = (row: BackupRow): Backup => ({
  id: row.id,
  serverId: row.server_id,
  roots: JSON.parse(row.roots_json) as BackupRoot[],
  size: row.size,
  checksum: row.checksum,
  createdAt: row.created_at,
  state: row.state,
});
function backupRow(serverId: string, id: string): BackupRow {
  const row = getDatabase()
    .query("SELECT * FROM backups WHERE server_id=? AND id=?")
    .get(serverId, id) as BackupRow | null;
  if (!row || row.state !== "complete")
    throw failBackup("BACKUP_NOT_FOUND", "Backup not found.");
  return row;
}
export function listBackups(serverId: string): Backup[] {
  return (
    getDatabase()
      .query(
        "SELECT * FROM backups WHERE server_id=? ORDER BY created_at DESC,id",
      )
      .all(serverId) as unknown as BackupRow[]
  ).map(toBackup);
}
export function getBackup(serverId: string, id: string): Backup {
  return toBackup(backupRow(serverId, id));
}
export async function openBackupDownload(
  serverId: string,
  id: string,
): Promise<ReadableStream<Uint8Array>> {
  const row = backupRow(serverId, id);
  return archiveReadStream(row.destination, row.id);
}
export async function deleteBackup(
  serverId: string,
  id: string,
  assertAccess?: () => void,
): Promise<void> {
  const row = backupRow(serverId, id);
  await withLocks([`server:${serverId}`, "backups:storage"], async () => {
    await removeArchive(row.destination, id, assertAccess);
    getDatabase()
      .query("DELETE FROM backups WHERE id=? AND server_id=?")
      .run(id, serverId);
  });
}
async function settingsForBackup(): Promise<BackupSettings> {
  const settings = getSetting<BackupSettings>("backups");
  if (!settings)
    throw failBackup(
      "BACKUPS_NOT_CONFIGURED",
      "An administrator must configure a backup destination, retention, and capacity before creating backups.",
    );
  return validateBackupSettings(settings);
}

function archiveUsageBytes(): number {
  return (
    getDatabase()
      .query("SELECT COALESCE(SUM(size),0) AS size FROM backups WHERE state='complete'")
      .get() as { size: number }
  ).size;
}

function remainingArchiveBytes(settings: BackupSettings, used: number): number {
  const available = settings.maxBytes - used;
  if (available <= 0)
    throw failBackup(
      "BACKUP_CAPACITY",
      "The global backup byte limit is reached. Remove backups or increase the limit.",
    );
  return available;
}

function readinessIssue(error: unknown, fallback: BackupReadinessIssue): BackupReadinessIssue {
  // Docker and filesystem exceptions can contain host paths or connection
  // credentials. Only our explicit application diagnostics are public.
  return error instanceof AppError
    ? { code: error.code, message: error.message }
    : fallback;
}

async function inspectBackupStorage(): Promise<{
  storage: BackupStorageStatus;
  settings: BackupSettings | null;
}> {
  const configured = getSetting<unknown>("backups");
  const parsed = backupSettingsSchema.safeParse(configured);
  const settings = parsed.success ? parsed.data : null;
  const storage: BackupStorageStatus = {
    configured: configured !== null,
    archiveBytes: archiveUsageBytes(),
    maxBytes: settings?.maxBytes ?? null,
    reserveBytes: settings?.reserveBytes ?? null,
    availableBytes: null,
    issues: [],
  };
  if (!settings) {
    storage.issues.push({
      code: configured === null ? "BACKUPS_NOT_CONFIGURED" : "BACKUP_SETTINGS_INVALID",
      message: "An administrator must configure a valid backup destination, retention, and capacity before creating backups.",
    });
    return { storage, settings: null };
  }
  try {
    remainingArchiveBytes(settings, storage.archiveBytes);
  } catch (error) {
    storage.issues.push(readinessIssue(error, {
      code: "BACKUP_CAPACITY",
      message: "The backup archive limit could not be checked.",
    }));
  }
  try {
    await validateBackupSettings(settings);
  } catch (error) {
    storage.issues.push(readinessIssue(error, {
      code: "BACKUP_DESTINATION",
      message: "The backup destination is missing or inaccessible. Ask an administrator to check its mount and LUDOCK_BACKUP_ROOTS.",
    }));
    return { storage, settings: null };
  }
  try {
    storage.availableBytes = await availableBackupDestinationBytes(settings.destination);
    if (storage.availableBytes <= settings.reserveBytes)
      storage.issues.push({
        code: "BACKUP_CAPACITY",
        message: "The backup destination does not have free space above its configured reserve.",
      });
  } catch (error) {
    storage.issues.push(readinessIssue(error, {
      code: "BACKUP_DISK_UNAVAILABLE",
      message: "Available disk space could not be checked. Ask an administrator to check that the backup destination is accessible and writable.",
    }));
  }
  return { storage, settings };
}

export async function getBackupStorageStatus(): Promise<BackupStorageStatus> {
  return (await inspectBackupStorage()).storage;
}

/** Advisory only: no helper containers, locks, stops, archive writes, or data
 * scans. Execution repeats its authoritative checks after acquiring locks. */
export async function getBackupPreflight(context: ServerContext): Promise<BackupPreflight> {
  const { storage, settings } = await inspectBackupStorage();
  const issues = [...storage.issues];
  const check = async (validate: () => void | Promise<void>, fallback: BackupReadinessIssue) => {
    try { await validate(); }
    catch (error) { issues.push(readinessIssue(error, fallback)); }
  };
  await check(() => { planBackupRoots(context, true); }, {
    code: "BACKUP_ROOT_UNAVAILABLE",
    message: "The selected data roots could not be verified. Ask an administrator to review the server's mounted data roots.",
  });
  if (settings)
    await check(() => assertSeparateDestination(context, settings), {
      code: "BACKUP_MOUNT_UNVERIFIED",
      message: "Ludock could not verify that its backup destination is separate from game data. Ask an administrator to check the backup mount.",
    });
  await check(async () => {
    const info = await getDockerInstance().getContainer(context.container.id).inspect();
    assertDataOperationState(info.State.Status);
  }, {
    code: "SERVER_STATE_UNAVAILABLE",
    message: "The server's current state could not be checked. Check its Docker connection and try again.",
  });
  await check(() => assertNoOtherWriters(context), {
    code: "BACKUP_WRITERS_UNVERIFIED",
    message: "Ludock could not check for other containers writing to this server's data. Check its Docker connection and try again.",
  });
  return { ready: issues.length === 0, checkedAt: Date.now(), issues };
}
const overlaps = (left: string, right: string) =>
  left === "/" ||
  right === "/" ||
  left === right ||
  left.startsWith(`${right}/`) ||
  right.startsWith(`${left}/`);
export function mountsOverlap(
  left: ServerObservation["mounts"][number],
  right: ServerObservation["mounts"][number],
): boolean {
  if (
    left.type === "volume" &&
    right.type === "volume" &&
    left.name &&
    right.name
  )
    return left.name === right.name;
  return (
    Boolean(left.source && right.source) &&
    overlaps(
      path.posix.resolve(left.source),
      path.posix.resolve(right.source),
    )
  );
}
async function assertNoOtherWriters(
  context: ServerContext,
  helperId?: string,
): Promise<void> {
  const docker = getDockerInstance();
  const containers = await docker.listContainers({ all: true });
  const sources = context.observation.mounts.filter((mount) => mount.writable);
  for (const candidate of containers) {
    if (
      candidate.Id === context.container.id ||
      candidate.Id === helperId ||
      !["running", "restarting", "paused"].includes(candidate.State)
    )
      continue;
    const info = await docker.getContainer(candidate.Id).inspect();
    if (!info.State.Running && !info.State.Restarting && !info.State.Paused)
      continue;
    const conflicts = (info.Mounts || [])
      .filter((mount) => mount.RW)
      .some((mount) =>
        sources.some((source) =>
          mountsOverlap(source, {
            type: mount.Type,
            source: mount.Source,
            destination: mount.Destination,
            writable: mount.RW,
            name: mount.Name,
          }),
        ),
      );
    if (conflicts)
      throw failBackup(
        "SHARED_DATA_WRITER",
        "Another running container can write to this server's data. Stop shared writers before backing up or restoring.",
      );
  }
}
async function assertSeparateDestination(
  context: ServerContext,
  settings: BackupSettings,
): Promise<void> {
  const destination = await approvedBackupDirectory(settings.destination);
  const targets: ServerObservation["mounts"] = [
    { type: "bind", source: destination, destination, writable: true },
  ];
  const selfId = process.env.LUDOCK_SELF_CONTAINER || process.env.HOSTNAME;
  let proven = false;
  if (selfId && /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$/.test(selfId)) {
    try {
      const self = await getDockerInstance().getContainer(selfId).inspect();
      for (const mount of self.Mounts || []) {
        if (
          destination === mount.Destination ||
          destination.startsWith(`${mount.Destination}/`)
        ) {
          proven = true;
          targets.push({
            type: mount.Type,
            name: mount.Name,
            source: `${mount.Source}${destination.slice(mount.Destination.length)}`,
            destination,
            writable: true,
          });
        }
      }
    } catch (error) {
      if ((error as { statusCode?: number }).statusCode !== 404) throw error;
    }
  }
  if (!proven)
    throw failBackup(
      "BACKUP_MOUNT_UNVERIFIED",
      "The backup destination must be mounted into Ludock. Set LUDOCK_SELF_CONTAINER if its Docker hostname does not identify its container.",
    );
  if (
    context.observation.mounts
      .filter((mount) => mount.writable)
      .some((source) => targets.some((target) => mountsOverlap(source, target)))
  )
    throw failBackup(
      "BACKUP_SOURCE_OVERLAP",
      "The backup destination overlaps game data. Mount a separate backup destination.",
    );
}

function assertDataOperationState(state: string): void {
  if (["paused", "restarting", "removing", "dead"].includes(state))
    throw failBackup(
      "SERVER_STATE",
      "Unpause or repair this server before a data operation.",
    );
}

export async function stopForDataOperation(
  context: ServerContext,
  job: JobContext,
): Promise<void> {
  const target = getDockerInstance().getContainer(context.container.id);
  const info = await target.inspect();
  assertDataOperationState(info.State.Status);
  assertDataOperationAuthority(context, job);
  if (job.job.recovery.initialRunning === undefined)
    job.progress("stopping", {
      initialRunning: info.State.Running || info.State.Status === "running",
      containerId: context.container.id,
      fingerprint: context.logical.bindingFingerprint,
      bindingRevision: context.logical.bindingRevision,
      dataSafe: true,
    });
  suppressMonitoring(context.logical.id);
  if (info.State.Running || info.State.Status === "running")
    await target.stop({ t: 120 });
  const stopped = await target.inspect();
  if (stopped.State.Running || stopped.State.Status === "running")
    throw failBackup(
      "SERVER_DID_NOT_STOP",
      "The server did not stop; no data was copied.",
    );
  job.progress("stopped", { stoppedStartedAt: stopped.State.StartedAt || "" });
  if (stopped.State.ExitCode === 137 || stopped.State.OOMKilled)
    throw failBackup(
      "UNCLEAN_SHUTDOWN",
      "The server needed forced termination. The backup was not created; review its graceful shutdown configuration before retrying.",
    );
}

function assertDataOperationAuthority(
  context: ServerContext,
  job: JobContext,
): void {
  assertServerCapability(
    jobActor(job),
    context.logical.id,
    job.job.kind === "backup"
      ? "backups.create"
      : job.job.kind === "restore"
        ? "backups.restore"
        : "server.update",
  );
  assertObservedServerBinding(
    context.logical.id,
    context.observation,
    context.logical.bindingRevision,
  );
}

export async function assertDataOperationStopped(
  context: ServerContext,
  job: JobContext,
): Promise<void> {
  const observed = await getManagedContainerObservation(context.container.id);
  assertObservedServerBinding(
    context.logical.id,
    observed.observation,
    context.logical.bindingRevision,
  );
  const info = await getDockerInstance()
    .getContainer(context.container.id)
    .inspect();
  if (
    info.State.Running ||
    info.State.Status === "running" ||
    info.State.Paused ||
    info.State.Restarting ||
    (info.State.StartedAt || "") !== job.job.recovery.stoppedStartedAt
  )
    throw failBackup(
      "EXTERNAL_SERVER_CHANGE",
      "The server was started or changed by another manager during the operation. Review its state before retrying.",
    );
  await assertNoOtherWriters(
    context,
    typeof job.job.recovery.dataHelperId === "string"
      ? job.job.recovery.dataHelperId
      : undefined,
  );
}

export async function restoreInitialRunningState(
  context: ServerContext,
  job: JobContext,
): Promise<void> {
  if (
    !job.job.recovery.initialRunning ||
    job.job.recovery.dataSafe === false ||
    job.job.recovery.stateRestored === true
  )
    return;
  const observed = await getManagedContainerObservation(context.container.id);
  assertObservedServerBinding(
    context.logical.id,
    observed.observation,
    context.logical.bindingRevision,
  );
  if (observed.container.state !== "running") {
    job.progress("restarting");
    await getDockerInstance().getContainer(context.container.id).start();
  }
  job.progress("state_restored", { stateRestored: true });
  suppressMonitoring(context.logical.id);
}

/** Parent holds all data/storage locks and owns restoring running state. */
export async function createStoppedBackup(
  context: ServerContext,
  job: JobContext,
): Promise<Backup> {
  const settings = await settingsForBackup();
  await assertSeparateDestination(context, settings);
  await assertDataOperationStopped(context, job);
  await assertDestinationSpace(settings.destination, settings.reserveBytes);
  const available = remainingArchiveBytes(settings, archiveUsageBytes());
  const id = newBackupId();
  job.progress("backing_up", {
    backupId: id,
    backupDestination: settings.destination,
    backupComplete: false,
  });
  let helper: DataHelper | undefined;
  try {
    assertDataOperationAuthority(context, job);
    helper = await createDataHelper(context, true, job.job.id);
    job.progress("backing_up", { dataHelperId: helper.container.id });
    let stoppedFailure: Error | undefined;
    let checking: Promise<void> | undefined;
    const assertCopyAllowed = async () => {
      if (stoppedFailure) throw stoppedFailure;
      await assertDataOperationStopped(context, job);
      assertDataOperationAuthority(context, job);
    };
    const timer = setInterval(() => {
      if (checking) return;
      checking = assertCopyAllowed()
        .catch(async (error: unknown) => {
          stoppedFailure =
            error instanceof Error
              ? error
              : failBackup(
                  "EXTERNAL_SERVER_CHANGE",
                  "The server changed during backup.",
                );
          await helper?.cleanup().catch(() => {});
        })
        .finally(() => {
          checking = undefined;
        });
    }, 1000);
    timer.unref();
    let result: { size: number; checksum: string };
    try {
      result = await writeSnapshot(
        context,
        helper,
        settings,
        id,
        available,
        assertCopyAllowed,
        () => assertDataOperationAuthority(context, job),
      );
    } finally {
      clearInterval(timer);
      await checking;
    }
    if (stoppedFailure) throw stoppedFailure;
    await validateArchive(
      settings.destination,
      id,
      helper.roots,
      settings.maxBytes,
      result.checksum,
    );
    await assertCopyAllowed();
    const createdAt = Date.now();
    getDatabase()
      .query(
        "INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state) VALUES(?,?,?,?,?,?,?,?,'complete')",
      )
      .run(
        id,
        context.logical.id,
        context.logical.bindingFingerprint,
        settings.destination,
        JSON.stringify(helper.roots),
        result.size,
        result.checksum,
        createdAt,
      );
    job.progress("backup_complete", { backupComplete: true });
    const existing = listBackups(context.logical.id).filter(
      (backup) => backup.state === "complete",
    );
    for (const old of existing.slice(settings.retentionCount)) {
      if (old.id === job.job.input.backupId) continue;
      const row = backupRow(context.logical.id, old.id);
      await removeArchive(row.destination, old.id);
      getDatabase().query("DELETE FROM backups WHERE id=?").run(old.id);
    }
    return {
      id,
      serverId: context.logical.id,
      roots: helper.roots,
      createdAt,
      ...result,
      state: "complete",
    };
  } catch (error) {
    if (!job.job.recovery.backupComplete) {
      await removeArchive(settings.destination, id).catch(() => {});
      await removePartialArchive(settings.destination, id).catch(() => {});
    }
    throw error;
  } finally {
    await helper?.cleanup();
    job.progress(job.job.phase, { dataHelperId: null });
  }
}

export async function runBackup(
  job: JobContext,
): Promise<Record<string, unknown>> {
  const actor = jobActor(job);
  const context = await resolveAuthorizedServer(
    actor,
    job.job.serverId,
    "backups.create",
    job.job.bindingRevision,
  );
  return withLocks([...context.lockKeys, "backups:storage"], async () => {
    assertServerCapability(jobActor(job), context.logical.id, "backups.create");
    const settings = await settingsForBackup();
    await assertSeparateDestination(context, settings);
    await assertDestinationSpace(settings.destination, settings.reserveBytes);
    await assertNoOtherWriters(context);
    try {
      await stopForDataOperation(context, job);
      const backup = await createStoppedBackup(context, job);
      return {
        backupId: backup.id,
        size: backup.size,
        checksum: backup.checksum,
      };
    } catch (error) {
      notifyEvent(
        `backup-failed:${job.job.id}`,
        `Backup failed for ${context.container.displayName}. Review its operation details.`,
      );
      throw error;
    } finally {
      await restoreInitialRunningState(context, job);
    }
  });
}

async function recoveryContext(job: JobContext): Promise<ServerContext | null> {
  await removeOperationHelpers(job);
  if (typeof job.job.recovery.containerId !== "string") return null;
  const logical = getLogicalServer(job.job.serverId);
  if (
    !logical ||
    logical.status !== "active" ||
    !logical.containerId ||
    logical.containerId !== job.job.recovery.containerId ||
    logical.bindingFingerprint !== job.job.recovery.fingerprint
  )
    throw failBackup(
      "RECOVERY_BINDING_CHANGED",
      "Recovery requires review because the server binding changed.",
    );
  const observed = await getManagedContainerObservation(logical.containerId);
  assertObservedServerBinding(
    logical.id,
    observed.observation,
    logical.bindingRevision,
  );
  return {
    logical: { ...logical, containerId: logical.containerId },
    ...observed,
    lockKeys: serverLockKeys(logical.id, observed.observation),
  };
}
async function removeUnpublishedBackup(job: JobContext): Promise<void> {
  const id = job.job.recovery.backupId,
    destination = job.job.recovery.backupDestination;
  if (
    typeof id === "string" &&
    typeof destination === "string" &&
    !getDatabase().query("SELECT id FROM backups WHERE id=?").get(id)
  ) {
    await removeArchive(destination, id);
    await removePartialArchive(destination, id);
  }
}
export async function recoverBackup(job: JobContext): Promise<void> {
  const context = await recoveryContext(job);
  if (!context) return;
  await withLocks([...context.lockKeys, "backups:storage"], async () => {
    await removeUnpublishedBackup(job);
    await restoreInitialRunningState(context, job);
  });
}

function journals(job: JobContext): RestoreJournal[] {
  const value = job.job.recovery.restoreRoots;
  const parsed = restoreJournalsSchema.safeParse(value === undefined ? [] : value);
  if (!parsed.success ||
      new Set(parsed.data.map(({ root }) => root.id)).size !== parsed.data.length ||
      (!parsed.data.length && (job.job.recovery.dataSafe === false || job.job.recovery.restoreCommitted === true)))
    throw failBackup("INVALID_RESTORE_JOURNAL", "Restore recovery records are invalid. Keep the server stopped and review its recovery state.");
  return parsed.data;
}
function stageName(job: JobContext): string {
  return `.ludock-restore-${job.job.id}`;
}
function persistJournal(
  job: JobContext,
  index: number,
  phase: RestoreJournal["phase"],
): void {
  const roots = journals(job).map((entry, position) =>
    position === index ? { ...entry, phase } : entry,
  );
  job.progress(`restore_${phase}`, { restoreRoots: roots });
}
async function restoreStep(
  helper: DataHelper,
  root: string,
  job: JobContext,
  operation: string,
  assertAccess?: () => void,
): Promise<Record<string, unknown>> {
  const command = [
    "bun",
    "-e",
    RESTORE_HELPER_SCRIPT,
    JSON.stringify({ operation, root, stage: stageName(job) }),
  ];
  const result = await helperExec(helper.container, command, {}, assertAccess);
  return JSON.parse(result) as Record<string, unknown>;
}
async function rollbackRestore(
  context: ServerContext,
  job: JobContext,
  helper: DataHelper,
): Promise<void> {
  for (const entry of [...journals(job)].reverse()) {
    await assertDataOperationStopped(context, job);
    const rootPath = helperRoot(context, entry.root);
    if (entry.phase === "old_moved" || entry.phase === "replaced")
      await restoreStep(helper, rootPath, job, "rollbackClean");
    if (entry.phase !== "staging" && entry.phase !== "rolled_back") {
      // Persist after remove-new-data and BEFORE returning any old entry. A
      // restart midway through rollback must never delete returned originals.
      persistJournal(
        job,
        journals(job).findIndex(
          (candidate) => candidate.root.id === entry.root.id,
        ),
        "rolling_back",
      );
      await restoreStep(helper, rootPath, job, "rollbackOld");
      // Record returned originals before removing the stage. Recovery can then
      // safely repeat cleanup even if the stage was already removed at a crash.
      persistJournal(
        job,
        journals(job).findIndex(
          (candidate) => candidate.root.id === entry.root.id,
        ),
        "rolled_back",
      );
    }
    await restoreStep(helper, rootPath, job, "cleanup");
    const remaining = journals(job).filter(
      (candidate) => candidate.root.id !== entry.root.id,
    );
    job.progress("restore_root_recovered", {
      restoreRoots: remaining,
      ...(remaining.length === 0 ? { dataSafe: true } : {}),
    });
  }
  job.progress("restore_rolled_back", { dataSafe: true, restoreRoots: [] });
}
async function cleanupRestore(
  context: ServerContext,
  job: JobContext,
  helper: DataHelper,
): Promise<void> {
  for (const entry of journals(job))
    await restoreStep(helper, helperRoot(context, entry.root), job, "cleanup");
  job.progress("restore_cleaned", { restoreRoots: [] });
}

export async function runRestore(
  job: JobContext,
): Promise<Record<string, unknown>> {
  const actor = jobActor(job);
  const context = await resolveAuthorizedServer(
    actor,
    job.job.serverId,
    "backups.restore",
    job.job.bindingRevision,
  );
  if (job.job.input.confirmation !== context.container.displayName)
    throw failBackup(
      "RESTORE_CONFIRMATION",
      "Type the server's display name to confirm this restore.",
    );
  const row = backupRow(
    context.logical.id,
    typeof job.job.input.backupId === "string" ? job.job.input.backupId : "",
  );
  if (row.binding_fingerprint !== context.logical.bindingFingerprint)
    throw failBackup(
      "BACKUP_BINDING_CHANGED",
      "This backup belongs to a different server configuration. Review its data roots before restoring.",
    );
  const roots = JSON.parse(row.roots_json) as BackupRoot[];
  if (
    JSON.stringify(roots) !==
    JSON.stringify(
      context.container.fileRoots.map(({ id, path: rootPath }) => ({
        id,
        path: rootPath,
      })),
    )
  )
    throw failBackup(
      "BACKUP_ROOT_CHANGED",
      "The current data roots do not match this backup.",
    );
  const settings = await settingsForBackup();
  await validateArchive(
    row.destination,
    row.id,
    roots,
    settings.maxBytes,
    row.checksum,
  );
  return withLocks([...context.lockKeys, "backups:storage"], async () => {
    let helper: DataHelper | undefined;
    const assertAccess = () => assertDataOperationAuthority(context, job);
    try {
      assertServerCapability(actor, context.logical.id, "backups.restore");
      await validateArchive(
        row.destination,
        row.id,
        roots,
        settings.maxBytes,
        row.checksum,
      );
      await stopForDataOperation(context, job);
      const safety = await createStoppedBackup(context, job);
      job.progress("restore_staging", {
        safetyBackupId: safety.id,
        restoreRoots: roots.map((root) => ({ root, phase: "staging" })),
      });
      assertDataOperationAuthority(context, job);
      helper = await createDataHelper(context, false, job.job.id);
      job.progress("restore_staging", { dataHelperId: helper.container.id });
      for (const root of roots) {
        await assertDataOperationStopped(context, job);
        const destination = helperRoot(context, root);
        const { availableBytes } = await restoreStep(
          helper,
          destination,
          job,
          "space",
          assertAccess,
        );
        if (
          typeof availableBytes !== "number" ||
          !Number.isFinite(availableBytes) ||
          availableBytes < row.size + settings.reserveBytes
        )
          throw failBackup(
            "RESTORE_CAPACITY",
            "A data root lacks space for restore staging and the configured reserve.",
          );
        assertDataOperationAuthority(context, job);
        await restoreStep(helper, destination, job, "stage", assertAccess);
        assertDataOperationAuthority(context, job);
        await extractRootToStage(
          row.destination,
          row.id,
          helper.container,
          root,
          destination,
          stageName(job),
          roots,
          settings.maxBytes,
          row.checksum,
          assertAccess,
        );
      }
      assertDataOperationAuthority(context, job);
      job.progress("restore_replacing", { dataSafe: false });
      for (let index = 0; index < roots.length; index++) {
        await assertDataOperationStopped(context, job);
        assertDataOperationAuthority(context, job);
        const destination = helperRoot(context, roots[index]);
        persistJournal(job, index, "moving_old");
        await restoreStep(helper, destination, job, "moveOld", assertAccess);
        persistJournal(job, index, "old_moved");
        assertDataOperationAuthority(context, job);
        await restoreStep(helper, destination, job, "moveNew", assertAccess);
        persistJournal(job, index, "replaced");
      }
      await assertDataOperationStopped(context, job);
      assertDataOperationAuthority(context, job);
      job.progress("restore_committed", {
        restoreCommitted: true,
        dataSafe: true,
      });
      await cleanupRestore(context, job, helper);
      notifyEvent(
        `restore-result:${job.job.id}`,
        `Restore completed for ${context.container.displayName}.`,
      );
      return { restoredBackupId: row.id, safetyBackupId: safety.id };
    } catch (error) {
      if (helper && !job.job.recovery.restoreCommitted)
        await rollbackRestore(context, job, helper);
      notifyEvent(
        `restore-result:${job.job.id}`,
        `Restore failed for ${context.container.displayName}. Review its recovery state.`,
      );
      throw error;
    } finally {
      await helper?.cleanup();
      await restoreInitialRunningState(context, job);
    }
  });
}

export async function recoverRestore(job: JobContext): Promise<void> {
  const context = await recoveryContext(job);
  if (!context) return;
  await withLocks([...context.lockKeys, "backups:storage"], async () => {
    const roots = journals(job);
    // Recovery must use exactly the roots of the revalidated binding. A corrupt
    // journal must never select a helper path or permit an unsafe restart.
    if (roots.length && (roots.length !== context.container.fileRoots.length ||
        roots.some(({ root }) => !context.container.fileRoots.some((current) => current.id === root.id && current.path === root.path))))
      throw failBackup("INVALID_RESTORE_JOURNAL", "Restore recovery roots do not match this server. Keep it stopped and review its recovery state.");
    await removeUnpublishedBackup(job);
    if (roots.length) {
      await assertDataOperationStopped(context, job);
      const helper = await createDataHelper(context, false, job.job.id);
      job.progress("recovering", { dataHelperId: helper.container.id });
      try {
        if (job.job.recovery.restoreCommitted)
          await cleanupRestore(context, job, helper);
        else await rollbackRestore(context, job, helper);
      } finally {
        await helper.cleanup();
      }
    }
    await restoreInitialRunningState(context, job);
  });
}

async function removeOperationHelpers(job: JobContext): Promise<void> {
  const docker = getDockerInstance();
  const helpers = await docker.listContainers({
    all: true,
    filters: { label: [`ludock.operation=${job.job.id}`] },
  });
  for (const helper of helpers) {
    if (
      !["backup-helper", "mount-validator"].includes(
        helper.Labels?.["ludock.internal"] || "",
      ) ||
      helper.Labels?.["ludock.operation"] !== job.job.id
    )
      continue;
    await docker.getContainer(helper.Id).remove({ force: true, v: true });
  }
  job.progress("recovering", { dataHelperId: null });
}
