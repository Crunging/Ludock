package selfupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
// "", and an error when this Ludock must not start: two would serve.
func BeforeOpen(ctx context.Context, client *docker.Client, container, dataDir string, wait time.Duration) (string, error) {
	if container == "" {
		return "", nil
	}
	record, ok, err := saved(dataDir)
	if err != nil || !ok {
		return "", err
	}
	restore, err := decide(ctx, client, container, record, wait)
	if err != nil {
		return "", err
	}
	// No Ludock opens the data, or puts the copy back, while another
	// container of an update that didn't finish may serve on it: every way
	// to start passes here, and anything but Docker showing the others
	// stopped or gone refuses, for Docker to start this one again later.
	if record.State == StateUpdating || record.State == StateInterrupted {
		if err := alone(ctx, client, container, record); err != nil {
			return "", err
		}
	}
	if !restore {
		return "", nil
	}
	if err := putDataBack(dataDir); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Ludock couldn't put back the copy of its data from before the update, so it starts on the newer data")
		}
		return "", nil
	}
	log.Info("Ludock put back the copy of its data from before the update")
	return DataRestored, nil
}

// decide reports whether container, starting, puts the update's copy back.
// It doesn't open or restore anything, so BeforeOpen's check covers it.
func decide(ctx context.Context, client *docker.Client, container string, record Record, wait time.Duration) (bool, error) {
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
	}
	return true, nil
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
	var tables int
	err = d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'").Scan(&tables)
	var value []byte
	if err == nil && tables == 0 {
		return record, false, nil
	} else if err == nil {
		err = d.QueryRow("SELECT value_json FROM settings WHERE key = ?", recordSetting).Scan(&value)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return record, false, nil
	case err != nil && db.Inspect(path) != nil:
		return record, false, nil
	case err != nil:
		return record, false, err
	}
	return record, true, json.Unmarshal(value, &record)
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
