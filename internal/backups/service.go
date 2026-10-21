package backups

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// Backup is a stored backup.
type Backup struct {
	ID        string
	ServerID  string
	Roots     []Root
	Size      int64
	Checksum  string
	CreatedAt int64
	State     string
	// fingerprint and destination are internal.
	fingerprint string
	destination string
}

// Fingerprint is the binding fingerprint the backup was made from.
func (b *Backup) Fingerprint() string { return b.fingerprint }

// Destination is where the archive is stored.
func (b *Backup) Destination() string { return b.destination }

// Service manages backup records and settings.
type Service struct {
	DB       *db.DB
	Storage  *Storage
	Helpers  *Helpers
	Docker   *docker.Client
	Observer *servers.Observer
	Locks    *locks.Table

	initMu  sync.Mutex
	initRun bool
}

const settingsKey = "backups"

var errNotFound = fail("BACKUP_NOT_FOUND", "Backup not found.")

const backupColumns = "id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state"

func scanBackup(s interface{ Scan(...any) error }) (*Backup, error) {
	var backup Backup
	var roots string
	if err := s.Scan(&backup.ID, &backup.ServerID, &backup.fingerprint, &backup.destination, &roots, &backup.Size,
		&backup.Checksum, &backup.CreatedAt, &backup.State); err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(roots), &backup.Roots)
	return &backup, nil
}

// List lists a server's backups, newest first.
func (s *Service) List(ctx context.Context, serverID string) ([]Backup, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE server_id=? ORDER BY created_at DESC,id", serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Backup
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *backup)
	}
	return result, rows.Err()
}

// Get returns a complete backup of a server.
func (s *Service) Get(ctx context.Context, serverID, id string) (*Backup, error) {
	backup, err := scanBackup(s.DB.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE server_id=? AND id=?", serverID, id))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && backup.State != "complete") {
		return nil, errNotFound
	}
	return backup, err
}

// Open opens a backup archive for download.
func (s *Service) Open(ctx context.Context, serverID, id string) (io.ReadCloser, error) {
	backup, err := s.Get(ctx, serverID, id)
	if err != nil {
		return nil, err
	}
	return s.Storage.Open(backup.destination, backup.ID)
}

// Delete removes a backup under the server and storage locks. It also
// finishes an interrupted deletion.
func (s *Service) Delete(ctx context.Context, serverID, id string, assertAccess func() error) error {
	backup, err := scanBackup(s.DB.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE server_id=? AND id=? AND state IN ('complete','deleting')",
		serverID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	release, err := s.Locks.Acquire([]string{"server:" + serverID, "backups:storage"}, nil)
	if err != nil {
		return err
	}
	defer release()
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	return s.remove(ctx, backup, assertAccess)
}

// remove marks a backup deleting before unlinking its archive, so an
// interrupted removal leaves a row that is never offered for download or
// restore and is retried by Delete or the next Prune, rather than a complete
// row without its archive. assertAccess runs again right before the unlink;
// if it refuses, the archive is kept and offered again.
func (s *Service) remove(ctx context.Context, backup *Backup, assertAccess func() error) error {
	if _, err := s.DB.ExecContext(ctx, "UPDATE backups SET state='deleting' WHERE id=?", backup.ID); err != nil {
		return err
	}
	var denied error
	var check func() error
	if assertAccess != nil {
		check = func() error {
			denied = assertAccess()
			return denied
		}
	}
	if err := s.Storage.Remove(backup.destination, backup.ID, check); err != nil {
		if denied != nil && backup.State == "complete" {
			if _, restoreErr := s.DB.ExecContext(context.WithoutCancel(ctx), "UPDATE backups SET state='complete' WHERE id=?", backup.ID); restoreErr != nil {
				return restoreErr
			}
		}
		return err
	}
	// A backup that failed before it was recorded may have left a partial file.
	if err := s.Storage.RemovePartial(backup.destination, backup.ID); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, "DELETE FROM backups WHERE id=?", backup.ID)
	return err
}

// SavedSettings returns the saved settings, whether any value (even null)
// was saved, and whether it validates.
func (s *Service) SavedSettings(ctx context.Context) (*Settings, bool, error) {
	var raw json.RawMessage
	found, err := s.DB.Setting(ctx, settingsKey, &raw)
	if err != nil || !found {
		return nil, found, err
	}
	var settings Settings
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if string(raw) == "null" || decoder.Decode(&settings) != nil || settings.Destination == "" || settings.RetentionCount < 1 ||
		settings.RetentionCount > 1000 || settings.MaxBytes <= 0 || settings.ReserveBytes < 0 {
		return nil, true, nil
	}
	return &settings, true, nil
}

// SaveSettings validates and stores settings while admin is still an
// administrator signed in with tokenHash.
func (s *Service) SaveSettings(ctx context.Context, settings Settings, admin *db.SessionUser, tokenHash string) (Settings, error) {
	settings, err := s.Storage.ValidateSettings(ctx, settings)
	if err != nil {
		return settings, err
	}
	// Validation can wait on Docker; the admin's session is checked again
	// in the transaction that saves.
	err = s.DB.AdminMutation(ctx, admin.ID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		return s.DB.SetSetting(ctx, tx, settingsKey, settings)
	})
	return settings, authz.AdminMutationError(err)
}

// Initialize enables the single approved root with default limits, unless
// an administrator has ever saved backup settings.
func (s *Service) Initialize(ctx context.Context) error {
	s.initMu.Lock()
	if s.initRun {
		s.initMu.Unlock()
		return nil
	}
	s.initRun = true
	s.initMu.Unlock()
	defer func() { s.initMu.Lock(); s.initRun = false; s.initMu.Unlock() }()
	// Any saved value, including null, is an administrator decision.
	if _, found, err := s.SavedSettings(ctx); err != nil || found {
		return err
	}
	roots := RootList(s.Storage.Roots)
	if len(roots) != 1 {
		return nil
	}
	settings := Settings{Destination: roots[0], RetentionCount: DefaultRetention, MaxBytes: DefaultMaxBytes, ReserveBytes: DefaultReserve}
	if _, err := s.Storage.ValidateSettings(ctx, settings); err != nil {
		return nil
	}
	observed, err := s.Observer.List(ctx)
	if err != nil {
		return nil
	}
	for _, entry := range observed {
		if s.AssertSeparateDestination(ctx, entry.Observation, settings) != nil {
			return nil
		}
	}
	encoded, _ := json.Marshal(settings)
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// A decision saved while validation awaited I/O wins.
		result, err := tx.ExecContext(ctx, "INSERT INTO settings(key,value_json) VALUES('backups',?) ON CONFLICT(key) DO NOTHING", string(encoded))
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed > 0 {
			return s.DB.WriteAudit(ctx, tx, db.AuditEntry{Action: "settings.backups.auto_enabled", TargetType: "settings"})
		}
		return nil
	})
}

// ForBackup returns validated settings, initializing them if needed: a
// backup due at startup can run before the first background tick.
func (s *Service) ForBackup(ctx context.Context) (Settings, error) {
	settings, _, err := s.SavedSettings(ctx)
	if err != nil {
		return Settings{}, err
	}
	if settings == nil {
		if err := s.Initialize(ctx); err != nil {
			return Settings{}, err
		}
		if settings, _, err = s.SavedSettings(ctx); err != nil {
			return Settings{}, err
		}
	}
	if settings == nil {
		return Settings{}, fail("BACKUPS_NOT_CONFIGURED", "An administrator must configure a backup destination, retention, and capacity before creating backups.")
	}
	return s.Storage.ValidateSettings(ctx, *settings)
}

// ArchiveBytes is the size of complete backups. An unreadable total is an
// error, never zero: zero would let backups grow past the global limit.
func (s *Service) ArchiveBytes(ctx context.Context) (int64, error) {
	var size int64
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM backups WHERE state='complete'").Scan(&size)
	return size, err
}

// Remaining is the archive budget left under the global limit.
func Remaining(settings Settings, used int64) (int64, error) {
	if available := settings.MaxBytes - used; available > 0 {
		return available, nil
	}
	return 0, fail("BACKUP_CAPACITY", "The global backup byte limit is reached. Remove backups or increase the limit.")
}

// Issue is a public readiness problem.
type Issue struct{ Code, Message string }

// readinessIssue keeps Docker and filesystem errors, which can contain host
// paths or credentials, out of the result.
func readinessIssue(err error, fallback Issue) Issue {
	if public, ok := apperr.As(err); ok {
		return Issue{Code: public.Code, Message: public.Message}
	}
	return fallback
}

// StorageStatus is the storage readiness shown to administrators.
type StorageStatus struct {
	Configured     bool
	ArchiveBytes   int64
	MaxBytes       int64
	ReserveBytes   int64
	AvailableBytes int64 // -1 when unknown
	Issues         []Issue
}

func (s *Service) inspect(ctx context.Context) (StorageStatus, *Settings) {
	settings, found, _ := s.SavedSettings(ctx)
	used, err := s.ArchiveBytes(ctx)
	status := StorageStatus{Configured: found, ArchiveBytes: used, AvailableBytes: -1}
	if err != nil {
		status.Issues = append(status.Issues, Issue{"BACKUP_RECORDS_UNAVAILABLE", "Ludock could not read its backup records. Try again."})
		return status, nil
	}
	if settings == nil {
		code := "BACKUP_SETTINGS_INVALID"
		if !found {
			code = "BACKUPS_NOT_CONFIGURED"
		}
		status.Issues = append(status.Issues, Issue{code, "An administrator must configure a valid backup destination, retention, and capacity before creating backups."})
		return status, nil
	}
	status.MaxBytes, status.ReserveBytes = settings.MaxBytes, settings.ReserveBytes
	if _, err := Remaining(*settings, status.ArchiveBytes); err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_CAPACITY", "The backup archive limit could not be checked."}))
	}
	if _, err := s.Storage.ValidateSettings(ctx, *settings); err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_DESTINATION",
			"The backup destination is missing or inaccessible. Ask an administrator to check its mount and LUDOCK_BACKUP_ROOTS."}))
		return status, nil
	}
	available, err := s.Storage.AvailableBytes(settings.Destination)
	if err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_DISK_UNAVAILABLE",
			"Available disk space could not be checked. Ask an administrator to check that the backup destination is accessible and writable."}))
	} else {
		status.AvailableBytes = available
		if available <= settings.ReserveBytes {
			status.Issues = append(status.Issues, Issue{"BACKUP_CAPACITY", "The backup destination does not have free space above its configured reserve."})
		}
	}
	return status, settings
}

// Status reports storage readiness.
func (s *Service) Status(ctx context.Context) StorageStatus {
	status, _ := s.inspect(ctx)
	return status
}

// Preflight is advisory only: no helpers, locks, stops, archive writes, or
// data scans. Execution repeats its checks after acquiring locks.
type Preflight struct {
	Ready     bool
	CheckedAt int64
	Issues    []Issue
}

// Preflight checks whether a backup could run now.
func (s *Service) Preflight(ctx context.Context, server *servers.Context) Preflight {
	status, settings := s.inspect(ctx)
	issues := slices.Clone(status.Issues)
	check := func(err error, fallback Issue) {
		if err != nil {
			issues = append(issues, readinessIssue(err, fallback))
		}
	}
	_, err := PlanRoots(server, true)
	check(err, Issue{"BACKUP_ROOT_UNAVAILABLE", "The selected data roots could not be verified. Ask an administrator to review the server's mounted data roots."})
	if settings != nil {
		check(s.AssertSeparateDestination(ctx, server.Observation, *settings), Issue{"BACKUP_MOUNT_UNVERIFIED",
			"Ludock could not verify that its backup destination is separate from game data. Ask an administrator to check the backup mount."})
	}
	info, err := s.Docker.Inspect(ctx, server.Container.ID)
	if err == nil {
		err = AssertDataOperationState(info.State.Status)
	}
	check(err, Issue{"SERVER_STATE_UNAVAILABLE", "The server's current state could not be checked. Check its Docker connection and try again."})
	check(s.AssertNoOtherWriters(ctx, server, ""), Issue{"BACKUP_WRITERS_UNVERIFIED",
		"Ludock could not check for other containers writing to this server's data. Check its Docker connection and try again."})
	return Preflight{Ready: len(issues) == 0, CheckedAt: time.Now().UnixMilli(), Issues: issues}
}

// AssertReady runs the checks that need no stop, so a backup that cannot
// succeed never takes the server offline.
func (s *Service) AssertReady(ctx context.Context, server *servers.Context) error {
	settings, err := s.ForBackup(ctx)
	if err != nil {
		return err
	}
	if err := s.AssertSeparateDestination(ctx, server.Observation, settings); err != nil {
		return err
	}
	if err := s.Storage.AssertSpace(settings.Destination, settings.ReserveBytes, 0); err != nil {
		return err
	}
	return s.AssertNoOtherWriters(ctx, server, "")
}

func overlaps(left, right string) bool {
	return left == "/" || right == "/" || left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

// MountsOverlap reports two mounts that can reach the same data.
func MountsOverlap(left, right identity.Mount) bool {
	if left.Type == "volume" && right.Type == "volume" && left.Name != "" && right.Name != "" {
		return left.Name == right.Name
	}
	return left.Source != "" && right.Source != "" && overlaps(path.Clean("/"+left.Source), path.Clean("/"+right.Source))
}

// AssertNoOtherWriters refuses when another running container can write to
// the server's data.
func (s *Service) AssertNoOtherWriters(ctx context.Context, server *servers.Context, helperID string) error {
	containers, err := s.Docker.ListContainers(ctx, true, nil)
	if err != nil {
		return err
	}
	var sources []identity.Mount
	for _, mount := range server.Observation.Mounts {
		if mount.Writable {
			sources = append(sources, mount)
		}
	}
	for _, candidate := range containers {
		state := ""
		if candidate.State != nil {
			state = *candidate.State
		}
		if candidate.ID == server.Container.ID || candidate.ID == helperID || !slices.Contains([]string{"running", "restarting", "paused"}, state) {
			continue
		}
		info, err := s.Docker.Inspect(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if !info.State.Running && !info.State.Restarting && !info.State.Paused {
			continue
		}
		for _, mount := range info.Mounts {
			if !mount.RW {
				continue
			}
			other := identity.Mount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, Writable: true, Name: mount.Name}
			for _, source := range sources {
				if MountsOverlap(source, other) {
					return fail("SHARED_DATA_WRITER", "Another running container can write to this server's data. Stop shared writers before backing up or restoring.")
				}
			}
		}
	}
	return nil
}

// AssertSeparateDestination proves the destination is mounted into Ludock
// and does not overlap the server's data.
func (s *Service) AssertSeparateDestination(ctx context.Context, observation identity.Observation, settings Settings) error {
	destination, err := s.Storage.ApprovedDirectory(settings.Destination)
	if err != nil {
		return err
	}
	targets := []identity.Mount{{Type: "bind", Source: destination, Destination: destination, Writable: true}}
	proven := false
	if docker.ContainerIDPattern.MatchString(s.Storage.Self) {
		self, err := s.Docker.Inspect(ctx, s.Storage.Self)
		if err != nil && !docker.IsNotFound(err) {
			return err
		}
		if err == nil {
			for _, mount := range self.Mounts {
				if destination == mount.Destination || strings.HasPrefix(destination, mount.Destination+"/") {
					proven = true
					targets = append(targets, identity.Mount{Type: mount.Type, Name: mount.Name,
						Source: mount.Source + destination[len(mount.Destination):], Destination: destination, Writable: true})
				}
			}
		}
	}
	if !proven {
		return fail("BACKUP_MOUNT_UNVERIFIED", "The backup destination must be mounted into Ludock. Set LUDOCK_SELF_CONTAINER if its Docker hostname does not identify its container.")
	}
	for _, source := range observation.Mounts {
		if !source.Writable {
			continue
		}
		for _, target := range targets {
			if MountsOverlap(source, target) {
				return fail("BACKUP_SOURCE_OVERLAP", "The backup destination overlaps game data. Mount a separate backup destination.")
			}
		}
	}
	return nil
}

// AssertDataOperationState refuses servers in transitional states.
func AssertDataOperationState(state string) error {
	if slices.Contains([]string{"paused", "restarting", "removing", "dead"}, state) {
		return fail("SERVER_STATE", "Unpause or repair this server before a data operation.")
	}
	return nil
}

// Record saves a complete backup.
func (s *Service) Record(ctx context.Context, backup Backup, fingerprint, destination string) error {
	roots, _ := json.Marshal(backup.Roots)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
    VALUES(?,?,?,?,?,?,?,?,'complete')`, backup.ID, backup.ServerID, fingerprint, destination, string(roots), backup.Size, backup.Checksum, backup.CreatedAt)
	return err
}

// RecordDeleting saves an unrecorded archive whose removal failed as a
// deleting backup. It is never offered, and Prune and Delete retry removing
// it, so it cannot use storage unseen by retention and capacity checks.
func (s *Service) RecordDeleting(ctx context.Context, id, serverID, fingerprint, destination string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state)
    VALUES(?,?,?,?,'[]',0,'',?,'deleting')`, id, serverID, fingerprint, destination, time.Now().UnixMilli())
	return err
}

// Exists reports whether a backup row exists (any state when complete is
// false). A failed lookup is an error, not an absent row.
func (s *Service) Exists(ctx context.Context, id string, complete bool) (bool, error) {
	query := "SELECT id FROM backups WHERE id=?"
	if complete {
		query += " AND state='complete'"
	}
	var found string
	err := s.DB.QueryRowContext(ctx, query, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Prune removes complete backups beyond the retention count, keeping keep,
// and finishes interrupted deletions.
func (s *Service) Prune(ctx context.Context, serverID string, retention int, keep string) error {
	list, err := s.List(ctx, serverID)
	if err != nil {
		return err
	}
	var complete, removed []Backup
	for _, backup := range list {
		switch backup.State {
		case "complete":
			complete = append(complete, backup)
		case "deleting":
			removed = append(removed, backup)
		}
	}
	if len(complete) > retention {
		for _, old := range complete[retention:] {
			if old.ID != keep {
				removed = append(removed, old)
			}
		}
	}
	for _, old := range removed {
		if err := s.remove(ctx, &old, nil); err != nil {
			return err
		}
	}
	return nil
}
