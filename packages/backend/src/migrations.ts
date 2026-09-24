import type { Database } from "bun:sqlite";
import { initializeFingerprintKey } from "./fingerprints.js";

// The "LUDK" marker identifies Ludock-owned SQLite files independently of the
// schema version. Never infer ownership merely from a familiar table name.
export const DATABASE_APPLICATION_ID = 0x4c55444b;
// Version 6 is the schema released in v0.3.0. Earlier development schemas have
// no supported upgrade path; add later changes as consecutive migrations.
export const DATABASE_BASE_SCHEMA_VERSION = 6;

interface DatabaseMigration {
  version: number;
  sql: string;
  upgrade?(db: Database, fingerprintKey?: Uint8Array): void;
}

export const DATABASE_MIGRATIONS: readonly DatabaseMigration[] = [
  {
    version: DATABASE_BASE_SCHEMA_VERSION,
    sql: `
      CREATE TABLE users (
        id TEXT PRIMARY KEY,
        username TEXT NOT NULL UNIQUE COLLATE NOCASE,
        password_hash TEXT NOT NULL,
        role TEXT NOT NULL CHECK (role IN ('admin', 'operator', 'viewer')),
        disabled INTEGER NOT NULL DEFAULT 0,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL
      ) STRICT;
      CREATE TABLE sessions (
        token_hash TEXT PRIMARY KEY,
        session_id TEXT NOT NULL UNIQUE,
        user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        created_at INTEGER NOT NULL,
        expires_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL,
        ip_address TEXT,
        user_agent TEXT
      ) STRICT;
      CREATE INDEX sessions_user_id_idx ON sessions(user_id);
      CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);
      CREATE TABLE audit_log (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
        action TEXT NOT NULL,
        target_type TEXT,
        target_id TEXT,
        details_json TEXT,
        ip_address TEXT,
        created_at INTEGER NOT NULL
      ) STRICT;
      CREATE INDEX audit_log_history_idx ON audit_log(created_at DESC, id DESC);
      CREATE INDEX audit_log_server_history_idx ON audit_log(target_type, target_id, created_at DESC, id DESC);
      CREATE INDEX audit_log_operation_history_idx ON audit_log(
        CASE WHEN json_valid(details_json) THEN CASE WHEN json_type(details_json, '$.operationId') = 'text' THEN json_extract(details_json, '$.operationId') END END,
        created_at DESC, id DESC
      );
      CREATE TABLE login_attempts (
        attempt_key TEXT PRIMARY KEY,
        failures INTEGER NOT NULL,
        window_started_at INTEGER NOT NULL,
        blocked_until INTEGER NOT NULL,
        updated_at INTEGER NOT NULL
      ) STRICT;
      CREATE TABLE docker_hosts (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL UNIQUE,
        created_at INTEGER NOT NULL
      ) STRICT;
      CREATE TABLE logical_servers (
        id TEXT PRIMARY KEY,
        host_id TEXT NOT NULL REFERENCES docker_hosts(id),
        external_identity TEXT NOT NULL,
        container_id TEXT,
        display_name TEXT NOT NULL,
        game_type TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('active','missing','ambiguous','review_required')),
        binding_revision INTEGER NOT NULL,
        binding_fingerprint TEXT NOT NULL,
        pending_fingerprint TEXT,
        pending_game_type TEXT,
        review_required INTEGER NOT NULL DEFAULT 0 CHECK (review_required IN (0,1)),
        first_seen_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL,
        UNIQUE (host_id, external_identity)
      ) STRICT;
      CREATE INDEX logical_servers_container_idx ON logical_servers(host_id, container_id);
      CREATE TABLE server_bindings (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        server_id TEXT NOT NULL REFERENCES logical_servers(id),
        binding_revision INTEGER NOT NULL,
        container_id TEXT NOT NULL,
        binding_fingerprint TEXT NOT NULL,
        observed_at INTEGER NOT NULL,
        accepted INTEGER NOT NULL CHECK (accepted IN (0,1))
      ) STRICT;
      CREATE INDEX server_bindings_server_idx ON server_bindings(server_id, observed_at DESC);
      CREATE TABLE server_grants (
        user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        server_id TEXT NOT NULL REFERENCES logical_servers(id) ON DELETE CASCADE,
        capabilities_json TEXT NOT NULL,
        updated_at INTEGER NOT NULL,
        PRIMARY KEY (user_id, server_id)
      ) STRICT;
      CREATE TABLE settings (key TEXT PRIMARY KEY, value_json TEXT NOT NULL) STRICT;
      CREATE TABLE operations (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        actor_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL,
        phase TEXT NOT NULL, input_json TEXT NOT NULL, recovery_json TEXT NOT NULL DEFAULT '{}',
        binding_revision INTEGER NOT NULL, request_key TEXT UNIQUE,
        created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
        error TEXT, result_json TEXT
      ) STRICT;
      CREATE INDEX operations_history_idx ON operations(created_at DESC, id DESC);
      CREATE INDEX operations_server_history_idx ON operations(server_id, created_at DESC, id DESC);
      CREATE TABLE backups (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        binding_fingerprint TEXT NOT NULL, destination TEXT NOT NULL,
        roots_json TEXT NOT NULL, size INTEGER NOT NULL, checksum TEXT NOT NULL,
        created_at INTEGER NOT NULL, state TEXT NOT NULL
      ) STRICT;
      CREATE TABLE schedules (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        input_json TEXT NOT NULL, binding_revision INTEGER NOT NULL,
        last_slot TEXT, last_result TEXT, created_at INTEGER NOT NULL,
        revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
        last_operation_id TEXT, last_run_at INTEGER CHECK (last_run_at >= 0)
      ) STRICT;
      CREATE TABLE availability (
        server_id TEXT PRIMARY KEY REFERENCES logical_servers(id), policy_json TEXT NOT NULL,
        outage_started_at INTEGER, notified INTEGER NOT NULL DEFAULT 0,
        suppressed_until INTEGER NOT NULL DEFAULT 0, intentionally_stopped INTEGER NOT NULL DEFAULT 0,
        last_state TEXT, updated_at INTEGER NOT NULL
      ) STRICT;
      CREATE TABLE notification_deliveries (
        id TEXT PRIMARY KEY, event_key TEXT NOT NULL UNIQUE, payload_json TEXT NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
        state TEXT NOT NULL DEFAULT 'queued', created_at INTEGER NOT NULL,
        kind TEXT NOT NULL DEFAULT 'event' CHECK (kind IN ('event','test')),
        retry_attempts INTEGER NOT NULL DEFAULT 0 CHECK (retry_attempts >= 0),
        last_attempt_at INTEGER CHECK (last_attempt_at >= 0),
        delivered_at INTEGER CHECK (delivered_at >= 0),
        failure_code TEXT
      ) STRICT;
      CREATE INDEX notification_deliveries_recent_idx ON notification_deliveries(created_at DESC, id DESC);
      CREATE INDEX notification_deliveries_due_idx ON notification_deliveries(state, next_attempt_at);
    `,
    upgrade: initializeFingerprintKey,
  },
];

function schemaVersion(db: Database): number {
  return (db.prepare("PRAGMA user_version").get() as { user_version: number })
    .user_version;
}

function incompatibleDatabaseError(): Error {
  const error = new Error(
    "This database is incompatible with Ludock. Use a new application data volume or LUDOCK_DB_PATH and complete first-administrator setup. The existing database and game data have not been changed.",
  );
  Object.assign(error, { code: "INCOMPATIBLE_DATABASE" });
  return error;
}

export function assertCompatibleDatabase(
  db: Database,
  latestVersion = DATABASE_MIGRATIONS.at(-1)!.version,
  earliestVersion = DATABASE_MIGRATIONS[0].version,
): void {
  const marker = (
    db.prepare("PRAGMA application_id").get() as { application_id: number }
  ).application_id;
  const version = schemaVersion(db);
  const tables = db
    .prepare(
      "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'",
    )
    .all();
  if (marker === 0 && version === 0 && tables.length === 0) return;
  if (
    marker !== DATABASE_APPLICATION_ID ||
    version < earliestVersion ||
    version > latestVersion
  ) {
    throw incompatibleDatabaseError();
  }
}

export function applyMigrations(
  db: Database,
  migrations: readonly DatabaseMigration[] = DATABASE_MIGRATIONS,
  fingerprintKey?: Uint8Array,
): void {
  const firstVersion = migrations[0]?.version ?? 0;
  if (firstVersion !== DATABASE_BASE_SCHEMA_VERSION)
    throw new Error(
      `Database migrations must start at version ${DATABASE_BASE_SCHEMA_VERSION}`,
    );
  for (let index = 0; index < migrations.length; index += 1) {
    if (firstVersion < 1 || migrations[index].version !== firstVersion + index) {
      throw new Error(
        "Database migrations must have consecutive versions",
      );
    }
  }
  const latestVersion = migrations.at(-1)?.version ?? 0;
  assertCompatibleDatabase(db, latestVersion, firstVersion);
  // One transaction ensures a failed upgrade never leaves half a schema behind.
  db.exec("BEGIN IMMEDIATE");
  try {
    const current = schemaVersion(db);
    for (const migration of migrations) {
      if (migration.version <= current) continue;
      db.exec(migration.sql);
      migration.upgrade?.(db, fingerprintKey);
      db.exec(`PRAGMA user_version = ${migration.version}`);
    }
    db.exec(`PRAGMA application_id = ${DATABASE_APPLICATION_ID}`);
    db.exec("COMMIT");
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
}
