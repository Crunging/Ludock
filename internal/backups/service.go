package backups

import (
	"cmp"
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
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/textutil"
)

type Backup struct {
	ID       string
	ServerID string
	Roots    []Root
	// GameType is the game the server ran when the backup was taken.
	GameType  string
	Size      int64
	Checksum  string
	CreatedAt int64
	State     string
	// KeptAt is when an administrator kept the backup, 0 if not kept;
	// retention never removes a kept backup.
	KeptAt int64
	Note   string
	// Reason is the work that took the backup for its own protection:
	// "restore" or "update", or empty.
	Reason string
	// CopyState is the second copy's state: "" for none, "pending",
	// "complete", or "failed" (still retried); CopyError says why the last
	// attempt failed.
	CopyState, CopyError string
	// FolderAway is set by ArchiveFolder and Locate when the folder the
	// backup was saved in can't be opened or isn't responding.
	FolderAway bool

	// FromCopy is set by ArchiveFolder when the archive is read from the
	// second copy, and Copying by Locate while the copy is being made.
	FromCopy, Copying bool
	fingerprint       string
	destination       string
	copyDestination   string
	copyAttempts      int
	// copyPrevious holds a verified copy made before the second folder
	// changed, until the new folder has one.
	copyPrevious string
}

type Service struct {
	DB       *db.DB
	Storage  *Storage
	Helpers  *Helpers
	Docker   *docker.Client
	Observer *servers.Observer
	Locks    *locks.Table
	// CopyFailed queues the alert for a second copy that still fails after
	// several attempts, inside the transaction that records it; problem is
	// public text. Alerted runs after that transaction commits.
	CopyFailed func(ctx context.Context, tx *sql.Tx, backupID, serverID, problem string) error
	// DataFailed queues the alert for saving Ludock's own data, or its
	// second copy, failing again and again; message is public text.
	DataFailed func(ctx context.Context, tx *sql.Tx, key, message string) error
	Alerted    func()

	copies copies

	// initMu serializes Initialize: a backup that finds no settings waits
	// for setup already under way instead of failing as unconfigured. It
	// also guards copyRetryAt.
	initMu      sync.Mutex
	copyRetryAt time.Time

	// deletionCursor is where the next FinishDeletions batch starts, so rows
	// that keep failing cannot hold back the ones after them.
	deletionMu     sync.Mutex
	deletionCursor struct {
		created int64
		id      string
	}
}

// deletionBatch is how many deleting rows one FinishDeletions call retries.
var deletionBatch = 100

const settingsKey = "backups"

var (
	errNotFound = fail("BACKUP_NOT_FOUND", "Backup not found.")
	errKept     = fail("BACKUP_KEPT", "This backup is kept. Release it before deleting it.")
	// errSettingsChanged is a save made from a page older than the settings.
	errSettingsChanged = fail("BACKUP_SETTINGS_CHANGED", "Backup settings changed in another window. Reload the page and save again.")
)

const backupColumns = "id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state,COALESCE(kept_at,0),note,reason," +
	"copy_destination,copy_state,copy_error,copy_attempts,copy_previous,game_type"

func scanBackup(s interface{ Scan(...any) error }) (*Backup, error) {
	var backup Backup
	var roots string
	if err := s.Scan(&backup.ID, &backup.ServerID, &backup.fingerprint, &backup.destination, &roots, &backup.Size,
		&backup.Checksum, &backup.CreatedAt, &backup.State, &backup.KeptAt, &backup.Note, &backup.Reason,
		&backup.copyDestination, &backup.CopyState, &backup.CopyError, &backup.copyAttempts,
		&backup.copyPrevious, &backup.GameType); err != nil {
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

func (s *Service) Open(ctx context.Context, serverID, id string) (io.ReadCloser, error) {
	backup, err := s.Get(ctx, serverID, id)
	if err != nil {
		return nil, err
	}
	folder, err := s.ArchiveFolder(backup)
	if err != nil {
		return nil, err
	}
	return s.Storage.Open(folder, backup.ID)
}

// errDeleteAway refuses a deletion that would leave its archive behind
// without the person saying so.
var errDeleteAway = fail("BACKUP_FOLDER_AWAY", "The folder this backup was saved in can’t be reached. Reload the page to remove the backup from Ludock anyway.")

// Delete removes a backup under the server and storage locks. It also
// finishes an interrupted deletion. With forget, a backup whose folder can't
// be reached is removed from Ludock with its second copies, leaving its
// archive wherever it is; left reports that.
func (s *Service) Delete(ctx context.Context, serverID, id string, forget bool, assertAccess func() error) (left bool, err error) {
	backup, err := scanBackup(s.DB.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE server_id=? AND id=? AND state IN ('complete','deleting')",
		serverID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, errNotFound
	}
	if err != nil {
		return false, err
	}
	release, err := s.Locks.AcquirePreempting(ctx, []string{locks.ServerKey(serverID), locks.StorageKey}, nil)
	if err != nil {
		return false, err
	}
	defer release()
	// Retention may have removed it while this waited for the locks.
	if exists, err := s.Exists(ctx, id, false); err != nil || !exists {
		return false, cmp.Or(err, error(errNotFound))
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return false, err
		}
	}
	err = s.remove(ctx, backup, assertAccess, false)
	if errors.Is(err, errFolderAway) || errors.Is(err, errFolderStalled) {
		if !forget {
			if errors.Is(err, errFolderAway) {
				err = errDeleteAway
			}
			return false, err
		}
		left, err = true, s.remove(ctx, backup, assertAccess, true)
	}
	// The archive is gone; its copy goes once the second folder is back.
	if errors.Is(err, errCopyPending) {
		err = nil
	}
	return left && err == nil, err
}

// remove marks a backup deleting before unlinking its archive, so an
// interrupted removal leaves a row that is never offered and is retried by
// Delete or Prune. If assertAccess refuses right before marking, the backup is
// left as it was. A folder that can't be reached leaves it as it was too,
// unless forget, which removes the row without touching the folder.
func (s *Service) remove(ctx context.Context, backup *Backup, assertAccess func() error, forget bool) error {
	if !forget {
		if err := s.reach(backup.destination, errFolderStalled, measureWait, func(func()) error {
			return s.Storage.reachable(backup.destination)
		}); err != nil {
			return err
		}
	}
	marked := false
	mark := func() error {
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		// Kept between listing and marking, the backup stays.
		result, err := s.DB.ExecContext(ctx, "UPDATE backups SET state='deleting' WHERE id=? AND (state='deleting' OR kept_at IS NULL)", backup.ID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed == 0 {
			return errKept
		}
		marked = true
		return nil
	}
	check := mark
	if assertAccess == nil || forget {
		if err := mark(); err != nil {
			return err
		}
		check = nil
	}
	if !forget {
		// The folder can stop responding after the look above. A removal
		// given up on must not mark the backup once this has returned and
		// its locks are released; one already marked is an interrupted
		// removal, finished by Delete or Prune.
		var gate sync.Mutex
		givenUp := false
		guarded := check
		if check != nil {
			guarded = func() error {
				gate.Lock()
				defer gate.Unlock()
				if givenUp {
					return errFolderStalled
				}
				return check()
			}
		}
		if err := s.reach(backup.destination, errFolderStalled, copyStall, func(func()) error {
			return s.Storage.Remove(backup.destination, backup.ID, guarded)
		}); err != nil {
			gate.Lock()
			givenUp = true
			gate.Unlock()
			return err
		}
		// Remove checks only when the archive exists.
		if !marked {
			if err := mark(); err != nil {
				return err
			}
		}
		// A backup that failed before it was recorded may have left a partial file.
		if err := s.reach(backup.destination, errFolderStalled, copyStall, func(func()) error {
			return s.Storage.RemovePartial(backup.destination, backup.ID)
		}); err != nil {
			return err
		}
	}
	if handed, err := s.markCopyRemoval(ctx, backup.ID); err != nil || handed {
		return cmp.Or(err, errCopyPending)
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
	if string(raw) == "null" || decoder.Decode(&settings) != nil || !settings.valid() {
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
	// A second folder that is away doesn't block other changes; the copier
	// checks it before every copy.
	saved, _, err := s.SavedSettings(ctx)
	unchanged := err == nil && saved != nil && saved.CopyTo == settings.CopyTo && saved.Destination == settings.Destination
	if !unchanged {
		if err := s.ValidateCopyFolder(ctx, settings); err != nil {
			return settings, err
		}
	}
	// Validation can wait on Docker; the admin's session is checked again
	// in the transaction that saves.
	err = s.DB.AdminMutation(ctx, admin.ID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		// Skipped validation holds only if the folders weren't changed since.
		if unchanged {
			var current Settings
			if found, err := db.ReadSetting(ctx, tx, settingsKey, &current); err != nil || !found ||
				current.CopyTo != settings.CopyTo || current.Destination != settings.Destination {
				return errSettingsChanged
			}
		}
		if err := s.DB.SetSetting(ctx, tx, settingsKey, settings); err != nil {
			return err
		}
		return retarget(ctx, tx, settings.CopyTo)
	})
	if err == nil && settings.CopyTo != "" {
		s.WakeCopies()
	}
	return settings, authz.AdminMutationError(err)
}

// Initialize enables the first approved root with the default reserve, unless
// an administrator has ever saved backup settings, then starts second copies
// to a found copy folder.
func (s *Service) Initialize(ctx context.Context) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	saved, found, err := s.SavedSettings(ctx)
	if err != nil {
		return err
	}
	// Any saved value, including null, is an administrator decision.
	if !found {
		if err := s.enable(ctx); err != nil {
			return err
		}
		if saved, _, err = s.SavedSettings(ctx); err != nil {
			return err
		}
	}
	if saved == nil {
		return nil
	}
	return s.startCopies(ctx, *saved)
}

func (s *Service) enable(ctx context.Context) error {
	if testHook != nil {
		testHook("initializing")
	}
	roots := RootList(s.Storage.Roots)
	if len(roots) == 0 {
		return nil
	}
	settings := Settings{Destination: roots[0], ReserveBytes: DefaultReserve}
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

// foundCopyRetry spaces out checks of a found copy folder that failed one,
// and foundCopyWait bounds one check, which also asks Docker.
const (
	foundCopyRetry = time.Minute
	foundCopyWait  = 30 * time.Second
)

// startCopies copies every backup to the found copy folder, unless an
// administrator chose a second folder or none. Mounting it says the owner
// wants the copy.
func (s *Service) startCopies(ctx context.Context, saved Settings) error {
	found := s.Storage.FoundCopy()
	if saved.CopyChosen || saved.CopyTo != "" || found == "" || found == saved.Destination || time.Now().Before(s.copyRetryAt) {
		return nil
	}
	settings := saved
	settings.CopyTo = found
	// A share that stops responding mustn't hold up the storage work after
	// this, and each folder has one check waiting on it at most.
	if !s.copies.measure(found) {
		return nil
	}
	checked := make(chan error, 1)
	go func() {
		defer s.copies.measured(found)
		checked <- s.ValidateCopyFolder(ctx, settings)
	}()
	var err error
	select {
	case err = <-checked:
	case <-time.After(foundCopyWait):
		err = errCopyStalled
	}
	if err != nil {
		s.copyRetryAt = time.Now().Add(foundCopyRetry)
		return nil
	}
	started := false
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		// A choice saved while the folder was checked wins.
		raw, found, err := db.RawSetting(ctx, tx, settingsKey)
		if err == nil && !found {
			err = sql.ErrNoRows
		}
		if err != nil {
			return err
		}
		var current Settings
		if json.Unmarshal([]byte(raw), &current) != nil || current != saved {
			return nil
		}
		if err := s.DB.SetSetting(ctx, tx, settingsKey, settings); err != nil {
			return err
		}
		if err := retarget(ctx, tx, settings.CopyTo); err != nil {
			return err
		}
		started = true
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{Action: "settings.backups.copy_auto_enabled", TargetType: "settings"})
	})
	if err == nil && started {
		s.WakeCopies()
	}
	return err
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
		return Settings{}, fail("BACKUPS_NOT_CONFIGURED", "Backups are off until a backup folder is chosen in Settings.")
	}
	return s.Storage.ValidateSettings(ctx, *settings)
}

// ArchiveBytes is the size of complete backups, and of the kept ones among
// them. An unreadable total is an error, never zero: zero would let backups
// grow past the total limit.
func (s *Service) ArchiveBytes(ctx context.Context) (total, kept int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0), COALESCE(SUM(CASE WHEN kept_at IS NULL THEN 0 ELSE size END),0)
    FROM backups WHERE state='complete'`).Scan(&total, &kept)
	return total, kept, err
}

// unlimited stands in for "no total limit"; it fits JSON's exact integers.
const unlimited = maxSetting

// Remaining is the archive budget left under the total limit, if one is set.
func Remaining(settings Settings, used, kept int64) (int64, error) {
	if settings.MaxBytes == 0 {
		return unlimited, nil
	}
	if available := settings.MaxBytes - used; available > 0 {
		return available, nil
	}
	return 0, CapacityError(codeLimit, kept)
}

// The problems behind a backup that can't fit, as error codes.
const (
	codeDisk  = "BACKUP_CAPACITY"
	codeLimit = "BACKUP_LIMIT"
)

// CapacityError reports a backup that can't fit, saying how much kept
// backups hold when they are part of the reason.
func CapacityError(code string, kept int64) error {
	problem := "The backup disk is down to its free-space reserve."
	if code == codeLimit {
		problem = "Backups have reached their total size limit."
	}
	advice := " Delete backups you no longer need, or free space on the backup disk."
	if code == codeLimit {
		advice = " Delete backups you no longer need, or raise the limit in Settings."
	}
	if kept > 0 {
		advice += " Kept backups use " + datasize.Format(kept) + "; release ones you no longer need so they can be deleted."
	}
	return fail(code, problem+advice)
}

// ExplainCapacity rewords a lack of room to say how much kept backups hold.
func ExplainCapacity(err error, kept int64) error {
	for _, code := range []string{codeDisk, codeLimit} {
		if apperr.Is(err, code) {
			return CapacityError(code, kept)
		}
	}
	return err
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
	Destination    string
	ArchiveBytes   int64
	KeptBytes      int64
	MaxBytes       int64 // 0 when there is no total limit
	ReserveBytes   int64
	AvailableBytes int64 // -1 when unknown
	Issues         []Issue
	// CopyTo is the second copy's folder, if any. Its problem never stops
	// backups, so it isn't among Issues.
	CopyTo string
	// CopyFound: Ludock chose CopyTo because it found the folder mounted.
	CopyFound          bool
	CopyAvailableBytes int64 // -1 when unknown
	CopyIssue          string
	// DataSavedAt is when Ludock last saved its own data, 0 if not yet, and
	// DataProblem why that or its second copy is failing.
	DataSavedAt int64
	DataProblem string
}

func (s *Service) inspect(ctx context.Context) (StorageStatus, *Settings) {
	settings, found, _ := s.SavedSettings(ctx)
	used, kept, err := s.ArchiveBytes(ctx)
	status := StorageStatus{Configured: found, ArchiveBytes: used, KeptBytes: kept, AvailableBytes: -1, CopyAvailableBytes: -1}
	if err != nil {
		status.Issues = append(status.Issues, Issue{"BACKUP_RECORDS_UNAVAILABLE", "Ludock could not read its backup records. Try again."})
		return status, nil
	}
	if settings == nil {
		code := "BACKUP_SETTINGS_INVALID"
		if !found {
			code = "BACKUPS_NOT_CONFIGURED"
		}
		status.Issues = append(status.Issues, Issue{code, "No usable backup folder is chosen in Settings."})
		return status, nil
	}
	status.Destination, status.MaxBytes, status.ReserveBytes = settings.Destination, settings.MaxBytes, settings.ReserveBytes
	status.DataSavedAt, status.DataProblem = s.dataStatus(ctx, *settings)
	status.CopyFound = settings.CopyTo != "" && !settings.CopyChosen
	if status.CopyTo = settings.CopyTo; status.CopyTo != "" {
		// A share that stops responding mustn't hold up the page, and each
		// folder has one reading waiting on it at most.
		if !s.copies.measure(settings.CopyTo) {
			status.CopyIssue = errCopyStalled.Message
		} else {
			measured := make(chan int64, 1)
			go func() {
				defer s.copies.measured(settings.CopyTo)
				if testHook != nil {
					testHook("measuring")
				}
				available, err := s.Storage.AvailableBytes(settings.CopyTo)
				if err != nil {
					available = -1
				}
				measured <- available
			}()
			select {
			case available := <-measured:
				if status.CopyAvailableBytes = available; available < 0 {
					status.CopyIssue = errCopyMissing.Message
				}
			case <-time.After(measureWait):
				status.CopyIssue = errCopyStalled.Message
			}
		}
	}
	if _, err := Remaining(*settings, status.ArchiveBytes, kept); err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_CAPACITY", "The backup size limit could not be checked."}))
	}
	if _, err := s.Storage.ValidateSettings(ctx, *settings); err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_DESTINATION",
			"The backup folder is missing or can’t be reached. Check its mount and LUDOCK_BACKUP_ROOTS."}))
		return status, nil
	}
	available, err := s.Storage.AvailableBytes(settings.Destination)
	if err != nil {
		status.Issues = append(status.Issues, readinessIssue(err, Issue{"BACKUP_DISK_UNAVAILABLE",
			"Free space on the backup disk couldn’t be checked. Check that the backup folder can be reached and written."}))
	} else {
		status.AvailableBytes = available
		if available <= settings.ReserveBytes {
			status.Issues = append(status.Issues, readinessIssue(CapacityError(codeDisk, kept), Issue{}))
		}
	}
	return status, settings
}

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

func (s *Service) Preflight(ctx context.Context, server *servers.Context) Preflight {
	status, settings := s.inspect(ctx)
	issues := slices.Clone(status.Issues)
	check := func(err error, fallback Issue) {
		if err != nil {
			issues = append(issues, readinessIssue(err, fallback))
		}
	}
	_, err := PlanRoots(server, true)
	check(err, Issue{"BACKUP_ROOT_UNAVAILABLE", "The server’s data folders couldn’t be checked. Review the folders mounted into it."})
	if settings != nil {
		check(s.AssertSeparateDestination(ctx, server.Observation, *settings), Issue{"BACKUP_MOUNT_UNVERIFIED",
			"Ludock couldn’t confirm that the backup folder is separate from game data. Check the backup mount."})
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
// the server's data. Docker's container list carries each container's state
// and mounts, so one request checks them all; it runs every second while a
// backup copies.
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
		for _, mount := range candidate.Mounts {
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
	mounts, err := s.hostMounts(ctx, destination)
	if err != nil {
		return err
	}
	targets := append([]identity.Mount{{Type: "bind", Source: destination, Destination: destination, Writable: true}}, mounts...)
	if len(mounts) == 0 {
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

// hostMounts is how a resolved folder reaches the Docker host through
// Ludock's own mounts; none when that can't be shown.
func (s *Service) hostMounts(ctx context.Context, folder string) ([]identity.Mount, error) {
	if !docker.ContainerIDPattern.MatchString(s.Storage.Self) {
		return nil, nil
	}
	self, err := s.Docker.Inspect(ctx, s.Storage.Self)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mounts []identity.Mount
	for _, mount := range self.Mounts {
		if within(folder, mount.Destination) {
			mounts = append(mounts, identity.Mount{Type: mount.Type, Name: mount.Name,
				Source: mount.Source + folder[len(mount.Destination):], Destination: folder, Writable: true})
		}
	}
	return mounts, nil
}

// AssertDataOperationState refuses servers in transitional states.
func AssertDataOperationState(state string) error {
	if slices.Contains([]string{"paused", "restarting", "removing", "dead"}, state) {
		return fail("SERVER_STATE", "Unpause or repair this server first.")
	}
	return nil
}

// Record saves a complete backup written to destination. Its second copy
// follows the setting saved now, not when the backup started, since saving
// settings retargets only recorded backups.
func (s *Service) Record(ctx context.Context, backup Backup, fingerprint, destination string) error {
	roots, _ := json.Marshal(backup.Roots)
	_, err := s.DB.ExecContext(ctx, `WITH setting(folder) AS (
      SELECT COALESCE((SELECT json_extract(value_json,'$.copyTo') FROM settings WHERE key='backups'),''))
    INSERT INTO backups(id,server_id,binding_fingerprint,destination,roots_json,size,checksum,created_at,state,reason,copy_destination,copy_state,game_type)
    SELECT ?,?,?,?,?,?,?,?,'complete',?, CASE WHEN folder=? THEN '' ELSE folder END, CASE WHEN folder IN ('',?) THEN '' ELSE 'pending' END, ?
    FROM setting`, backup.ID, backup.ServerID, fingerprint, destination, string(roots), backup.Size, backup.Checksum,
		backup.CreatedAt, backup.Reason, destination, destination, backup.GameType)
	return err
}

// FinishDeletions retries every interrupted or failed removal under its server
// and storage locks; a busy one waits for the next call.
func (s *Service) FinishDeletions(ctx context.Context) error {
	s.deletionMu.Lock()
	defer s.deletionMu.Unlock()
	cursor := s.deletionCursor
	rows, err := s.DB.QueryContext(ctx, "SELECT "+backupColumns+` FROM backups WHERE state='deleting' AND copy_state<>'removing' AND (created_at,id) > (?,?)
    ORDER BY created_at,id LIMIT ?`, cursor.created, cursor.id, deletionBatch)
	if err != nil {
		return err
	}
	var pending []*Backup
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, backup)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// The next call continues after this batch, and a short batch means
	// the end was reached, so it starts over.
	if len(pending) < deletionBatch {
		s.deletionCursor.created, s.deletionCursor.id = 0, ""
	} else {
		last := pending[len(pending)-1]
		s.deletionCursor.created, s.deletionCursor.id = last.CreatedAt, last.ID
	}
	if testHook != nil {
		testHook("pending")
	}
	var failed error
	for _, backup := range pending {
		release, err := s.Locks.Acquire([]string{locks.ServerKey(backup.ServerID), locks.StorageKey}, nil)
		if err != nil {
			continue
		}
		// A folder that is away keeps its deletions until it is back.
		if err := s.finishDeletion(ctx, backup.ID); err != nil && !errors.Is(err, errCopyPending) && !errors.Is(err, errFolderAway) && failed == nil {
			failed = err
		}
		release()
	}
	return failed
}

// finishDeletion removes a backup only if it is still deleting under the
// held locks: a Delete refused before its unlink restores the row to
// complete after it was listed, and that backup must be kept.
func (s *Service) finishDeletion(ctx context.Context, id string) error {
	backup, err := scanBackup(s.DB.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE id=? AND state='deleting'", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.remove(ctx, backup, nil, false)
}

// testHook, set only by tests, runs at named points so a test can force an
// interleaving.
var testHook func(point string)

// RecordDeleting records an archive whose removal failed as a deleting backup,
// so FinishDeletions, Prune, and Delete retry it and it never uses storage
// unseen.
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

// Prune applies the retention rule to a server's backups and finishes its
// interrupted deletions. protected backups (the one just taken, a restore's
// source) stay, and so does any archive open for reading. The caller holds
// the server and storage locks.
func (s *Service) Prune(ctx context.Context, serverID string, settings Settings, protected ...string) error {
	list, err := s.List(ctx, serverID)
	if err != nil {
		return err
	}
	// Archives outside the current folder, and second copies, may be on a
	// disk that is gone; failing to remove them never fails a new backup.
	failed := func(backup Backup, err error) bool {
		return err != nil && !errors.Is(err, errCopyPending) && backup.destination == settings.Destination
	}
	for _, backup := range list {
		if backup.State == "deleting" && backup.CopyState != "removing" {
			if err := s.remove(ctx, &backup, nil, false); failed(backup, err) {
				return err
			}
		}
	}
	expired, spare := planRetention(list, time.Local, func(id string) bool {
		return slices.Contains(protected, id) || s.Storage.Reading(id)
	})
	for _, backup := range expired {
		if err := s.removeIdle(ctx, backup); failed(backup, err) {
			return err
		}
	}
	// Space is measured again after each removal, and only archives in the
	// current folder free room on its disk.
	for _, backup := range spare {
		if room, need := s.room(ctx, list, settings); room >= need {
			break
		}
		if backup.destination != settings.Destination {
			continue
		}
		if err := s.removeIdle(ctx, backup); failed(backup, err) {
			return err
		}
	}
	return nil
}

// removeIdle removes a backup for retention unless it is being read or was
// kept since it was listed. One whose archive isn't in its folder stays: an
// unmounted disk can leave its folder empty, and the second copy may then be
// the only one.
func (s *Service) removeIdle(ctx context.Context, backup Backup) error {
	release, idle := s.Storage.claimIdle(backup.ID)
	if !idle {
		return nil
	}
	defer release()
	if found, err := s.look(backup.destination, backup.ID, errFolderStalled); err != nil || !found {
		return err
	}
	if err := s.remove(ctx, &backup, nil, false); !errors.Is(err, errKept) {
		return err
	}
	return nil
}

// room is the space left for the server's next backup and the space it
// should need: its newest backup's size plus a margin. When either is
// unknown, need is zero, so space alone removes nothing.
func (s *Service) room(ctx context.Context, list []Backup, settings Settings) (room, need int64) {
	index := slices.IndexFunc(list, func(backup Backup) bool { return backup.State == "complete" })
	if index < 0 {
		return 0, 0
	}
	available, err := s.Storage.AvailableBytes(settings.Destination)
	if err != nil {
		return 0, 0
	}
	room = available - settings.ReserveBytes
	if settings.MaxBytes > 0 {
		used, _, err := s.ArchiveBytes(ctx)
		if err != nil {
			return 0, 0
		}
		room = min(room, settings.MaxBytes-used)
	}
	size := list[index].Size
	return room, size + size*spaceMargin/100
}

// Keep marks a complete backup kept, with an optional note, while admin is
// still an administrator signed in with tokenHash.
func (s *Service) Keep(ctx context.Context, serverID, id, note string, admin *db.SessionUser, tokenHash string) error {
	note = strings.Join(strings.Fields(note), " ")
	if textutil.Length(note) > noteLimit {
		return apperr.New("INVALID_REQUEST", 400, "Keep the note to 100 characters or fewer.")
	}
	return s.setKept(ctx, serverID, id, admin, tokenHash, "kept_at=?, note=?", db.Now(), note)
}

// Release lets retention remove a kept backup again.
func (s *Service) Release(ctx context.Context, serverID, id string, admin *db.SessionUser, tokenHash string) error {
	return s.setKept(ctx, serverID, id, admin, tokenHash, "kept_at=NULL, note=''")
}

func (s *Service) setKept(ctx context.Context, serverID, id string, admin *db.SessionUser, tokenHash, assignments string, values ...any) error {
	err := s.DB.AdminMutation(ctx, admin.ID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE backups SET "+assignments+" WHERE server_id=? AND id=? AND state='complete'",
			append(values, serverID, id)...)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed == 0 {
			return errNotFound
		}
		return nil
	})
	return authz.AdminMutationError(err)
}
