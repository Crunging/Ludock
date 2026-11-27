package selfupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
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
	record, ok := saved(dataDir)
	if container == "" || !ok || record.Container != container {
		return "", nil
	}
	// The new version took over when the update stopped partway, and runs.
	if record.State == StateInterrupted && record.OnLeftover {
		if spec, err := client.InspectSpec(ctx, record.Leftover); err == nil && spec.State.Running {
			return "", errBeside
		}
		return "", nil
	}
	if record.State != StateUpdating {
		return "", nil
	}
	// Docker not answering is waited out as a running helper is; after
	// that this original doesn't start, and its restart policy retries it.
	deadline := time.Now().Add(wait)
	code := -1
	for {
		spec, err := client.InspectSpec(ctx, HelperName(record.ID))
		if docker.IsNotFound(err) {
			break
		}
		if err == nil && !spec.State.Running && !spec.State.Restarting {
			if spec.State.Status == "exited" {
				code = spec.State.ExitCode
			}
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
		if time.Now().After(deadline) {
			return "", errBeside
		}
	}
	if code == ExitUpdated || code == ExitUnchanged {
		return "", nil
	}
	if putBackReason(code) < 0 {
		// Cut off with no word, as by a reboot. This original exists, so
		// the new version wasn't accepted; until the update settles it
		// accepted no work and no changes, so stopping it and putting the
		// copy back loses nothing.
		own, err := client.InspectSpec(ctx, container)
		if err == nil {
			err = client.Stop(ctx, servers.ReplacementName(strings.TrimPrefix(own.Name, "/"), record.ID), nil)
		}
		if err != nil && !docker.IsNotFound(err) && docker.StatusCode(err) != 304 {
			return "", errBeside
		}
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

var errBeside = apperr.New("LUDOCK_UPDATE_CUT_OFF", 503,
	"Ludock’s update didn’t finish, and Docker didn’t confirm its new version stopped, so this one won’t start beside it.")

// saved is the update the database in dataDir records, read without opening
// it for writing.
func saved(dataDir string) (Record, bool) {
	var record Record
	d, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ludock.db")+"?mode=ro")
	if err != nil {
		return record, false
	}
	defer d.Close()
	var value []byte
	if d.QueryRow("SELECT value_json FROM settings WHERE key = ?", recordSetting).Scan(&value) != nil || json.Unmarshal(value, &record) != nil {
		return record, false
	}
	return record, true
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
