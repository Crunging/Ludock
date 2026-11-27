package selfupdate

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/launch"
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
// database in dataDir: when an update's helper put this container back, it
// renames the data copy over the database, so the version that wrote the
// copy is the one that restores it. So does an original started while its
// update is unfinished, with no word from the helper, as after a reboot. A
// helper still running after wait, or one whose end Docker can't say, leaves
// the data as it is and the copy beside it. It returns DataRestored or "",
// and an error when Ludock must not start: the new version still runs.
func BeforeOpen(ctx context.Context, client *docker.Client, container, dataDir string, wait time.Duration) (string, error) {
	// With no copy beside the database there is nothing to put back, so a
	// start doesn't wait for Docker.
	if _, err := os.Lstat(filepath.Join(dataDir, CopyName)); container == "" || errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	// Docker not answering is waited out as a running helper is.
	deadline := time.Now().Add(wait)
	pause := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
		return time.Now().Before(deadline)
	}
	var helpers []docker.ContainerSummary
	for {
		var err error
		if helpers, err = client.ListContainers(ctx, true, docker.Filters{"label": {launch.InternalLabel + "=" + helperKind}}); err == nil {
			break
		}
		if !pause() {
			return "", nil
		}
	}
	// The newest helper replacing this container is this update's; one an
	// earlier, interrupted update left says nothing about it.
	slices.SortFunc(helpers, func(a, b docker.ContainerSummary) int { return cmp.Compare(b.Created, a.Created) })
	for _, summary := range helpers {
		for {
			spec, err := client.InspectSpec(ctx, summary.ID)
			if docker.IsNotFound(err) || (err == nil && !ours(spec, container)) {
				break
			}
			if err == nil && !spec.State.Running && !spec.State.Restarting {
				code := spec.State.ExitCode
				if spec.State.Status != "exited" {
					code = -1
				}
				if code == ExitUpdated || code == ExitUnchanged {
					return "", nil
				}
				if putBackReason(code) < 0 {
					// Cut off with no word, as by a reboot. This original
					// exists, so the new version wasn't accepted; until the
					// update settles it accepted no work and no changes, so
					// stopping it and putting the copy back loses nothing.
					if !unsettled(dataDir, helperOp(spec)) {
						return "", nil
					}
					if err := stopNew(ctx, client, container, helperOp(spec)); err != nil {
						return "", err
					}
				}
				if err := putDataBack(dataDir); err != nil {
					log.Error("Ludock couldn't put back the copy of its data from before the update, so it starts on the newer data")
					return "", nil
				}
				log.Info("Ludock put back the copy of its data from before the update")
				return DataRestored, nil
			}
			if !pause() {
				return "", nil
			}
		}
	}
	return "", nil
}

// unsettled reports whether the database in dataDir still records update
// op as in progress, read without opening it for writing.
func unsettled(dataDir, op string) bool {
	d, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "ludock.db")+"?mode=ro")
	if err != nil {
		return false
	}
	defer d.Close()
	var state, id sql.NullString
	err = d.QueryRow("SELECT json_extract(value_json, '$.state'), json_extract(value_json, '$.id') FROM settings WHERE key = ?", recordSetting).Scan(&state, &id)
	return err == nil && state.String == StateUpdating && id.String == op
}

// stopNew stops update op's new container beside container, whatever state
// it is in, so two Ludocks never serve.
func stopNew(ctx context.Context, client *docker.Client, container, op string) error {
	own, err := client.InspectSpec(ctx, container)
	if err != nil {
		return err
	}
	err = client.Stop(ctx, servers.ReplacementName(strings.TrimPrefix(own.Name, "/"), op), nil)
	if err == nil || docker.IsNotFound(err) || docker.StatusCode(err) == 304 {
		return nil
	}
	return apperr.New("LUDOCK_UPDATE_CUT_OFF", 503, "Ludock’s update was cut off, and Docker didn’t stop its new version, so this one won’t start beside it.")
}

// Unfinished is what to add to Ludock's refusal of data a newer version
// changed when its update's copy is still there: that update didn't finish.
func Unfinished(dataDir string) string {
	if _, err := os.Lstat(filepath.Join(dataDir, CopyName)); err != nil {
		return ""
	}
	return " An update of Ludock to a newer version didn’t finish. In the folder with its compose.yaml, run docker compose pull ludock, then docker compose up -d ludock."
}

// ours reports whether a helper replaces container.
func ours(spec *docker.ContainerSpec, container string) bool {
	var config struct{ Cmd []string }
	return json.Unmarshal(encode(spec.Config), &config) == nil && len(config.Cmd) == 4 && config.Cmd[0] == "self-update" && config.Cmd[1] == container
}

// helperOp is the update a helper of ours runs.
func helperOp(spec *docker.ContainerSpec) string {
	var config struct{ Cmd []string }
	json.Unmarshal(encode(spec.Config), &config)
	return config.Cmd[3]
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
