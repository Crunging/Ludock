package backups

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
)

// Ludock saves its own data (the database and identity key, from
// db.Snapshot) into the backup folder whenever backup storage is on, and the
// copier puts each save in the second folder too. Saves are plain files
// named ludock-data-<UTC time>.tar with no row in the backups table, so game
// backups' listing, retention, kept and size rules, browsing, and restores
// never see them; only the free-space reserve applies. Each folder keeps
// its own newest saves, pruned only right after a newer save is verified
// there, so a lost backup folder never removes the copies in the second.

const (
	dataEvery = 24 * time.Hour
	// dataSoonest is the shortest time between saves, when something worth
	// keeping changed since the last.
	dataSoonest    = 10 * time.Minute
	dataKeepNewest = 3
	dataKeepDays   = 7
	dataSaveKey    = "data-save"
	dataCopyKey    = "data-save-copy"
)

var (
	dataName = regexp.MustCompile(`^ludock-data-\d{8}-\d{6}\.tar$`)
	// dataPartial is a partial file CopyFile left writing a save.
	dataPartial = regexp.MustCompile(`^ludock-data-\d{8}-\d{6}\.tar\.[0-9a-f]{8}\.partial$`)
)

const dataStamp = "20060102-150405"

func dataFileName(at time.Time) string { return "ludock-data-" + at.UTC().Format(dataStamp) + ".tar" }

// dataTrack is the state of saving into one folder: the last save that
// succeeded, with the snapshot's own size and checksum, and the failures
// since in FailedIn.
type dataTrack struct {
	At           int64  `json:"at"`
	Name         string `json:"name"`
	Folder       string `json:"folder"`
	Size         int64  `json:"size"`
	Checksum     string `json:"checksum"`
	FailedIn     string `json:"failedIn,omitempty"`
	Failures     int    `json:"failures,omitempty"`
	FailingSince int64  `json:"failingSince,omitempty"`
	Problem      string `json:"problem,omitempty"`
	NextAt       int64  `json:"nextAt,omitempty"`
	Alerted      bool   `json:"alerted,omitempty"`
}

// dataTrack reads a track for folder: failures in another folder don't
// hold back or alert for this one.
func (s *Service) dataTrack(ctx context.Context, key, folder string) (dataTrack, error) {
	var track dataTrack
	_, err := s.DB.Setting(ctx, key, &track)
	if track.FailedIn != folder {
		track.FailedIn, track.Failures, track.FailingSince, track.Problem, track.NextAt, track.Alerted = "", 0, 0, "", 0, false
	}
	return track, err
}

var errDataSnapshot = fail("BACKUP_DATA_SNAPSHOT", "Ludock couldn’t read its own data to save it. Check free space where Ludock keeps its data.")

// dataProblem says why a save into the backup folder failed. CopyFile's
// messages name the second folder, which this isn't.
func dataProblem(err error) string {
	switch {
	case apperr.Is(err, "BACKUP_DATA_SNAPSHOT"):
		return errDataSnapshot.Message
	case apperr.Is(err, "BACKUP_COPY_SPACE"), apperr.Is(err, codeDisk):
		return "The backup folder is down to its free-space reserve."
	case apperr.Is(err, "BACKUP_COPY_STALLED"):
		return "The backup folder stopped responding."
	case apperr.Is(err, "BACKUP_SOURCE_OVERLAP"):
		return "The backup folder overlaps game data, where game servers could read it."
	case apperr.Is(err, "BACKUP_MOUNT_UNVERIFIED"):
		return "Ludock couldn’t check that the backup folder is a mount separate from game data."
	case apperr.Is(err, "BACKUP_COPY_FOLDER"), apperr.Is(err, "BACKUP_DESTINATION"):
		return "The backup folder is missing, not mounted, or not writable."
	}
	return "The copy couldn’t be written to the backup folder."
}

// copyProblem says why a save's second copy failed, as the copier does.
func copyProblem(err error) string {
	if public, ok := apperr.As(err); ok && strings.HasPrefix(public.Code, "BACKUP_") {
		return public.Message
	}
	return errCopyFailed.Message
}

// dataDue reports whether a save into folder is due at now.
func (s *Service) dataDue(ctx context.Context, track dataTrack, folder string, now time.Time) (bool, error) {
	switch since := now.Sub(time.UnixMilli(track.At)); {
	case now.UnixMilli() < track.NextAt:
		return false, nil
	case track.At == 0 || track.Folder != folder || track.Failures > 0 || since >= dataEvery:
		return true, nil
	case since < dataSoonest:
		return false, nil
	}
	return s.changedSince(ctx, track.At)
}

// changedSince reports a change worth saving soon after: a new game backup,
// a kept one, or an administrator's change to accounts, sharing, schedules,
// or settings. Sign-ins, downloads, and server work wait for the daily save.
func (s *Service) changedSince(ctx context.Context, at int64) (bool, error) {
	var changed bool
	err := s.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM backups WHERE state='complete' AND (created_at>? OR kept_at>?))
    OR EXISTS(SELECT 1 FROM audit_log WHERE created_at>? AND (action LIKE 'user.%' OR action LIKE 'settings.%' OR action LIKE 'schedule.%'
      OR action LIKE 'server.sharing.%' OR action LIKE 'availability.%'
      OR action IN ('notifications.configured','backup.released','backup.deleted','auth.setup.completed','auth.password.changed',
        'auth.two-factor.enabled','auth.two-factor.disabled')))`, at, at, at).Scan(&changed)
	return changed, err
}

// SaveData saves Ludock's own data into the backup folder when a save is
// due. A failure is recorded and retried with backoff; it never affects game
// backups. The caller runs one at a time.
func (s *Service) SaveData(ctx context.Context) error {
	settings, _, err := s.SavedSettings(ctx)
	if err != nil || settings == nil {
		return err
	}
	track, err := s.dataTrack(ctx, dataSaveKey, settings.Destination)
	if err != nil {
		return err
	}
	now := time.Now()
	if due, err := s.dataDue(ctx, track, settings.Destination, now); err != nil || !due {
		return err
	}
	done := dataTrack{At: now.UnixMilli(), Name: dataFileName(now), Folder: settings.Destination}
	err = s.writeData(ctx, *settings, &done)
	if ctx.Err() != nil {
		return nil
	}
	problem := ""
	if err != nil {
		problem = dataProblem(err)
	}
	if err := s.finishData(ctx, dataSaveKey, track, done, problem); err != nil {
		return err
	}
	if problem == "" && settings.CopyTo != "" {
		s.WakeCopies()
	}
	return nil
}

// writeData snapshots the data and publishes it as done.Name, setting
// done's size and checksum. The folder is checked like a game backup's,
// and kept apart from game data, whose file helpers could read it.
func (s *Service) writeData(ctx context.Context, settings Settings, done *dataTrack) error {
	snapshot, err := s.DB.Snapshot(ctx)
	if err != nil {
		return errDataSnapshot
	}
	defer snapshot.Close()
	done.Size, done.Checksum = snapshot.Size, snapshot.Checksum
	if testHook != nil {
		testHook("saving")
	}
	return s.isolate(ctx, settings.Destination, func(ctx context.Context, progress func()) error {
		if _, err := s.Storage.validateFolder(ctx, settings.Destination); err != nil {
			return err
		}
		if err := s.separateFromGames(ctx, settings.Destination, fail("BACKUP_MOUNT_UNVERIFIED", "")); err != nil {
			return err
		}
		progress()
		return s.Storage.publishData(ctx, snapshot, settings.Destination, done.Name, snapshot.Size, snapshot.Checksum, settings.ReserveBytes, progress)
	})
}

// CopyData puts the newest save in the second folder, if one is chosen and
// it isn't there yet. The copier calls it after game backups' copies, so it
// never starts I/O in a folder where theirs is running.
func (s *Service) CopyData(ctx context.Context) error {
	settings, _, err := s.SavedSettings(ctx)
	if err != nil || settings == nil || settings.CopyTo == "" {
		return err
	}
	saved, err := s.dataTrack(ctx, dataSaveKey, settings.Destination)
	if err != nil || saved.Name == "" || saved.Folder != settings.Destination || saved.Checksum == "" {
		return err
	}
	track, err := s.dataTrack(ctx, dataCopyKey, settings.CopyTo)
	if err != nil {
		return err
	}
	now := time.Now()
	if track.Folder == settings.CopyTo && track.Name == saved.Name || now.UnixMilli() < track.NextAt {
		return nil
	}
	err = s.isolate(ctx, settings.CopyTo, func(ctx context.Context, progress func()) error {
		if err := s.assertCopyFolder(ctx, settings.CopyTo, settings.Destination); err != nil {
			if apperr.Is(err, "BACKUP_DESTINATION") {
				if _, missing := s.Storage.ApprovedDirectory(settings.CopyTo); missing != nil {
					return errCopyMissing
				}
				return errCopyReadOnly
			}
			return err
		}
		progress()
		// CopyFile checks the save against the snapshot it was made from.
		source, err := s.Storage.openNamed(saved.Folder, saved.Name)
		if err != nil {
			return errCopySource
		}
		defer source.Close()
		return s.Storage.publishData(ctx, source, settings.CopyTo, saved.Name, saved.Size, saved.Checksum, settings.ReserveBytes, progress)
	})
	if ctx.Err() != nil {
		return nil
	}
	problem := ""
	if err != nil {
		problem = copyProblem(err)
	}
	return s.finishData(ctx, dataCopyKey, track,
		dataTrack{At: now.UnixMilli(), Name: saved.Name, Folder: settings.CopyTo, Size: saved.Size, Checksum: saved.Checksum}, problem)
}

// finishData records an attempt's result: done when it succeeded, or a
// failure in done.Folder on track. The failure that reaches
// copyAttemptsBeforeAlert queues one alert, in the same transaction, for the
// run of failures.
func (s *Service) finishData(ctx context.Context, key string, track, done dataTrack, problem string) error {
	if problem == "" {
		return s.DB.SetSetting(ctx, nil, key, done)
	}
	if track.Failures == 0 {
		track.FailingSince = done.At
	}
	track.FailedIn = done.Folder
	track.Failures++
	track.Problem = problem
	track.NextAt = time.UnixMilli(done.At).Add(copyRetry[min(track.Failures, len(copyRetry))-1]).UnixMilli()
	alert := track.Failures >= copyAttemptsBeforeAlert && !track.Alerted && s.DataFailed != nil
	track.Alerted = track.Alerted || alert
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.DB.SetSetting(ctx, tx, key, track); err != nil {
			return err
		}
		if !alert {
			return nil
		}
		message := "Ludock couldn’t save a copy of its own data: " + problem
		if key == dataCopyKey {
			message = "The second copy of Ludock’s own data couldn’t be made: " + problem
		}
		return s.DataFailed(ctx, tx, key+"-failed:"+strconv.FormatInt(track.FailingSince, 10),
			message+" Ludock keeps trying; Settings shows when it last succeeded.")
	})
	if err == nil && alert && s.Alerted != nil {
		s.Alerted()
	}
	return err
}

// dataStatus is when Ludock's own data was last saved, 0 if never, and why
// saving it or its second copy is failing.
func (s *Service) dataStatus(ctx context.Context, settings Settings) (savedAt int64, problem string) {
	saved, err := s.dataTrack(ctx, dataSaveKey, settings.Destination)
	if err != nil {
		return 0, "Ludock couldn’t read when its own data was last saved."
	}
	if saved.Folder == settings.Destination {
		savedAt = saved.At
	}
	if saved.Failures > 0 {
		return savedAt, "Ludock couldn’t save its own data: " + saved.Problem + " It tries again shortly."
	}
	if settings.CopyTo == "" {
		return savedAt, ""
	}
	copied, err := s.dataTrack(ctx, dataCopyKey, settings.CopyTo)
	if err == nil && copied.Failures > 0 {
		return savedAt, "The second copy of Ludock’s own data couldn’t be made: " + copied.Problem + " Ludock keeps trying."
	}
	return savedAt, ""
}

// publishData writes a save into directory through CopyFile, then removes
// the older saves there that dataKeeps doesn't keep, and leftovers of
// interrupted saves.
func (s *Storage) publishData(ctx context.Context, source io.Reader, directory, name string, size int64, checksum string,
	reserve int64, progress func()) error {
	if err := s.removeDataLeftovers(directory); err != nil {
		return errCopyFailed
	}
	if err := s.CopyFile(ctx, source, directory, name, size, checksum, reserve, progress); err != nil {
		return err
	}
	names, err := s.dataSaves(directory)
	if err != nil {
		return nil
	}
	keep := dataKeeps(names, time.Local)
	for _, saved := range names {
		// Only saves older than the one just verified make way for it.
		if !keep[saved] && saved < name {
			s.removeFile(directory, saved, nil)
		}
	}
	return nil
}

// dataSaves lists the saves in directory, newest first.
func (s *Storage) dataSaves(directory string) ([]string, error) {
	names, err := s.list(directory)
	if err != nil {
		return nil, err
	}
	names = slices.DeleteFunc(names, func(name string) bool { return !dataName.MatchString(name) })
	slices.Sort(names)
	slices.Reverse(names)
	return names, nil
}

// removeDataLeftovers removes the partial files of interrupted saves. Saves
// into one folder run one at a time, so none is being written.
func (s *Storage) removeDataLeftovers(directory string) error {
	names, err := s.list(directory)
	if err != nil {
		return err
	}
	dir, err := s.pin(directory)
	if err != nil {
		return err
	}
	defer dir.close()
	for _, name := range names {
		if dataPartial.MatchString(name) {
			if err := unix.Unlinkat(dir.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return nil
}

func (s *Storage) list(directory string) ([]string, error) {
	dir, err := s.pin(directory)
	if err != nil {
		return nil, err
	}
	defer dir.close()
	listing, err := unix.Dup(dir.fd)
	if err != nil {
		return nil, err
	}
	folder := os.NewFile(uintptr(listing), ".")
	defer folder.Close()
	return folder.Readdirnames(-1)
}

// dataKeeps returns the saves to keep among names, newest first: the newest
// dataKeepNewest, and the newest of each of the last dataKeepDays days that
// have one, in zone.
func dataKeeps(names []string, zone *time.Location) map[string]bool {
	keep := map[string]bool{}
	days := map[string]bool{}
	for index, name := range names {
		if index < dataKeepNewest {
			keep[name] = true
		}
		at, err := time.ParseInLocation(dataStamp, strings.TrimSuffix(strings.TrimPrefix(name, "ludock-data-"), ".tar"), time.UTC)
		if err != nil {
			continue
		}
		day := at.In(zone).Format(time.DateOnly)
		if !days[day] && len(days) < dataKeepDays {
			days[day] = true
			keep[name] = true
		}
	}
	return keep
}
