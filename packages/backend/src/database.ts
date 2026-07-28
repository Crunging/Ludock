import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

export interface UserRecord {
  id: string;
  username: string;
  passwordHash: string;
  role: "admin" | "operator" | "viewer";
  disabled: boolean;
  createdAt: number;
}

export interface SessionUser {
  id: string;
  username: string;
  role: UserRecord["role"];
}

export interface UserSummary extends SessionUser {
  disabled: boolean;
  createdAt: number;
}

export interface AuditRecord {
  id: number;
  username: string | null;
  action: string;
  targetType: string | null;
  targetId: string | null;
  details: unknown;
  ipAddress: string | null;
  createdAt: number;
}

export interface SessionSummary {
  id: string;
  createdAt: number;
  expiresAt: number;
  lastSeenAt: number;
  ipAddress: string | null;
  userAgent: string | null;
  current: boolean;
}

export interface LoginThrottle {
  failures: number;
  blockedUntil: number;
}

let database: DatabaseSync | null = null;

export function getDatabase(): DatabaseSync {
  if (database) return database;

  const configuredPath = process.env.PANEL_DB_PATH;
  const dbPath =
    configuredPath ||
    (process.env.NODE_ENV === "production"
      ? "/data/panel.db"
      : path.resolve("data/panel.db"));

  if (dbPath !== ":memory:") {
    fs.mkdirSync(path.dirname(dbPath), { recursive: true, mode: 0o700 });
  }

  database = new DatabaseSync(dbPath, {
    enableForeignKeyConstraints: true,
    timeout: 5000,
  });
  if (dbPath !== ":memory:") fs.chmodSync(dbPath, 0o600);
  database.exec(`
    PRAGMA journal_mode = WAL;
    PRAGMA foreign_keys = ON;

    CREATE TABLE IF NOT EXISTS users (
      id TEXT PRIMARY KEY,
      username TEXT NOT NULL UNIQUE COLLATE NOCASE,
      password_hash TEXT NOT NULL,
      role TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
      disabled INTEGER NOT NULL DEFAULT 0,
      created_at INTEGER NOT NULL,
      updated_at INTEGER NOT NULL
    ) STRICT;

    CREATE TABLE IF NOT EXISTS sessions (
      token_hash TEXT PRIMARY KEY,
      session_id TEXT UNIQUE,
      user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
      created_at INTEGER NOT NULL,
      expires_at INTEGER NOT NULL,
      last_seen_at INTEGER NOT NULL,
      ip_address TEXT,
      user_agent TEXT
    ) STRICT;

    CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions(user_id);
    CREATE INDEX IF NOT EXISTS sessions_expires_at_idx ON sessions(expires_at);

    CREATE TABLE IF NOT EXISTS audit_log (
      id INTEGER PRIMARY KEY AUTOINCREMENT,
      user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
      action TEXT NOT NULL,
      target_type TEXT,
      target_id TEXT,
      details_json TEXT,
      ip_address TEXT,
      created_at INTEGER NOT NULL
    ) STRICT;

    CREATE INDEX IF NOT EXISTS audit_log_created_at_idx
      ON audit_log(created_at DESC);

    CREATE TABLE IF NOT EXISTS login_attempts (
      attempt_key TEXT PRIMARY KEY,
      failures INTEGER NOT NULL,
      window_started_at INTEGER NOT NULL,
      blocked_until INTEGER NOT NULL,
      updated_at INTEGER NOT NULL
    ) STRICT;
  `);

  const sessionColumns = database
    .prepare("PRAGMA table_info(sessions)")
    .all() as Array<{ name: string }>;
  if (!sessionColumns.some((column) => column.name === "session_id")) {
    database.exec("ALTER TABLE sessions ADD COLUMN session_id TEXT");
    database.exec(
      "CREATE UNIQUE INDEX IF NOT EXISTS sessions_session_id_idx ON sessions(session_id)"
    );
  }
  const legacySessions = database
    .prepare("SELECT token_hash FROM sessions WHERE session_id IS NULL")
    .all() as Array<{ token_hash: string }>;
  const assignSessionId = database.prepare(
    "UPDATE sessions SET session_id = ? WHERE token_hash = ?"
  );
  for (const session of legacySessions) {
    assignSessionId.run(randomUUID(), session.token_hash);
  }

  return database;
}

export function getLoginThrottle(
  attemptKey: string,
  now: number,
  windowMs: number
): LoginThrottle {
  const row = getDatabase()
    .prepare(
      `SELECT failures, window_started_at, blocked_until
       FROM login_attempts WHERE attempt_key = ?`
    )
    .get(attemptKey) as
    | { failures: number; window_started_at: number; blocked_until: number }
    | undefined;

  if (
    !row ||
    (row.blocked_until <= now && now - row.window_started_at >= windowMs)
  ) {
    if (row) clearLoginThrottle(attemptKey);
    return { failures: 0, blockedUntil: 0 };
  }
  return { failures: row.failures, blockedUntil: row.blocked_until };
}

export function recordLoginFailure(
  attemptKey: string,
  now: number,
  windowMs: number,
  maxFailures: number
): LoginThrottle {
  const current = getLoginThrottle(attemptKey, now, windowMs);
  const failures = current.failures + 1;
  const blockedUntil =
    failures >= maxFailures ? now + windowMs : current.blockedUntil;
  const existing = getDatabase()
    .prepare(
      "SELECT window_started_at FROM login_attempts WHERE attempt_key = ?"
    )
    .get(attemptKey) as { window_started_at: number } | undefined;
  getDatabase()
    .prepare(
      `INSERT INTO login_attempts
        (attempt_key, failures, window_started_at, blocked_until, updated_at)
       VALUES (?, ?, ?, ?, ?)
       ON CONFLICT(attempt_key) DO UPDATE SET
         failures = excluded.failures,
         blocked_until = excluded.blocked_until,
         updated_at = excluded.updated_at`
    )
    .run(
      attemptKey,
      failures,
      existing?.window_started_at || now,
      blockedUntil,
      now
    );
  return { failures, blockedUntil };
}

export function clearLoginThrottle(attemptKey: string): void {
  getDatabase()
    .prepare("DELETE FROM login_attempts WHERE attempt_key = ?")
    .run(attemptKey);
}

export function countUsers(): number {
  const row = getDatabase().prepare("SELECT COUNT(*) AS count FROM users").get() as {
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
       VALUES (?, ?, ?, ?, ?, ?, ?)`
    )
    .run(
      user.id,
      user.username,
      user.passwordHash,
      user.role,
      user.disabled ? 1 : 0,
      user.createdAt,
      user.createdAt
    );
}

export function findUserByUsername(username: string): UserRecord | null {
  const row = getDatabase()
    .prepare(
      `SELECT id, username, password_hash, role, disabled, created_at
       FROM users WHERE username = ?`
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
    | undefined;

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
       FROM users WHERE id = ?`
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
    | undefined;
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
       FROM users ORDER BY username COLLATE NOCASE`
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
      "SELECT COUNT(*) AS count FROM users WHERE role = 'admin' AND disabled = 0"
    )
    .get() as { count: number };
  return row.count;
}

export function updateUserAccess(
  id: string,
  role: UserRecord["role"],
  disabled: boolean
): void {
  getDatabase()
    .prepare(
      "UPDATE users SET role = ?, disabled = ?, updated_at = ? WHERE id = ?"
    )
    .run(role, disabled ? 1 : 0, Date.now(), id);
  if (disabled) deleteUserSessions(id);
}

export function updateUserPassword(id: string, passwordHash: string): void {
  getDatabase()
    .prepare(
      "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?"
    )
    .run(passwordHash, Date.now(), id);
  deleteUserSessions(id);
}

export function upgradeUserPasswordHash(
  id: string,
  passwordHash: string
): void {
  getDatabase()
    .prepare(
      "UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?"
    )
    .run(passwordHash, Date.now(), id);
}

export function deleteUser(id: string): void {
  getDatabase().prepare("DELETE FROM users WHERE id = ?").run(id);
}

export function deleteUserSessions(userId: string): void {
  getDatabase().prepare("DELETE FROM sessions WHERE user_id = ?").run(userId);
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
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
  ).run(
    input.tokenHash,
    input.sessionId,
    input.userId,
    input.createdAt,
    input.expiresAt,
    input.createdAt,
    input.ipAddress || null,
    input.userAgent || null
  );
}

export function listUserSessions(
  userId: string,
  currentTokenHash: string
): SessionSummary[] {
  const rows = getDatabase()
    .prepare(
      `SELECT session_id, token_hash, created_at, expires_at, last_seen_at,
              ip_address, user_agent
       FROM sessions
       WHERE user_id = ? AND expires_at > ?
       ORDER BY last_seen_at DESC`
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
      Boolean(row.session_id)
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
  sessionId: string
): boolean {
  const result = getDatabase()
    .prepare("DELETE FROM sessions WHERE user_id = ? AND session_id = ?")
    .run(userId, sessionId);
  return result.changes > 0;
}

export function findSessionUser(
  tokenHash: string,
  now: number
): SessionUser | null {
  const row = getDatabase()
    .prepare(
      `SELECT users.id, users.username, users.role, users.disabled, sessions.expires_at
       FROM sessions
       JOIN users ON users.id = sessions.user_id
       WHERE sessions.token_hash = ?`
    )
    .get(tokenHash) as
    | {
        id: string;
        username: string;
        role: UserRecord["role"];
        disabled: number;
        expires_at: number;
      }
    | undefined;

  if (!row || row.disabled === 1 || row.expires_at <= now) {
    if (row) deleteSessionRecord(tokenHash);
    return null;
  }

  getDatabase()
    .prepare("UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?")
    .run(now, tokenHash);

  return { id: row.id, username: row.username, role: row.role };
}

export function deleteSessionRecord(tokenHash: string): void {
  getDatabase()
    .prepare("DELETE FROM sessions WHERE token_hash = ?")
    .run(tokenHash);
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
       VALUES (?, ?, ?, ?, ?, ?, ?)`
    )
    .run(
      input.userId || null,
      input.action,
      input.targetType || null,
      input.targetId || null,
      input.details === undefined ? null : JSON.stringify(input.details),
      input.ipAddress || null,
      Date.now()
    );
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
       LIMIT ?`
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
    details: row.details_json ? JSON.parse(row.details_json) : null,
    ipAddress: row.ip_address,
    createdAt: row.created_at,
  }));
}

export function closeDatabase(): void {
  database?.close();
  database = null;
}
