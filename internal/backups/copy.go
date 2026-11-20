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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/identity"
)

// Second copies: a complete backup recorded while Settings.CopyTo is set
// gets copy_state 'pending'. The copier makes it after the backup's work has
// ended and the server is back as it was, then marks it 'complete'. A
// failing copy is retried with backoff; after copyAttemptsBeforeAlert
// attempts it is 'failed', reported once, and still retried. Each attempt is
// counted before it starts, so an attempt interrupted by a restart waits its
// backoff like a failure. A copy counts only once its row says 'complete':
// it is written under a name of its own attempt, read back and checked, then
// published under its final name.
//
// Every file operation in the second folder, which is often a network share,
// runs on a goroutine of its own (isolate). The copier gives up on one that
// stops making progress and moves on; until it returns, nothing else starts
// in that folder.

const copyAttemptsBeforeAlert = 3

// copyRetry is the wait after each failed attempt; the last repeats.
var copyRetry = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 6 * time.Hour}

// copyStall ends a copy that makes no progress for this long.
var copyStall = 2 * time.Minute

// measureWait is how long the storage summary waits for the second folder.
var measureWait = 5 * time.Second

var (
	errFolderAway    = fail("BACKUP_FOLDER_AWAY", "The folder this backup was saved in can’t be reached.")
	errFolderStalled = fail("BACKUP_FOLDER_STALLED", "The backup folder isn’t responding. Check its disk or network share, then try again.")
)

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
// checksum, to name in directory. The source is checked as it is read; the
// copy is written under a name of this attempt's own, synced, read back from
// disk and checked, then published as name without replacing a valid file
// already there. It leaves the reserve free, and once ctx ends it stops and
// never publishes; progress is called after each write.
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
	// Partial files of interrupted attempts don't count against the space
	// this one needs. No earlier attempt is still writing: isolate starts no
	// I/O in a folder where one hasn't returned.
	if err := removePartials(dir, name); err != nil {
		return errCopyFailed
	}
	if _, err := budget(dir, reserve, size); err != nil {
		return errCopySpace
	}
	partial := name + "." + identity.NewUUID()[:8] + ".partial"
	fd, err := unix.Openat(dir.fd, partial, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		if errors.Is(err, unix.EROFS) || errors.Is(err, unix.EACCES) {
			return errCopyReadOnly
		}
		return errCopyFailed
	}
	file := os.NewFile(uintptr(fd), partial)
	published := false
	defer func() {
		file.Close()
		if !published {
			unix.Unlinkat(dir.fd, partial, 0)
		}
	}()
	if testHook != nil {
		testHook("writing")
	}
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
	if ctx.Err() != nil {
		return errCopyStalled
	}
	if err := publish(ctx, dir, partial, name, size, checksum); err != nil {
		return err
	}
	published = true
	if unix.Fsync(dir.fd) != nil {
		return errCopyFailed
	}
	return nil
}

// publish gives a verified partial file its final name. A valid file
// already there is kept, so an attempt that wakes after a newer one never
// replaces its file; an invalid one is replaced.
func publish(ctx context.Context, dir pinned, partial, name string, size int64, checksum string) error {
	err := unix.Renameat2(dir.fd, partial, dir.fd, name, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		// Network filesystems often lack RENAME_NOREPLACE; a hard link never
		// replaces either.
		if err = unix.Linkat(dir.fd, partial, dir.fd, name, 0); err == nil {
			unix.Unlinkat(dir.fd, partial, 0)
		}
	}
	if errors.Is(err, unix.EEXIST) {
		if verifyFile(ctx, dir, name, size, checksum) == nil {
			unix.Unlinkat(dir.fd, partial, 0)
			return nil
		}
		if ctx.Err() != nil {
			return errCopyStalled
		}
		err = unix.Renameat(dir.fd, partial, dir.fd, name)
	} else if err != nil && !errors.Is(err, unix.ENOENT) {
		// No exclusive way to publish here: rename only onto nothing.
		var stat unix.Stat_t
		if unix.Fstatat(dir.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW) != nil && ctx.Err() == nil {
			err = unix.Renameat(dir.fd, partial, dir.fd, name)
		}
	}
	if err != nil {
		return errCopyFailed
	}
	return nil
}

// removePartials removes the partial files of name's earlier attempts.
func removePartials(dir pinned, name string) error {
	listing, err := unix.Dup(dir.fd)
	if err != nil {
		return err
	}
	folder := os.NewFile(uintptr(listing), ".")
	defer folder.Close()
	names, err := folder.Readdirnames(-1)
	if err != nil {
		return err
	}
	for _, entry := range names {
		if strings.HasPrefix(entry, name+".") && strings.HasSuffix(entry, ".partial") {
			if err := unix.Unlinkat(dir.fd, entry, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return nil
}

// RemoveCopy removes a copy named name from directory, with the partial
// files of its attempts. Only a regular file is removed.
func (s *Storage) RemoveCopy(directory, name string) error {
	if !fileName.MatchString(name) || strings.HasSuffix(name, ".partial") {
		return fail("INVALID_REQUEST", "Invalid file name.")
	}
	if err := s.removeFile(directory, name, nil); err != nil {
		return err
	}
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	return removePartials(dir, name)
}

// HasCopy reports whether name in directory is already a complete copy:
// one published by an attempt interrupted before it was recorded.
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
	if testHook != nil {
		testHook("verifying")
	}
	// Reading a large copy back can take longer than copyStall; only a
	// read that stops making progress is a stall.
	if progress, ok := w.ctx.Value(progressKey{}).(func()); ok {
		progress()
	}
	return w.digest.Write(p)
}

// progressKey carries isolate's progress callback to I/O that has no other
// way to report it.
type progressKey struct{}

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
	err = s.separateFromGames(ctx, folder, fail("BACKUP_MOUNT_UNVERIFIED",
		"Ludock could not check that the second folder is separate from game data. Check its Docker connection and try again."), false)
	if apperr.Is(err, "BACKUP_SOURCE_OVERLAP") {
		return fail("BACKUP_SOURCE_OVERLAP", "The second folder overlaps game data. Mount a separate folder.")
	}
	return err
}

// separateFromGames checks folder against every game server's data;
// unlisted is the error when the servers can't be listed. With readable,
// read-only game mounts count too, for files games must not read.
func (s *Service) separateFromGames(ctx context.Context, folder string, unlisted error, readable bool) error {
	if s.Observer == nil {
		return nil
	}
	observed, err := s.Observer.List(ctx)
	if err != nil {
		return unlisted
	}
	for _, entry := range observed {
		observation := entry.Observation
		if readable {
			observation.Mounts = slices.Clone(observation.Mounts)
			for index := range observation.Mounts {
				observation.Mounts[index].Writable = true
			}
		}
		if err := s.AssertSeparateDestination(ctx, observation, Settings{Destination: folder}); err != nil {
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

// copies is the copier's in-memory state: the backup being copied, folders
// whose abandoned I/O hasn't returned, and a request to look for due copies
// now.
type copies struct {
	mu      sync.Mutex
	current string
	stuck   map[string]bool
	// measuring holds folders whose free-space reading hasn't returned.
	measuring map[string]bool
	// hung counts each folder's looks (reach) that were given up on and
	// haven't returned.
	hung map[string]int
	wake chan struct{}
}

// measure claims a folder's free-space reading; false while an earlier one
// is still waiting on the folder, so readings of a stuck share don't pile up.
func (c *copies) measure(folder string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.measuring[folder] {
		return false
	}
	if c.measuring == nil {
		c.measuring = map[string]bool{}
	}
	c.measuring[folder] = true
	return true
}

func (c *copies) measured(folder string) {
	c.mu.Lock()
	delete(c.measuring, folder)
	c.mu.Unlock()
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

// enter claims a folder for I/O; false while earlier I/O there is stuck.
func (c *copies) enter(folder string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stuck[folder] {
		return false
	}
	if c.stuck == nil {
		c.stuck = map[string]bool{}
	}
	c.stuck[folder] = true
	return true
}

func (c *copies) leave(folder string) {
	c.mu.Lock()
	delete(c.stuck, folder)
	c.mu.Unlock()
}

// isolate runs fn, file I/O in folder, on a goroutine of its own. It gives
// fn up when ctx ends (returning ctx's error) or when fn makes no progress
// for copyStall (errCopyStalled), and cancels fn's context either way. A
// call blocked in the kernel can't see that, so until fn returns the folder
// stays claimed and later calls fail at once with errCopyStalled.
func (s *Service) isolate(ctx context.Context, folder string, fn func(ctx context.Context, progress func()) error) error {
	if !s.copies.enter(folder) {
		return errCopyStalled
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan error, 1)
	progress := func() { last.Store(time.Now().UnixNano()) }
	work = context.WithValue(work, progressKey{}, progress)
	go func() {
		err := fn(work, progress)
		// Released before the result is reported, so the caller's next
		// call in this folder doesn't find it still claimed.
		s.copies.leave(folder)
		done <- err
	}()
	ticker := time.NewTicker(copyStall / 4)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Since(time.Unix(0, last.Load())) > copyStall {
				return errCopyStalled
			}
		}
	}
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
	s.copies.set(backup.ID)
	defer s.copies.set("")
	err = s.makeCopy(ctx, backup, reserve)
	if err == nil {
		result, err := s.DB.ExecContext(ctx, `UPDATE backups SET copy_state='complete', copy_error='', copy_previous=''
      WHERE id=? AND state='complete' AND copy_destination=?`, backup.ID, backup.copyDestination)
		if err != nil {
			return err
		}
		// Removed or sent elsewhere while copying: the copy isn't wanted.
		if changed, _ := result.RowsAffected(); changed == 0 {
			s.isolate(ctx, backup.copyDestination, func(context.Context, func()) error {
				return s.Storage.RemoveCopy(backup.copyDestination, backup.ID+".tar")
			})
		}
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		// Shutdown, or the backup was removed or sent elsewhere; nothing
		// failed.
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

func (s *Service) makeCopy(ctx context.Context, backup *Backup, reserve int64) error {
	// A removal marks the row deleting before the copier next looks, so a
	// copy starting after that sees it here.
	var state, destination string
	err := s.DB.QueryRowContext(ctx, "SELECT state,copy_destination FROM backups WHERE id=?", backup.ID).Scan(&state, &destination)
	if err != nil || state != "complete" || destination != backup.copyDestination {
		return context.Canceled
	}
	if testHook != nil {
		testHook("copying")
	}
	return s.isolate(ctx, backup.copyDestination, func(ctx context.Context, progress func()) error {
		if err := s.assertCopyFolder(ctx, backup.copyDestination, backup.destination); err != nil {
			if apperr.Is(err, "BACKUP_DESTINATION") {
				if _, missing := s.Storage.ApprovedDirectory(backup.copyDestination); missing != nil {
					return errCopyMissing
				}
				return errCopyReadOnly
			}
			return err
		}
		progress()
		if s.Storage.HasCopy(ctx, backup.copyDestination, backup.ID+".tar", backup.Size, backup.Checksum) {
			return nil
		}
		// The previous second copy stands in for an archive missing from the
		// backup folder; CopyFile checks it against the original's checksum.
		folder, err := s.ArchiveFolder(backup)
		if err != nil {
			return err
		}
		source, err := s.Storage.Open(folder, backup.ID)
		if err != nil {
			return errCopySource
		}
		defer source.Close()
		return s.Storage.CopyFile(ctx, source, backup.copyDestination, backup.ID+".tar", backup.Size, backup.Checksum, reserve, progress)
	})
}

// markCopyRemoval hands a removed backup's second copies to the copier,
// which removes them without holding up the removal; false means it has
// none. A copy never attempted wrote nothing, and one starting now finds no
// row.
func (s *Service) markCopyRemoval(ctx context.Context, id string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE backups SET copy_state='removing'
    WHERE id=? AND (copy_state='removing' OR copy_previous<>'' OR (copy_destination<>'' AND (copy_attempts>0 OR copy_state='complete')))`, id)
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
// their rows. A folder that is away or stuck is tried again on the next pass.
func (s *Service) finishCopyRemovals(ctx context.Context) error {
	// All of them each pass: removals stuck on a folder that is away must not
	// hold back the rest.
	rows, err := s.DB.QueryContext(ctx, `SELECT id,copy_destination,copy_previous FROM backups WHERE state='deleting' AND copy_state='removing'
    ORDER BY created_at,id`)
	if err != nil {
		return err
	}
	type removal struct{ id, folder, previous string }
	var pending []removal
	for rows.Next() {
		var r removal
		if err := rows.Scan(&r.id, &r.folder, &r.previous); err != nil {
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
		removed := true
		for _, folder := range []string{r.folder, r.previous} {
			if folder == "" || !removed {
				continue
			}
			removed = s.isolate(ctx, folder, func(context.Context, func()) error {
				return s.Storage.RemoveCopy(folder, r.id+".tar")
			}) == nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !removed {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, "DELETE FROM backups WHERE id=? AND state='deleting' AND copy_state='removing'", r.id); err != nil {
			return err
		}
	}
	return nil
}

// reach runs fn, a look into folder, on a goroutine of its own. It gives fn
// up with stalled after limit without progress. A look blocked in the kernel
// can't be cancelled, so until it returns, later looks into that folder fail
// at once instead of piling up on it.
func (s *Service) reach(folder string, stalled error, limit time.Duration, fn func(progress func()) error) error {
	c := &s.copies
	c.mu.Lock()
	if c.hung[folder] > 0 {
		c.mu.Unlock()
		return stalled
	}
	c.mu.Unlock()
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan error, 1)
	// Both guarded by c.mu: whichever side comes second settles the count.
	finished, abandoned := false, false
	go func() {
		if testHook != nil {
			testHook("reaching")
		}
		err := fn(func() { last.Store(time.Now().UnixNano()) })
		c.mu.Lock()
		finished = true
		if abandoned {
			if c.hung[folder]--; c.hung[folder] <= 0 {
				delete(c.hung, folder)
			}
		}
		c.mu.Unlock()
		done <- err
	}()
	ticker := time.NewTicker(min(limit/4, time.Second))
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
		}
		if time.Since(time.Unix(0, last.Load())) <= limit {
			continue
		}
		c.mu.Lock()
		if finished {
			c.mu.Unlock()
			return <-done
		}
		abandoned = true
		if c.hung == nil {
			c.hung = map[string]int{}
		}
		c.hung[folder]++
		c.mu.Unlock()
		return stalled
	}
}

// look reports whether a backup's archive is in folder, within measureWait.
func (s *Service) look(folder, id string, stalled error) (bool, error) {
	var found bool
	err := s.reach(folder, stalled, measureWait, func(func()) error {
		var err error
		found, err = s.Storage.present(folder, id)
		return err
	})
	return err == nil && found, err
}

// ArchiveFolder is where a backup's archive is read from: its own folder,
// or a second copy when the archive is missing there, preferring the one in
// the chosen folder. It sets FromCopy and FolderAway (also when that folder
// isn't responding), and fails when a folder it must look into isn't.
func (s *Service) ArchiveFolder(backup *Backup) (string, error) {
	backup.FromCopy = false
	found, err := s.look(backup.destination, backup.ID, errFolderStalled)
	backup.FolderAway = err != nil
	if errors.Is(err, errFolderStalled) {
		return "", err
	}
	if found {
		return backup.destination, nil
	}
	var copies []string
	if backup.CopyState == "complete" {
		copies = append(copies, backup.copyDestination)
	}
	for _, folder := range append(copies, backup.copyPrevious) {
		if folder == "" {
			continue
		}
		found, err := s.look(folder, backup.ID, errCopyStalled)
		if errors.Is(err, errCopyStalled) {
			return "", err
		}
		if found {
			backup.FromCopy = true
			return folder, nil
		}
	}
	return backup.destination, nil
}

// Locate sets FromCopy and FolderAway on backups and Copying on complete
// ones. It returns the first folder that isn't responding.
func (s *Service) Locate(list []Backup) error {
	var stalled error
	for index := range list {
		backup := &list[index]
		switch backup.State {
		case "complete":
			if _, err := s.ArchiveFolder(backup); err != nil && stalled == nil {
				stalled = err
			}
			backup.Copying = s.Copying(backup.ID)
		case "deleting":
			err := s.reach(backup.destination, errFolderStalled, measureWait, func(func()) error {
				return s.Storage.reachable(backup.destination)
			})
			backup.FolderAway = err != nil
			if errors.Is(err, errFolderStalled) && stalled == nil {
				stalled = err
			}
		}
	}
	return stalled
}

// Validate checks a backup's archive in folder, from ArchiveFolder, as
// Storage.Validate does, or one item of it as ValidateItem does, and gives
// up when the folder stops responding.
func (s *Service) Validate(backup *Backup, folder, rootID, item string) (map[string]int64, error) {
	stalled := errFolderStalled
	if backup.FromCopy {
		stalled = errCopyStalled
	}
	var staged map[string]int64
	err := s.reach(folder, stalled, copyStall, func(progress func()) error {
		if item == "" {
			var err error
			staged, err = s.Storage.Validate(folder, backup.ID, backup.Roots, backup.Size, backup.Checksum, progress)
			return err
		}
		bytes, err := s.Storage.ValidateItem(folder, backup.ID, backup.Roots, backup.Size, backup.Checksum, rootID, item, progress)
		staged = map[string]int64{rootID: bytes}
		return err
	})
	if err != nil {
		return nil, err
	}
	return staged, nil
}

// retarget points every copy not in the newly saved folder at it. A copy
// made in another folder stays tracked as the previous copy, for reading and
// for removal with its backup, until the new folder has one; then it is left
// where it is, untracked. With no folder saved, previous copies stay tracked.
func retarget(ctx context.Context, tx *sql.Tx, copyTo string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE backups SET copy_previous=copy_destination, copy_destination='', copy_state='', copy_attempts=0,
    copy_next_at=0, copy_error='' WHERE state='complete' AND copy_state='complete' AND copy_destination<>?`, copyTo); err != nil {
		return err
	}
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
