package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ApplicationID marks Ludock-owned SQLite files ("LDCK") independently of the
// schema version. Never infer ownership merely from a familiar table name.
// Releases up to 0.5 used "LUDK" with a different schema; Ludock refuses
// those files without writing to them.
const ApplicationID = 0x4c44434b

// BaseSchemaVersion is the initial schema. Add later changes as consecutive
// migrations; never edit a released one.
const BaseSchemaVersion = 1

// Migration is one schema version. Upgrade runs inside the migration
// transaction after SQL.
type Migration struct {
	Version int
	SQL     string
	Upgrade func(tx *sql.Tx, key []byte) error
}

// ErrIncompatible reports a database that Ludock does not own or cannot upgrade.
var ErrIncompatible = errors.New("This database is incompatible with Ludock. Use a new application data volume or LUDOCK_DATA_DIR and complete first-administrator setup. The existing database and game data have not been changed.")

// Migrations is the complete schema history.
var Migrations = []Migration{
	{
		Version: BaseSchemaVersion,
		SQL: `
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
      -- Server lists and the runner read active work on every refresh.
      CREATE INDEX operations_active_server_idx ON operations(server_id) WHERE status IN ('queued','running');
      CREATE INDEX operations_queued_idx ON operations(created_at, id) WHERE status = 'queued';
      CREATE TABLE backups (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        binding_fingerprint TEXT NOT NULL, destination TEXT NOT NULL,
        roots_json TEXT NOT NULL, size INTEGER NOT NULL, checksum TEXT NOT NULL,
        created_at INTEGER NOT NULL, state TEXT NOT NULL
      ) STRICT;
      CREATE INDEX backups_server_idx ON backups(server_id, created_at DESC, id);
      CREATE TABLE schedules (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        input_json TEXT NOT NULL, binding_revision INTEGER NOT NULL,
        last_slot TEXT, last_result TEXT, created_at INTEGER NOT NULL,
        revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
        last_operation_id TEXT, last_run_at INTEGER CHECK (last_run_at >= 0)
      ) STRICT;
      CREATE INDEX schedules_server_idx ON schedules(server_id, created_at, id);
      -- Claimed countdown warnings, so a restart never repeats one. Repeated
      -- fall-back hours share a slot, so each lead time is claimed separately.
      CREATE TABLE schedule_announcements (
        schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
        slot TEXT NOT NULL, lead_minutes INTEGER NOT NULL,
        PRIMARY KEY (schedule_id, slot, lead_minutes)
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
		Upgrade: initializeKeyCheck,
	},
	{
		// Schedule runs look up the accepted binding of a revision, and every
		// tick looks for backups still being deleted.
		Version: 2,
		SQL: `
      CREATE INDEX server_bindings_revision_idx ON server_bindings(server_id, binding_revision) WHERE accepted = 1;
      CREATE INDEX backups_deleting_idx ON backups(created_at, id) WHERE state = 'deleting';
    `,
	},
	{
		// Resource history: one sample per running server a minute, kept for a
		// day. CPU is in hundredths of a percent and memory in KiB, so a row
		// stays a few dozen bytes.
		Version: 3,
		SQL: `
      CREATE TABLE resource_samples (
        server_id TEXT NOT NULL REFERENCES logical_servers(id) ON DELETE CASCADE,
        at INTEGER NOT NULL, cpu INTEGER NOT NULL CHECK (cpu >= 0), memory INTEGER NOT NULL CHECK (memory >= 0),
        PRIMARY KEY (server_id, at)
      ) STRICT, WITHOUT ROWID;
    `,
	},
	{
		// Notifications go to several destinations: each event is delivered to
		// each one separately, and the message is formatted when it is sent.
		// Discord, the only earlier destination, keeps its settings and queue.
		Version: 4,
		SQL: `
      CREATE TABLE notification_deliveries_v4 (
        id TEXT PRIMARY KEY, event_key TEXT NOT NULL, destination TEXT NOT NULL, message TEXT NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
        state TEXT NOT NULL DEFAULT 'queued', created_at INTEGER NOT NULL,
        kind TEXT NOT NULL DEFAULT 'event' CHECK (kind IN ('event','test')),
        retry_attempts INTEGER NOT NULL DEFAULT 0 CHECK (retry_attempts >= 0),
        last_attempt_at INTEGER CHECK (last_attempt_at >= 0),
        delivered_at INTEGER CHECK (delivered_at >= 0),
        failure_code TEXT,
        UNIQUE (event_key, destination)
      ) STRICT;
      INSERT INTO notification_deliveries_v4 (id, event_key, destination, message, attempts, next_attempt_at, state, created_at,
        kind, retry_attempts, last_attempt_at, delivered_at, failure_code)
        SELECT id, event_key, 'discord', COALESCE(json_extract(payload_json, '$.content'), ''), attempts, next_attempt_at, state,
          created_at, kind, retry_attempts, last_attempt_at, delivered_at, failure_code FROM notification_deliveries;
      DROP TABLE notification_deliveries;
      ALTER TABLE notification_deliveries_v4 RENAME TO notification_deliveries;
      CREATE INDEX notification_deliveries_recent_idx ON notification_deliveries(created_at DESC, id DESC);
      CREATE INDEX notification_deliveries_due_idx ON notification_deliveries(state, next_attempt_at);
      UPDATE settings SET value_json = json_object('discord', json_object(
          'enabled', json(CASE WHEN json_extract(value_json, '$.enabled') THEN 'true' ELSE 'false' END),
          'url', COALESCE(json_extract(value_json, '$.webhookUrl'), '')))
        WHERE key = 'notifications';
    `,
	},
}

func LatestVersion() int { return Migrations[len(Migrations)-1].Version }

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func pragmaInt(ctx context.Context, q queryer, name string) (int64, error) {
	var value int64
	err := q.QueryRowContext(ctx, "PRAGMA "+name).Scan(&value)
	return value, err
}

// AssertCompatible accepts an empty database or a Ludock database whose
// schema version is within [earliest, latest].
func AssertCompatible(ctx context.Context, q queryer, earliest, latest int) error {
	marker, err := pragmaInt(ctx, q, "application_id")
	if err != nil {
		return err
	}
	version, err := pragmaInt(ctx, q, "user_version")
	if err != nil {
		return err
	}
	var tables int
	if err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if marker == 0 && version == 0 && tables == 0 {
		return nil
	}
	if marker != ApplicationID || version < int64(earliest) || version > int64(latest) {
		return ErrIncompatible
	}
	return nil
}

// ApplyMigrations upgrades the schema in one immediate transaction, so a
// failed upgrade never leaves half a schema behind.
func ApplyMigrations(ctx context.Context, conn *sql.Conn, migrations []Migration, key []byte) error {
	if len(migrations) == 0 || migrations[0].Version != BaseSchemaVersion {
		return fmt.Errorf("database migrations must start at version %d", BaseSchemaVersion)
	}
	first := migrations[0].Version
	for index, migration := range migrations {
		if migration.Version != first+index {
			return errors.New("database migrations must have consecutive versions")
		}
	}
	latest := migrations[len(migrations)-1].Version
	if err := AssertCompatible(ctx, conn, first, latest); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := pragmaInt(ctx, tx, "user_version")
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if int64(migration.Version) <= current {
			continue
		}
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			return err
		}
		if migration.Upgrade != nil {
			if err := migration.Upgrade(tx, key); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", migration.Version)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", ApplicationID)); err != nil {
		return err
	}
	return tx.Commit()
}
