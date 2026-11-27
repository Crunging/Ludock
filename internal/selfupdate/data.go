package selfupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// How a put-back left the data.
const (
	DataRestored = "restored"
	// DataKept: no copy was there to put back, so the newer data stayed.
	DataKept = "kept"
	// DataNotRestored: the copy is there, but Ludock couldn't put it back.
	DataNotRestored = "not_restored"
)

// BeforeOpen runs as Ludock starts in container, before it opens its
// database in dataDir, and decides from the update that database records.
// When the helper put this original back, or the update was cut off with no
// word from it, as by a reboot, it renames the data copy over the database,
// so the version that wrote the copy restores it. It returns DataRestored or
// "", with the data folder's lock for this Ludock to hold while it runs, and
// an error when this Ludock must not start: two would serve.
func BeforeOpen(ctx context.Context, client *docker.Client, container, dataDir string, wait time.Duration) (string, io.Closer, error) {
	restore, id := false, ""
	if container != "" {
		record, ok, err := saved(dataDir)
		if err != nil {
			return "", nil, err
		}
		if ok {
			if restore, err = decide(ctx, client, container, dataDir, record, wait); err != nil {
				return "", nil, err
			}
			id = record.ID
		}
		// No Ludock opens the data, or puts the copy back, while another
		// container of an update that didn't finish may serve on it: every
		// way to start passes here, and anything but Docker showing the
		// others stopped or gone refuses, for Docker to start this one again
		// later.
		if ok && (record.State == StateUpdating || record.State == StateInterrupted) {
			if err := alone(ctx, client, container, record); err != nil {
				return "", nil, err
			}
		}
	}
	// Nor while any other Ludock runs on the folder, in whatever container:
	// this one holds it from here. That comes after the decision, which may
	// stop an update's new container, so the original can still put it back.
	lock, err := db.Lock(dataDir)
	if err != nil {
		return "", nil, err
	}
	// Held, no other Ludock runs on the folder, and any that settled did so
	// before it let go: the record read now is the last word, even about a
	// new container Docker no longer showed by its name.
	if restore {
		_, unsettled, err := still(dataDir, id)
		if err != nil {
			lock.Close()
			return "", nil, err
		}
		restore = unsettled
	}
	if !restore {
		return "", lock, nil
	}
	if err := putDataBack(dataDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Ludock couldn't put back the copy of its data from before the update, so it starts on the newer data")
		}
		return "", lock, nil
	}
	log.Info("Ludock put back the copy of its data from before the update")
	return DataRestored, lock, nil
}

// decide reports whether container, starting, puts the update's copy back.
// It doesn't open or restore anything, so BeforeOpen's check covers it.
//
// Any conclusion that stops another container or restores the copy is drawn
// from the record as it is after the last look at the helper, never from one
// read before a wait: the new version may have settled the update meanwhile
// and accepted changes since. Ludock removes a helper only once an outcome is
// saved, or one that never ran, so a helper gone while the record still says
// updating means the update is unsettled, and its new version has accepted
// nothing.
func decide(ctx context.Context, client *docker.Client, container, dataDir string, record Record, wait time.Duration) (bool, error) {
	if record.Container != container {
		return false, nil
	}
	if record.State == StateInterrupted && record.OnLeftover {
		log.Warn("Ludock's last update stopped partway with Ludock running in its new container. This is the old container: it starts on the data as the new version left it, once that one has stopped, and Settings says what is left.",
			logging.F("newContainer", record.Leftover))
		return false, nil
	}
	if record.State != StateUpdating {
		return false, nil
	}

	// Docker not answering is waited out as a running helper is; after
	// that this original doesn't start, and its restart policy retries it.
	deadline := time.Now().Add(wait)
	code, helper := -1, false
	for {
		spec, err := client.InspectSpec(ctx, HelperName(record.ID))
		if docker.IsNotFound(err) {
			break
		}
		// Only Docker showing it proves this update's copy was written.
		helper = helper || err == nil
		if err == nil && !spec.State.Running && !spec.State.Restarting {
			if spec.State.Status == "exited" {
				code = spec.State.ExitCode
			}
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			return false, errBeside
		}
	}
	if code == ExitUpdated || code == ExitUnchanged {
		return false, nil
	}
	record, unsettled, err := still(dataDir, record.ID)
	if err != nil || !unsettled {
		return false, err
	}
	// Only this update's own copy goes back. Its helper, whose name carries
	// the operation, exists only once the copy was written, and the live
	// record says so just after; with neither, nothing was swapped, and a
	// copy there may be an earlier update's.
	if !helper && !record.Copy {
		return false, nil
	}
	if putBackReason(code) < 0 {
		// Cut off with no word, as by a reboot. This original exists, so
		// the new version wasn't accepted; until the update settles it
		// accepted no work and no changes, so stopping it and putting the
		// copy back loses nothing.
		err := client.Stop(ctx, servers.ReplacementName(record.Name, record.ID), nil)
		if err != nil && !docker.IsNotFound(err) && docker.StatusCode(err) != 304 {
			return false, errBeside
		}
		// Stopped, it can't settle any more, but may have just before.
		if _, unsettled, err = still(dataDir, record.ID); err != nil || !unsettled {
			return false, err
		}
	}
	return true, nil
}

// still reads the record in dataDir again and reports whether it is update
// id, still in progress.
func still(dataDir, id string) (Record, bool, error) {
	record, ok, err := saved(dataDir)
	return record, err == nil && ok && record.ID == id && record.State == StateUpdating, err
}

// alone confirms that no container of record's update but container runs:
// Docker shows each stopped or gone.
func alone(ctx context.Context, client *docker.Client, container string, record Record) error {
	for _, other := range []string{record.Container, servers.ReplacementName(record.Name, record.ID), record.Leftover} {
		if other == "" {
			continue
		}
		spec, err := client.InspectSpec(ctx, other)
		switch {
		case docker.IsNotFound(err):
		case err != nil:
			return errBeside
		case spec.ID != container && !strings.HasPrefix(spec.ID, container) && (spec.State.Running || spec.State.Restarting):
			return errBeside
		}
	}
	return nil
}

var errBeside = apperr.New("LUDOCK_UPDATE_CUT_OFF", 503,
	"Ludock’s update didn’t finish, and Docker didn’t confirm its other container stopped, so this one won’t start beside it.")

// saved is the update the database in dataDir records, read without opening
// it for writing. A missing or empty database, or one without the table, has
// none. When it can't be read and db.Open would refuse it, db.Open says why;
// any other failure to read it stops the start.
func saved(dataDir string) (Record, bool, error) {
	var record Record
	path := filepath.Join(dataDir, "ludock.db")
	if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) || err == nil && info.Size() == 0 {
		return record, false, nil
	}
	d, err := sql.Open("sqlite", db.FileURI(path, url.Values{"mode": {"ro"}}))
	if err != nil {
		return record, false, err
	}
	defer d.Close()
	var value []byte
	// A read can fail for a moment, as while another connection recovers
	// the WAL; it is tried again before it decides anything.
	for try := 0; ; try++ {
		var tables int
		err = d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'").Scan(&tables)
		if err == nil && tables == 0 {
			return record, false, nil
		} else if err == nil {
			err = d.QueryRow("SELECT value_json FROM settings WHERE key = ?", recordSetting).Scan(&value)
		}
		if err == nil || errors.Is(err, sql.ErrNoRows) || try == 10 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return record, false, nil
	case err != nil && refused(db.Inspect(path)):
		return record, false, nil
	case err != nil:
		return record, false, err
	}
	return record, true, json.Unmarshal(value, &record)
}

// refused reports whether db.Open refuses the database for good, as one a
// beta or a newer version wrote, or a file that isn't a database: not a busy
// or locked moment.
func refused(err error) bool {
	var failed *sqlite.Error
	if errors.As(err, &failed) {
		code := failed.Code() & 0xff
		return code != sqlite3.SQLITE_BUSY && code != sqlite3.SQLITE_LOCKED
	}
	return err != nil && !errors.Is(err, os.ErrNotExist)
}

// Unfinished is what to add to Ludock's refusal of data a newer version
// changed when its update's copy is still there: that update didn't finish.
func Unfinished(dataDir string) string {
	if _, err := os.Lstat(filepath.Join(dataDir, CopyName)); err != nil {
		return ""
	}
	return " An update of Ludock to a newer version didn’t finish. In the folder with its compose.yaml, run docker compose pull ludock, then docker compose up -d ludock."
}

// putDataBack renames the data copy over the database, after removing the
// newer version's journal files, so the folder always holds a database.
func putDataBack(dataDir string) error {
	copied := filepath.Join(dataDir, CopyName)
	if info, err := os.Lstat(copied); err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return os.ErrNotExist
	}
	for _, journal := range []string{"ludock.db-wal", "ludock.db-shm", "ludock.db-journal"} {
		if err := os.Remove(filepath.Join(dataDir, journal)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(copied, filepath.Join(dataDir, "ludock.db")); err != nil {
		return err
	}
	// Renamed, the copy is the database, whether or not the folder syncs.
	if dir, err := os.Open(dataDir); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
