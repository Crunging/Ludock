import fs from "node:fs";
import path from "node:path";
import { createHmac } from "node:crypto";
import { pathToFileURL } from "node:url";
import { Database, constants as sqliteConstants } from "bun:sqlite";
import {
  applyMigrations,
  assertCompatibleDatabase,
  DATABASE_BASE_SCHEMA_VERSION,
} from "./migrations.js";
import {
  assertFingerprintKey,
  loadFingerprintKey,
  protectBindingFingerprint,
  protectComposeSourceFingerprint,
} from "./fingerprints.js";

import type {
  AuthUser,
  UserSummary,
  SessionSummary,
  AuditEntry,
} from "@ludock/shared";
export type { UserSummary, SessionSummary } from "@ludock/shared";
export type SessionUser = AuthUser;
export type AuditRecord = AuditEntry;

export interface UserRecord extends UserSummary {
  passwordHash: string;
}

export interface LoginThrottle {
  failures: number;
  blockedUntil: number;
}

let database: Database | null = null;
let fingerprintKey: Buffer | null = null;

export function getDatabase(): Database {
  if (database) return database;

  const configuredPath = process.env.LUDOCK_DB_PATH;
  const dbPath =
    configuredPath ||
    (process.env.NODE_ENV === "production"
      ? "/data/ludock.db"
      : path.resolve("data/ludock.db"));

  // Validate existing data before WAL, chmod, migrations, or any other writes.
  if (
    dbPath !== ":memory:" &&
    fs.existsSync(dbPath) &&
    fs.statSync(dbPath).size > 0
  ) {
    const canonicalPath = fs.realpathSync(dbPath);
    const hasJournal = ["-wal", "-journal"].some((suffix) =>
      fs.existsSync(`${canonicalPath}${suffix}`) && fs.statSync(`${canonicalPath}${suffix}`).size > 0,
    );
    // SQLite cannot always open a checkpointed WAL-mode database read-only
    // when its shared-memory files are absent. Immutable inspection avoids
    // creating sidecars; use the normal reader if journaled changes exist.
    const inspectionPath = hasJournal
      ? canonicalPath
      : `${pathToFileURL(canonicalPath).href}?immutable=1`;
    const inspectionOptions = hasJournal
      ? { readonly: true }
      : sqliteConstants.SQLITE_OPEN_READONLY |
        sqliteConstants.SQLITE_OPEN_URI;
    const existing = new Database(inspectionPath, inspectionOptions);
    try {
      assertCompatibleDatabase(existing);
    } finally {
      existing.close(true);
    }
  }
  if (dbPath !== ":memory:") {
    fs.mkdirSync(path.dirname(dbPath), { recursive: true, mode: 0o700 });
  }
  const opened = new Database(dbPath, {
    strict: true,
  });
  let key: Buffer | null = null;
  try {
    assertCompatibleDatabase(opened);
    const version = (opened.prepare("PRAGMA user_version").get() as { user_version: number }).user_version;
    const keyed = version >= DATABASE_BASE_SCHEMA_VERSION;
    key = loadFingerprintKey(dbPath, keyed);
    if (keyed) assertFingerprintKey(opened, key);
    opened.exec("PRAGMA foreign_keys = ON");
    opened.exec("PRAGMA busy_timeout = 5000");
    applyMigrations(opened, undefined, key);
    opened.exec("PRAGMA journal_mode = WAL");
    if (dbPath !== ":memory:") fs.chmodSync(dbPath, 0o600);
    database = opened;
    fingerprintKey = key;
    key = null;
  } catch (error) {
    key?.fill(0);
    try {
      opened.close(true);
    } catch {
      // Preserve the initialization error while still clearing key material.
    }
    throw error;
  }

  return database;
}

export function keyedBindingFingerprint(digest: string): string {
  getDatabase();
  return protectBindingFingerprint(digest, fingerprintKey!);
}

export function keyedComposeSourceFingerprint(digest: string): string {
  getDatabase();
  return protectComposeSourceFingerprint(digest, fingerprintKey!);
}

export function keyedCredentialFingerprint(value: string): string {
  getDatabase();
  return createHmac("sha256", fingerprintKey!)
    .update(`ludock:credential:v1:${value}`).digest("hex");
}

export function getLoginThrottle(
  attemptKey: string,
  now: number,
  windowMs: number,
): LoginThrottle {
  const row = getDatabase()
    .prepare(
      `SELECT failures, window_started_at, blocked_until
       FROM login_attempts WHERE attempt_key = ?`,
    )
    .get(attemptKey) as
    | { failures: number; window_started_at: number; blocked_until: number }
    | null;

  if (
    !row ||
    (row.blocked_until <= now && now - row.window_started_at >= windowMs)
  ) {
    if (row) clearLoginThrottle(attemptKey);
    return { failures: 0, blockedUntil: 0 };
  }
  return { failures: row.failures, blockedUntil: row.blocked_until };
}

const LOGIN_PRUNE_INTERVAL = 200;
let failuresSincePrune = 0;

export function recordLoginFailure(
  attemptKey: string,
  now: number,
  windowMs: number,
  maxFailures: number,
): LoginThrottle {
  const current = getLoginThrottle(attemptKey, now, windowMs);
  const failures = current.failures + 1;
  const blockedUntil =
    failures >= maxFailures ? now + windowMs : current.blockedUntil;
  const existing = getDatabase()
    .prepare(
      "SELECT window_started_at FROM login_attempts WHERE attempt_key = ?",
    )
    .get(attemptKey) as { window_started_at: number } | null;
  getDatabase()
    .prepare(
      `INSERT INTO login_attempts
        (attempt_key, failures, window_started_at, blocked_until, updated_at)
       VALUES (?, ?, ?, ?, ?)
       ON CONFLICT(attempt_key) DO UPDATE SET
         failures = excluded.failures,
         blocked_until = excluded.blocked_until,
         updated_at = excluded.updated_at`,
    )
    .run(
      attemptKey,
      failures,
      existing?.window_started_at || now,
      blockedUntil,
      now,
    );

  failuresSincePrune += 1;
  if (failuresSincePrune >= LOGIN_PRUNE_INTERVAL) {
    failuresSincePrune = 0;
    pruneLoginAttempts(now, windowMs);
  }
  return { failures, blockedUntil };
}

export function clearLoginThrottle(attemptKey: string): void {
  getDatabase()
    .prepare("DELETE FROM login_attempts WHERE attempt_key = ?")
    .run(attemptKey);
}

export function countUsers(): number {
  const row = getDatabase()
    .prepare("SELECT COUNT(*) AS count FROM users")
    .get() as {
    count: number;
  };
  return row.count;
}

export function checkDatabase(): void {
  getDatabase().prepare("SELECT 1").get();
}

export function createUser(user: UserRecord): void {
  getDatabase()
    .prepare(
      `INSERT INTO users
        (id, username, password_hash, role, disabled, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)`,
    )
    .run(
      user.id,
      user.username,
      user.passwordHash,
      user.role,
      user.disabled ? 1 : 0,
      user.createdAt,
      user.createdAt,
    );
}

export function findUserByUsername(username: string): UserRecord | null {
  const row = getDatabase()
    .prepare(
      `SELECT id, username, password_hash, role, disabled, created_at
       FROM users WHERE username = ?`,
    )
    .get(username) as
    | {
        id: string;
        username: string;
        password_hash: string;
        role: UserRecord["role"];
        disabled: number;
        created_at: number;
      }
    | null;

  return row
    ? {
        id: row.id,
        username: row.username,
        passwordHash: row.password_hash,
        role: row.role,
        disabled: row.disabled === 1,
        createdAt: row.created_at,
      }
    : null;
}

export function findUserById(id: string): UserRecord | null {
  const row = getDatabase()
    .prepare(
      `SELECT id, username, password_hash, role, disabled, created_at
       FROM users WHERE id = ?`,
    )
    .get(id) as
    | {
        id: string;
        username: string;
        password_hash: string;
        role: UserRecord["role"];
        disabled: number;
        created_at: number;
      }
    | null;
  return row
    ? {
        id: row.id,
        username: row.username,
        passwordHash: row.password_hash,
        role: row.role,
        disabled: row.disabled === 1,
        createdAt: row.created_at,
      }
    : null;
}

export function listUsers(): UserSummary[] {
  const rows = getDatabase()
    .prepare(
      `SELECT id, username, role, disabled, created_at
       FROM users ORDER BY username COLLATE NOCASE`,
    )
    .all() as Array<{
    id: string;
    username: string;
    role: UserRecord["role"];
    disabled: number;
    created_at: number;
  }>;
  return rows.map((row) => ({
    id: row.id,
    username: row.username,
    role: row.role,
    disabled: row.disabled === 1,
    createdAt: row.created_at,
  }));
}

export function countEnabledAdmins(): number {
  const row = getDatabase()
    .prepare(
      "SELECT COUNT(*) AS count FROM users WHERE role = 'admin' AND disabled = 0",
    )
    .get() as { count: number };
  return row.count;
}

export function updateUserAccess(
  id: string,
  role: UserRecord["role"],
  disabled: boolean,
): void {
  getDatabase()
    .prepare(
      "UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?",
    )
    .run(role, disabled ? 1 : 0, Date.now(), id);
  if (disabled) deleteUserSessions(id);
}

export function updateUserPassword(id: string, passwordHash: string): void {
  getDatabase()
    .prepare("UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?")
    .run(passwordHash, Date.now(), id);
  deleteUserSessions(id);
}

export function upgradeUserPasswordHash(
  id: string,
  passwordHash: string,
): void {
  getDatabase()
    .prepare("UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?")
    .run(passwordHash, Date.now(), id);
}

export function deleteUser(id: string): void {
  getDatabase().prepare("DELETE FROM users WHERE id = ?").run(id);
}

export function deleteUserSessions(userId: string): void {
  getDatabase().prepare("DELETE FROM sessions WHERE user_id = ?").run(userId);
}

/**
 * Every distinct source IP creates a throttle row, so a distributed attack would
 * leave them behind indefinitely. Drop rows that are no longer blocking and
 * whose window has long since closed.
 */
export function pruneLoginAttempts(now: number, windowMs: number): void {
  getDatabase()
    .prepare(
      `DELETE FROM login_attempts
       WHERE blocked_until <= ? AND updated_at < ?`,
    )
    .run(now, now - windowMs);
}

export function createSessionRecord(input: {
  sessionId: string;
  tokenHash: string;
  userId: string;
  createdAt: number;
  expiresAt: number;
  ipAddress?: string;
  userAgent?: string;
}): void {
  const db = getDatabase();
  db.prepare("DELETE FROM sessions WHERE expires_at <= ?").run(input.createdAt);
  db.prepare(
    `INSERT INTO sessions
      (token_hash, session_id, user_id, created_at, expires_at, last_seen_at, ip_address, user_agent)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
  ).run(
    input.tokenHash,
    input.sessionId,
    input.userId,
    input.createdAt,
    input.expiresAt,
    input.createdAt,
    input.ipAddress || null,
    input.userAgent || null,
  );
}

export function listUserSessions(
  userId: string,
  currentTokenHash: string,
): SessionSummary[] {
  const rows = getDatabase()
    .prepare(
      `SELECT session_id, token_hash, created_at, expires_at, last_seen_at,
              ip_address, user_agent
       FROM sessions
       WHERE user_id = ? AND expires_at > ?
       ORDER BY last_seen_at DESC`,
    )
    .all(userId, Date.now()) as Array<{
    session_id: string | null;
    token_hash: string;
    created_at: number;
    expires_at: number;
    last_seen_at: number;
    ip_address: string | null;
    user_agent: string | null;
  }>;

  return rows
    .filter((row): row is typeof row & { session_id: string } =>
      Boolean(row.session_id),
    )
    .map((row) => ({
      id: row.session_id,
      createdAt: row.created_at,
      expiresAt: row.expires_at,
      lastSeenAt: row.last_seen_at,
      ipAddress: row.ip_address,
      userAgent: row.user_agent,
      current: row.token_hash === currentTokenHash,
    }));
}

export function deleteUserSessionById(
  userId: string,
  sessionId: string,
): boolean {
  const result = getDatabase()
    .prepare("DELETE FROM sessions WHERE user_id = ? AND session_id = ?")
    .run(userId, sessionId);
  return result.changes > 0;
}

const LAST_SEEN_WRITE_INTERVAL_MS = 60_000;

export function findSessionUser(
  tokenHash: string,
  now: number,
): SessionUser | null {
  const row = getDatabase()
    .prepare(
      `SELECT users.id, users.username, users.role, users.disabled,
              sessions.expires_at, sessions.last_seen_at
       FROM sessions
       JOIN users ON users.id = sessions.user_id
       WHERE sessions.token_hash = ?`,
    )
    .get(tokenHash) as
    | {
        id: string;
        username: string;
        role: UserRecord["role"];
        disabled: number;
        expires_at: number;
        last_seen_at: number;
      }
    | null;

  if (!row || row.disabled === 1 || row.expires_at <= now) {
    if (row) deleteSessionRecord(tokenHash);
    return null;
  }

  // Every authenticated request lands here, so avoid a write per request. The
  // value only drives the session list and coarse activity display.
  if (now - row.last_seen_at >= LAST_SEEN_WRITE_INTERVAL_MS) {
    getDatabase()
      .prepare("UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?")
      .run(now, tokenHash);
  }

  return { id: row.id, username: row.username, role: row.role };
}

export function deleteSessionRecord(tokenHash: string): void {
  getDatabase()
    .prepare("DELETE FROM sessions WHERE token_hash = ?")
    .run(tokenHash);
}

/**
 * Failed logins are audited for unauthenticated callers, so the audit log would
 * otherwise grow without bound and fill the volume. Keep a rolling window.
 */
const AUDIT_LOG_MAX_ROWS = Math.max(
  1000,
  Number(process.env.AUDIT_LOG_MAX_ROWS) || 100_000,
);
const AUDIT_PRUNE_INTERVAL = 500;
let auditWritesSincePrune = 0;

export function pruneAuditLog(): void {
  getDatabase()
    .prepare(
      `DELETE FROM audit_log
       WHERE id <= (SELECT MAX(id) FROM audit_log) - ?`,
    )
    .run(AUDIT_LOG_MAX_ROWS);
}

export function writeAuditLog(input: {
  userId?: string;
  action: string;
  targetType?: string;
  targetId?: string;
  details?: unknown;
  ipAddress?: string;
}): void {
  getDatabase()
    .prepare(
      `INSERT INTO audit_log
        (user_id, action, target_type, target_id, details_json, ip_address, created_at)
       VALUES (?, ?, ?, ?, ?, ?, ?)`,
    )
    .run(
      input.userId || null,
      input.action,
      input.targetType || null,
      input.targetId || null,
      input.details === undefined ? null : JSON.stringify(input.details),
      input.ipAddress || null,
      Date.now(),
    );

  auditWritesSincePrune += 1;
  if (auditWritesSincePrune >= AUDIT_PRUNE_INTERVAL) {
    auditWritesSincePrune = 0;
    pruneAuditLog();
  }
}

export function listAuditLog(limit: number): AuditRecord[] {
  const rows = getDatabase()
    .prepare(
      `SELECT audit_log.id, users.username, audit_log.action,
              audit_log.target_type, audit_log.target_id,
              audit_log.details_json, audit_log.ip_address,
              audit_log.created_at
       FROM audit_log
       LEFT JOIN users ON users.id = audit_log.user_id
       ORDER BY audit_log.id DESC
       LIMIT ?`,
    )
    .all(limit) as Array<{
    id: number;
    username: string | null;
    action: string;
    target_type: string | null;
    target_id: string | null;
    details_json: string | null;
    ip_address: string | null;
    created_at: number;
  }>;
  return rows.map((row) => ({
    id: row.id,
    username: row.username,
    action: row.action,
    targetType: row.target_type,
    targetId: row.target_id,
    details: row.details_json
      ? (JSON.parse(row.details_json) as unknown)
      : null,
    ipAddress: row.ip_address,
    createdAt: row.created_at,
  }));
}

export function closeDatabase(): void {
  const opened = database;
  database = null;
  const key = fingerprintKey;
  fingerprintKey = null;
  try {
    opened?.close(true);
  } finally {
    key?.fill(0);
  }
}
