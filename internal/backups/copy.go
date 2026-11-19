package backups

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
)

// Second copies: a complete backup recorded while Settings.CopyTo is set
// gets copy_state 'pending'. The copier makes it after the backup's work has
// ended and the server is back as it was, then marks it 'complete'. A
// failing copy is retried with backoff; after copyAttemptsBeforeAlert
// attempts it is 'failed', reported once, and still retried. Each attempt is
// counted before it starts, so an attempt interrupted by a restart waits its
// backoff like a failure. A copy counts only once its row says 'complete':
// it is written as <name>.partial, read back and checked, then renamed.

const copyAttemptsBeforeAlert = 3

// copyRetry is the wait after each failed attempt; the last repeats.
var copyRetry = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 6 * time.Hour}

// copyStall ends a copy that makes no progress for this long.
var copyStall = 2 * time.Minute

var (
	errCopyMissing  = fail("BACKUP_COPY_FOLDER", "The second folder is missing or not mounted.")
	errCopyReadOnly = fail("BACKUP_COPY_FOLDER", "Ludock can’t write to the second folder.")
	errCopySpace    = fail("BACKUP_COPY_SPACE", "The second folder is down to its free-space reserve.")
	errCopyStalled  = fail("BACKUP_COPY_STALLED", "The second folder stopped responding.")
	errCopyMismatch = fail("BACKUP_COPY_MISMATCH", "The copy didn’t match the original.")
	errCopyOverlap  = fail("BACKUP_COPY_OVERLAP", "The second folder must be separate from the backup folder.")
	errCopySource   = fail("BACKUP_COPY_SOURCE", "The original archive is missing or changed.")
	errCopyFailed   = fail("BACKUP_COPY_FAILED", "The copy couldn’t be written.")
	// errCopyPending is a removed backup whose second copy the copier
	// removes; its row stays deleting until then.
	errCopyPending = errors.New("second copy not removed yet")
)

// fileName is a plain file name inside a backup folder.
var fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// CopyFile copies source, which must be size bytes with the SHA-256
// checksum, to name in directory. The source is checked as it is read, the
// copy is written as name.partial, synced, read back from disk and checked,
// and only then renamed to name. It leaves the reserve free and stops when
// ctx ends; progress is called after each write.
func (s *Storage) CopyFile(ctx context.Context, source io.Reader, directory, name string, size int64, checksum string,
	reserve int64, progress func()) error {
	if !fileName.MatchString(name) || strings.HasSuffix(name, ".partial") {
		return fail("INVALID_REQUEST", "Invalid file name.")
	}
	dir, err := s.pin(directory)
	if err != nil {
		return errCopyMissing
	}
	defer dir.close()
	if unix.Faccessat(dir.fd, ".", unix.W_OK|unix.X_OK, 0) != nil {
		return errCopyReadOnly
	}
	// A partial file left by an interrupted attempt doesn't count against
	// the space this one needs.
	partial := name + ".partial"
	if err := unix.Unlinkat(dir.fd, partial, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return errCopyFailed
	}
	if _, err := budget(dir, reserve, size); err != nil {
		return errCopySpace
	}
	fd, err := unix.Openat(dir.fd, partial, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.EROFS) || errors.Is(err, unix.EACCES) {
			return errCopyReadOnly
		}
		return errCopyFailed
	}
	file := os.NewFile(uintptr(fd), partial)
	renamed := false
	defer func() {
		file.Close()
		if !renamed {
			unix.Unlinkat(dir.fd, partial, 0)
		}
	}()
	written, sum, err := copyChecked(ctx, file, source, func(written int64) error {
		if _, err := budget(dir, reserve, 0); err != nil {
			return errCopySpace
		}
		if progress != nil {
			progress()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if written != size || sum != checksum {
		return errCopySource
	}
	if file.Sync() != nil {
		return errCopyFailed
	}
	// Dropping the cached pages makes the check below read the disk.
	unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)
	if err := verifyFile(ctx, dir, partial, size, checksum); err != nil {
		return err
	}
	if unix.Renameat(dir.fd, partial, dir.fd, name) != nil {
		return errCopyFailed
	}
	renamed = true
	if unix.Fsync(dir.fd) != nil {
		return errCopyFailed
	}
	return nil
}

// HasCopy reports whether name in directory is already a complete copy:
// one renamed into place by an attempt interrupted before it was recorded.
func (s *Storage) HasCopy(ctx context.Context, directory, name string, size int64, checksum string) bool {
	if !fileName.MatchString(name) {
		return false
	}
	dir, err := s.pin(directory)
	if err != nil {
		return false
	}
	defer dir.close()
	// The rename that published it may not have reached the disk yet.
	return verifyFile(ctx, dir, name, size, checksum) == nil && unix.Fsync(dir.fd) == nil
}

// sameDirectory reports whether two folders are one directory, as when two
// host paths are aliases of it.
func (s *Storage) sameDirectory(left, right string) bool {
	identity := func(folder string) (unix.Stat_t, bool) {
		var stat unix.Stat_t
		dir, err := s.pin(folder)
		if err != nil {
			return stat, false
		}
		defer dir.close()
		return stat, unix.Fstat(dir.fd, &stat) == nil
	}
	a, okA := identity(left)
	b, okB := identity(right)
	return okA && okB && a.Dev == b.Dev && a.Ino == b.Ino
}

// copyChecked copies source to file, hashing what it reads. checkpoint runs
// at least every 16 MiB written.
func copyChecked(ctx context.Context, file *os.File, source io.Reader, checkpoint func(written int64) error) (int64, string, error) {
	digest := sha256.New()
	buffer := make([]byte, 1<<20)
	var written, checked int64
	for {
		if ctx.Err() != nil {
			return 0, "", errCopyStalled
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			digest.Write(buffer[:n])
			if _, err := file.Write(buffer[:n]); err != nil {
				if errors.Is(err, unix.ENOSPC) {
					return 0, "", errCopySpace
				}
				return 0, "", errCopyFailed
			}
			written += int64(n)
			if written-checked >= 16<<20 || readErr == io.EOF {
				checked = written
				if err := checkpoint(written); err != nil {
					return 0, "", err
				}
			}
		}
		if readErr == io.EOF {
			return written, hex.EncodeToString(digest.Sum(nil)), nil
		}
		if readErr != nil {
			return 0, "", errCopySource
		}
	}
}

// verifyFile reads a written copy back and checks its size and checksum.
func verifyFile(ctx context.Context, dir pinned, name string, size int64, checksum string) error {
	fd, err := unix.Openat(dir.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return errCopyFailed
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return errCopyMismatch
	}
	digest := sha256.New()
	read, err := io.Copy(digestWriter{ctx, digest}, file)
	if err != nil {
		if ctx.Err() != nil {
			return errCopyStalled
		}
		return errCopyMismatch
	}
	if read != size || hex.EncodeToString(digest.Sum(nil)) != checksum {
		return errCopyMismatch
	}
	return nil
}

type digestWriter struct {
	ctx    context.Context
	digest hash.Hash
}

func (w digestWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.digest.Write(p)
}

// assertCopyFolder checks a second copy's folder the way the backup folder
// is checked, and that it is separate from main and from game data.
func (s *Service) assertCopyFolder(ctx context.Context, folder, main string) error {
	resolved, err := s.Storage.validateFolder(ctx, folder)
	if err != nil {
		return err
	}
	if overlaps(resolved, filepath.Clean(main)) {
		return errCopyOverlap
	}
	// Two container paths can be the same host folder.
	if mainResolved, err := s.Storage.ApprovedDirectory(main); err == nil {
		if s.Storage.sameDirectory(resolved, mainResolved) {
			return errCopyOverlap
		}
		copyMounts, err := s.hostMounts(ctx, resolved)
		if err != nil {
			return err
		}
		mainMounts, err := s.hostMounts(ctx, mainResolved)
		if err != nil {
			return err
		}
		for _, left := range copyMounts {
			for _, right := range mainMounts {
				if MountsOverlap(left, right) {
					return errCopyOverlap
				}
			}
		}
	}
	if s.Observer == nil {
		return nil
	}
	observed, err := s.Observer.List(ctx)
	if err != nil {
		return fail("BACKUP_MOUNT_UNVERIFIED", "Ludock could not check that the second folder is separate from game data. Check its Docker connection and try again.")
	}
	for _, entry := range observed {
		if err := s.AssertSeparateDestination(ctx, entry.Observation, Settings{Destination: folder}); err != nil {
			if apperr.Is(err, "BACKUP_SOURCE_OVERLAP") {
				return fail("BACKUP_SOURCE_OVERLAP", "The second folder overlaps game data. Mount a separate folder.")
			}
			return err
		}
	}
	return nil
}

// ValidateCopyFolder checks settings' second copy folder, if any.
func (s *Service) ValidateCopyFolder(ctx context.Context, settings Settings) error {
	if settings.CopyTo == "" {
		return nil
	}
	return s.assertCopyFolder(ctx, settings.CopyTo, settings.Destination)
}

// copies is the copier's in-memory state: the backup being copied, and a
// request to look for due copies now.
type copies struct {
	mu      sync.Mutex
	current string
	wake    chan struct{}
}

func (c *copies) channel() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wake == nil {
		c.wake = make(chan struct{}, 1)
	}
	return c.wake
}

func (c *copies) set(id string) {
	c.mu.Lock()
	c.current = id
	c.mu.Unlock()
}

// Copying reports whether a backup's second copy is being made.
func (s *Service) Copying(id string) bool {
	s.copies.mu.Lock()
	defer s.copies.mu.Unlock()
	return s.copies.current == id
}

// WakeCopies asks the copier to look for due copies now.
func (s *Service) WakeCopies() {
	select {
	case s.copies.channel() <- struct{}{}:
	default:
	}
}

// CopyWake receives after WakeCopies.
func (s *Service) CopyWake() <-chan struct{} { return s.copies.channel() }

// copyLimit is how many due copies one query looks at.
const copyLimit = 50

// CopyDue makes due second copies one at a time, skipping servers that have
// work running so a copy never reads while a backup or restore holds them.
func (s *Service) CopyDue(ctx context.Context) error {
	for {
		if err := s.finishCopyRemovals(ctx); err != nil {
			return err
		}
		attempted, err := s.copyBatch(ctx)
		if err != nil || !attempted {
			return err
		}
	}
}

// copyBatch attempts the copies due now and reports whether it tried any.
func (s *Service) copyBatch(ctx context.Context) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+backupColumns+` FROM backups
    WHERE state='complete' AND copy_state IN ('pending','failed') AND copy_next_at<=?
    ORDER BY copy_next_at,created_at,id LIMIT ?`, time.Now().UnixMilli(), copyLimit)
	if err != nil {
		return false, err
	}
	var due []*Backup
	for rows.Next() {
		backup, err := scanBackup(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		due = append(due, backup)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	reserve := int64(DefaultReserve)
	if settings, _, err := s.SavedSettings(ctx); err == nil && settings != nil {
		reserve = settings.ReserveBytes
	}
	attempted := false
	for _, backup := range due {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if s.Locks != nil && s.Locks.ServerBusy(backup.ServerID) {
			continue
		}
		attempted = true
		if err := s.copyOne(ctx, backup, reserve); err != nil {
			return false, err
		}
	}
	return attempted, nil
}

// copyOne makes one attempt at a backup's second copy. Only a database
// error is returned; a failed copy is recorded on the backup.
func (s *Service) copyOne(ctx context.Context, backup *Backup, reserve int64) error {
	attempt := backup.copyAttempts + 1
	wait := copyRetry[min(attempt, len(copyRetry))-1]
	result, err := s.DB.ExecContext(ctx, `UPDATE backups SET copy_attempts=?, copy_next_at=?
    WHERE id=? AND state='complete' AND copy_state IN ('pending','failed') AND copy_destination=? AND copy_attempts=?`,
		attempt, time.Now().Add(wait).UnixMilli(), backup.ID, backup.copyDestination, backup.copyAttempts)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		return err
	}
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.copies.set(backup.ID)
	defer s.copies.set("")
	var stalled atomic.Bool
	err = s.makeCopy(copyCtx, backup, reserve, func() { stalled.Store(true); cancel() })
	if err == nil {
		result, err := s.DB.ExecContext(ctx, `UPDATE backups SET copy_state='complete', copy_error=''
      WHERE id=? AND state='complete' AND copy_destination=?`, backup.ID, backup.copyDestination)
		if err != nil {
			return err
		}
		// Removed or sent elsewhere while copying: the copy isn't wanted.
		if changed, _ := result.RowsAffected(); changed == 0 {
			s.Storage.Remove(backup.copyDestination, backup.ID, nil)
		}
		return nil
	}
	if copyCtx.Err() != nil && !stalled.Load() {
		// Stopped for a removal or shutdown; nothing failed.
		return nil
	}
	problem := errCopyFailed
	if public, ok := apperr.As(err); ok && strings.HasPrefix(public.Code, "BACKUP_") {
		problem = public
	}
	state := "pending"
	if attempt >= copyAttemptsBeforeAlert {
		state = "failed"
	}
	// The wait counts from the failure, however long the attempt took. The
	// first change to failed queues its alert in the same transaction.
	alerted := false
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE backups SET copy_error=?, copy_next_at=?, copy_state=?
      WHERE id=? AND state='complete' AND copy_state=? AND copy_destination=? AND copy_attempts=?`,
			problem.Message, time.Now().Add(wait).UnixMilli(), state, backup.ID, backup.CopyState, backup.copyDestination, attempt)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil || changed == 0 || state != "failed" || backup.CopyState == "failed" ||
			s.CopyFailed == nil {
			return err
		}
		alerted = true
		return s.CopyFailed(ctx, tx, backup.ID, backup.ServerID, problem.Message)
	})
	if err == nil && alerted && s.Alerted != nil {
		s.Alerted()
	}
	return err
}

func (s *Service) makeCopy(ctx context.Context, backup *Backup, reserve int64, stall func()) error {
	// A removal marks the row deleting before it stops copies, so a copy
	// registered after that sees it here.
	var state, destination string
	err := s.DB.QueryRowContext(ctx, "SELECT state,copy_destination FROM backups WHERE id=?", backup.ID).Scan(&state, &destination)
	if err != nil || state != "complete" || destination != backup.copyDestination {
		return context.Canceled
	}
	if testHook != nil {
		testHook("copying")
	}
	if err := s.assertCopyFolder(ctx, backup.copyDestination, backup.destination); err != nil {
		if apperr.Is(err, "BACKUP_DESTINATION") {
			if _, missing := s.Storage.ApprovedDirectory(backup.copyDestination); missing != nil {
				return errCopyMissing
			}
			return errCopyReadOnly
		}
		return err
	}
	if s.Storage.HasCopy(ctx, backup.copyDestination, backup.ID+".tar", backup.Size, backup.Checksum) {
		return nil
	}
	source, err := s.Storage.Open(backup.destination, backup.ID)
	if err != nil {
		return errCopySource
	}
	defer source.Close()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	watched := make(chan struct{})
	defer close(watched)
	go func() {
		ticker := time.NewTicker(copyStall / 4)
		defer ticker.Stop()
		for {
			select {
			case <-watched:
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, last.Load())) > copyStall {
					stall()
					return
				}
			}
		}
	}()
	return s.Storage.CopyFile(ctx, source, backup.copyDestination, backup.ID+".tar", backup.Size, backup.Checksum, reserve,
		func() { last.Store(time.Now().UnixNano()) })
}

// markCopyRemoval hands a removed backup's second copy to the copier, which
// removes it without holding up the removal; false means it has none. A copy
// never attempted wrote nothing, and one starting now finds no row.
func (s *Service) markCopyRemoval(ctx context.Context, id string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE backups SET copy_state='removing'
    WHERE id=? AND copy_destination<>'' AND (copy_attempts>0 OR copy_state IN ('complete','removing'))`, id)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if changed > 0 {
		s.WakeCopies()
	}
	return changed > 0, err
}

// finishCopyRemovals removes the second copies of removed backups, then
// their rows. It runs on the copier, so no attempt of the same copy is still
// writing, and a folder that is away is tried again on the next pass.
func (s *Service) finishCopyRemovals(ctx context.Context) error {
	// All of them each pass: removals stuck on a folder that is away must not
	// hold back the rest.
	rows, err := s.DB.QueryContext(ctx, `SELECT id,copy_destination FROM backups WHERE state='deleting' AND copy_state='removing'
    ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	type removal struct{ id, folder string }
	var pending []removal
	for rows.Next() {
		var r removal
		if err := rows.Scan(&r.id, &r.folder); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range pending {
		if s.Storage.Remove(r.folder, r.id, nil) != nil || s.Storage.RemovePartial(r.folder, r.id) != nil {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM backups WHERE id=? AND state='deleting' AND copy_state='removing'", r.id); err != nil {
			return err
		}
	}
	return nil
}

// ArchiveFolder is where a backup's archive is read from: its own folder,
// or its second copy when the archive is missing there. It sets FromCopy.
func (s *Service) ArchiveFolder(backup *Backup) string {
	if backup.CopyState == "complete" && !s.Storage.present(backup.destination, backup.ID) &&
		s.Storage.present(backup.copyDestination, backup.ID) {
		backup.FromCopy = true
		return backup.copyDestination
	}
	return backup.destination
}

// Locate sets FromCopy and Copying on complete backups.
func (s *Service) Locate(list []Backup) {
	for index := range list {
		if backup := &list[index]; backup.State == "complete" {
			s.ArchiveFolder(backup)
			backup.Copying = s.Copying(backup.ID)
		}
	}
}

// retarget points copies not made yet at the newly saved folder, and queues
// a copy of every backup that has none.
func retarget(ctx context.Context, tx *sql.Tx, copyTo string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE backups SET copy_destination='', copy_state='', copy_attempts=0, copy_next_at=0, copy_error=''
    WHERE state='complete' AND copy_state IN ('pending','failed') AND copy_destination<>?`, copyTo); err != nil {
		return err
	}
	if copyTo == "" {
		return nil
	}
	// Saving again retries failed copies now.
	_, err := tx.ExecContext(ctx, `UPDATE backups SET copy_destination=?, copy_state=CASE copy_state WHEN 'failed' THEN 'failed' ELSE 'pending' END,
    copy_next_at=0 WHERE state='complete' AND copy_state IN ('','pending','failed') AND destination<>?`, copyTo, copyTo)
	return err
}
