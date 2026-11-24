package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/Crunging/Ludock/internal/apperr"
)

// ApplicationID marks a Ludock 1.x database ("LDK1"), whatever its schema
// version.
const ApplicationID = 0x4c444b31

// betaApplicationIDs are the markers betas wrote: "LUDK" up to 0.5 and
// "LDCK" from 0.6. 1.0 started fresh, so their data is refused.
var betaApplicationIDs = []int64{0x4c55444b, 0x4c44434b}

// BaseSchemaVersion is the schema 1.0 ships with. Add later changes as
// consecutive migrations; never edit a released one.
const BaseSchemaVersion = 1

// Migration is one schema version. Upgrade runs inside the migration
// transaction after SQL.
type Migration struct {
	Version int
	SQL     string
	Upgrade func(tx *sql.Tx, key []byte) error
}

// Every refusal shares one code: the server waits with its message instead
// of exiting.
var (
	ErrIncompatible  = apperr.New("DATABASE_INCOMPATIBLE", 500, "The data folder holds a database Ludock can’t use: it isn’t Ludock’s, or it is from an early Ludock beta. Start Ludock with a new, empty data volume (or LUDOCK_DATA_DIR) and set up the first administrator. Nothing was changed: the old database, your game servers, and your backup archives are as they were.")
	ErrBetaDatabase  = apperr.New("DATABASE_INCOMPATIBLE", 500, "This data is from a Ludock beta, and Ludock 1.0 starts fresh, so it can’t use it. Start Ludock with a new, empty data volume (or LUDOCK_DATA_DIR) and set up the first administrator; keep the old volume or remove it, as you like. Nothing was changed: your game servers and your backup archives are not affected.")
	ErrNewerDatabase = apperr.New("DATABASE_INCOMPATIBLE", 500, "This data was written by a newer version of Ludock. Run that version or a later one. Nothing was changed.")
)

// Migrations is the complete schema history.
var Migrations = []Migration{
	{
		Version: BaseSchemaVersion,
		SQL: `
      -- TOTP secrets are sealed with the identity key. totp_step is the last
      -- accepted time step, so a code works once.
      CREATE TABLE users (
        id TEXT PRIMARY KEY,
        username TEXT NOT NULL UNIQUE COLLATE NOCASE,
        password_hash TEXT NOT NULL,
        role TEXT NOT NULL CHECK (role IN ('admin', 'member')),
        disabled INTEGER NOT NULL DEFAULT 0,
        created_at INTEGER NOT NULL,
        updated_at INTEGER NOT NULL,
        totp_secret TEXT,
        totp_pending TEXT,
        totp_step INTEGER NOT NULL DEFAULT 0 CHECK (totp_step >= 0)
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
      CREATE TABLE recovery_codes (
        user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        code_hash TEXT NOT NULL,
        PRIMARY KEY (user_id, code_hash)
      ) STRICT, WITHOUT ROWID;
      -- The password step of a two-factor sign-in, bound to the password hash
      -- it verified.
      CREATE TABLE sign_in_challenges (
        token_hash TEXT PRIMARY KEY,
        user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        password_hash TEXT NOT NULL,
        attempts INTEGER NOT NULL DEFAULT 0,
        expires_at INTEGER NOT NULL
      ) STRICT;
      CREATE INDEX sign_in_challenges_user_idx ON sign_in_challenges(user_id);
      -- One invite or password reset link per account. It works only while
      -- the account still has password_hash; an invited account's is empty.
      CREATE TABLE account_links (
        token_hash TEXT PRIMARY KEY,
        user_id TEXT NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
        purpose TEXT NOT NULL CHECK (purpose IN ('invite', 'reset')),
        password_hash TEXT NOT NULL,
        created_at INTEGER NOT NULL,
        expires_at INTEGER NOT NULL
      ) STRICT;
      CREATE TABLE login_attempts (
        attempt_key TEXT PRIMARY KEY,
        failures INTEGER NOT NULL,
        window_started_at INTEGER NOT NULL,
        blocked_until INTEGER NOT NULL,
        updated_at INTEGER NOT NULL
      ) STRICT;
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
      CREATE TABLE docker_hosts (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL UNIQUE,
        created_at INTEGER NOT NULL
      ) STRICT;
      -- sharing_paused: a change to the server's game or data folders paused
      -- its grants until an administrator confirms it.
      CREATE TABLE logical_servers (
        id TEXT PRIMARY KEY,
        host_id TEXT NOT NULL REFERENCES docker_hosts(id),
        external_identity TEXT NOT NULL,
        container_id TEXT,
        display_name TEXT NOT NULL,
        game_type TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('active','missing','ambiguous')),
        binding_revision INTEGER NOT NULL,
        binding_fingerprint TEXT NOT NULL,
        sharing_paused INTEGER NOT NULL DEFAULT 0 CHECK (sharing_paused IN (0,1)),
        first_seen_at INTEGER NOT NULL,
        last_seen_at INTEGER NOT NULL,
        UNIQUE (host_id, external_identity)
      ) STRICT;
      CREATE INDEX logical_servers_container_idx ON logical_servers(host_id, container_id);
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
      -- Retention never removes a kept backup (kept_at set); reason names the
      -- work that took one for its own protection. game_type lets a backup
      -- restore onto the same folders after they move to other host storage.
      -- The second copy: copy_state is '' (none), 'pending', 'complete',
      -- 'failed', or 'removing' (its backup was removed); copy_next_at is when
      -- the copier tries again; copy_previous is a folder holding a verified
      -- copy from before the second folder changed, kept until the new one
      -- has one.
      CREATE TABLE backups (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        binding_fingerprint TEXT NOT NULL, destination TEXT NOT NULL,
        roots_json TEXT NOT NULL, size INTEGER NOT NULL, checksum TEXT NOT NULL,
        created_at INTEGER NOT NULL, state TEXT NOT NULL, game_type TEXT NOT NULL,
        kept_at INTEGER CHECK (kept_at >= 0),
        note TEXT NOT NULL DEFAULT '',
        reason TEXT NOT NULL DEFAULT '' CHECK (reason IN ('', 'restore', 'update')),
        copy_destination TEXT NOT NULL DEFAULT '',
        copy_state TEXT NOT NULL DEFAULT '' CHECK (copy_state IN ('', 'pending', 'complete', 'failed', 'removing')),
        copy_attempts INTEGER NOT NULL DEFAULT 0 CHECK (copy_attempts >= 0),
        copy_next_at INTEGER NOT NULL DEFAULT 0,
        copy_error TEXT NOT NULL DEFAULT '',
        copy_previous TEXT NOT NULL DEFAULT ''
      ) STRICT;
      CREATE INDEX backups_server_idx ON backups(server_id, created_at DESC, id);
      CREATE INDEX backups_deleting_idx ON backups(created_at, id) WHERE state = 'deleting';
      CREATE INDEX backups_copy_idx ON backups(copy_next_at, created_at, id) WHERE copy_state IN ('pending', 'failed');
      -- A recurring run may start late, but never for a minute before
      -- changed_at, when the schedule was last saved. owner_id is NULL once
      -- the owner's account is deleted; the schedule is suspended until an
      -- administrator takes it over.
      CREATE TABLE schedules (
        id TEXT PRIMARY KEY, server_id TEXT NOT NULL REFERENCES logical_servers(id),
        owner_id TEXT REFERENCES users(id) ON DELETE SET NULL,
        input_json TEXT NOT NULL, binding_revision INTEGER NOT NULL,
        created_at INTEGER NOT NULL, changed_at INTEGER NOT NULL CHECK (changed_at >= 0),
        revision INTEGER NOT NULL DEFAULT 1 CHECK (revision > 0),
        last_slot TEXT, last_result TEXT,
        last_operation_id TEXT, last_run_at INTEGER CHECK (last_run_at >= 0)
      ) STRICT;
      CREATE INDEX schedules_server_idx ON schedules(server_id, created_at, id);
      -- Claimed countdown warnings, so a restart never repeats one. Repeated
      -- fall-back hours share a slot, so each lead time is claimed separately.
      -- heard_binding is the binding revision a warning was delivered on;
      -- NULL while it is only claimed, or when it never reached players.
      CREATE TABLE schedule_announcements (
        schedule_id TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
        slot TEXT NOT NULL, lead_minutes INTEGER NOT NULL,
        heard_binding INTEGER,
        PRIMARY KEY (schedule_id, slot, lead_minutes)
      ) STRICT;
      -- A server's monitoring policy and the disk, memory, and recovery state
      -- judged by it. answered_json is when the game last answered in its
      -- container run.
      CREATE TABLE availability (
        server_id TEXT PRIMARY KEY REFERENCES logical_servers(id), policy_json TEXT NOT NULL,
        outage_started_at INTEGER, notified INTEGER NOT NULL DEFAULT 0,
        suppressed_until INTEGER NOT NULL DEFAULT 0, intentionally_stopped INTEGER NOT NULL DEFAULT 0,
        last_state TEXT, updated_at INTEGER NOT NULL,
        disk_json TEXT, memory_json TEXT, recovery_json TEXT, answered_json TEXT
      ) STRICT;
      -- Each event is delivered to each destination separately, and the
      -- message is formatted when it is sent.
      CREATE TABLE notification_deliveries (
        id TEXT PRIMARY KEY, event_key TEXT NOT NULL, destination TEXT NOT NULL, message TEXT NOT NULL,
        kind TEXT NOT NULL CHECK (kind IN ('event','test')),
        attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL,
        state TEXT NOT NULL DEFAULT 'queued', created_at INTEGER NOT NULL,
        retry_attempts INTEGER NOT NULL DEFAULT 0 CHECK (retry_attempts >= 0),
        last_attempt_at INTEGER CHECK (last_attempt_at >= 0),
        delivered_at INTEGER CHECK (delivered_at >= 0),
        failure_code TEXT,
        UNIQUE (event_key, destination)
      ) STRICT;
      CREATE INDEX notification_deliveries_recent_idx ON notification_deliveries(created_at DESC, id DESC);
      CREATE INDEX notification_deliveries_due_idx ON notification_deliveries(state, next_attempt_at);
      -- One sample per running server a minute, kept for a day. CPU is in
      -- hundredths of a percent and memory in KiB.
      CREATE TABLE resource_samples (
        server_id TEXT NOT NULL REFERENCES logical_servers(id) ON DELETE CASCADE,
        at INTEGER NOT NULL, cpu INTEGER NOT NULL CHECK (cpu >= 0), memory INTEGER NOT NULL CHECK (memory >= 0),
        PRIMARY KEY (server_id, at)
      ) STRICT, WITHOUT ROWID;
    `,
		Upgrade: initializeKeyCheck,
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

// AssertCompatible accepts an empty database or a Ludock 1.x database whose
// schema version is at most latest. The marker, not the version,
// tells a beta's database apart: betas numbered their schemas from 1 too.
func AssertCompatible(ctx context.Context, q queryer, latest int) error {
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
		`SELECT COUNT(*) FROM sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\'`).Scan(&tables); err != nil {
		return err
	}
	if marker == 0 && version == 0 && tables == 0 {
		return nil
	}
	switch {
	case slices.Contains(betaApplicationIDs, marker):
		return ErrBetaDatabase
	case marker == ApplicationID && version > int64(latest):
		return ErrNewerDatabase
	case marker != ApplicationID || version < BaseSchemaVersion:
		return ErrIncompatible
	}
	return nil
}

// ApplyMigrations brings the schema to the latest version in one immediate
// transaction, so a failed migration never leaves half a schema behind.
func ApplyMigrations(ctx context.Context, conn *sql.Conn, migrations []Migration, key []byte) error {
	if len(migrations) == 0 || migrations[0].Version != BaseSchemaVersion {
		return fmt.Errorf("database migrations must start at version %d", BaseSchemaVersion)
	}
	for index, migration := range migrations {
		if migration.Version != BaseSchemaVersion+index {
			return errors.New("database migrations must have consecutive versions")
		}
	}
	latest := migrations[len(migrations)-1].Version
	if err := AssertCompatible(ctx, conn, latest); err != nil {
		return err
	}
	// A migration may rebuild a table others reference, which SQLite allows
	// only with foreign keys off; they are checked before committing.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
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
	violations, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	broken := violations.Next()
	violations.Close()
	if broken {
		return errors.New("database migration broke a foreign key")
	}
	return tx.Commit()
}
