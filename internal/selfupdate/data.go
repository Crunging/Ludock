package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/launch"
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
// copy is the one that restores it. A helper still running after wait, or
// one whose end Docker can't say, leaves the data as it is and the copy
// beside it. It returns DataRestored, DataKept, or "".
func BeforeOpen(ctx context.Context, client *docker.Client, container, dataDir string, wait time.Duration) string {
	if container == "" {
		return ""
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
			return ""
		}
	}
	for _, summary := range helpers {
		for {
			spec, err := client.InspectSpec(ctx, summary.ID)
			if docker.IsNotFound(err) || (err == nil && !ours(spec, container)) {
				break
			}
			if err == nil && !spec.State.Running && !spec.State.Restarting {
				if spec.State.ExitCode < ExitPutBack || spec.State.ExitCode >= ExitPutBack+len(Reasons) {
					return ""
				}
				if err := putDataBack(dataDir); errors.Is(err, os.ErrNotExist) {
					return DataKept
				} else if err != nil {
					log.Error("Ludock couldn't put back the copy of its data from before the update, so it starts on the newer data")
					return ""
				}
				log.Info("Ludock put back the copy of its data from before the update")
				return DataRestored
			}
			if !pause() {
				return ""
			}
		}
	}
	return ""
}

// ours reports whether a helper replaces container.
func ours(spec *docker.ContainerSpec, container string) bool {
	var config struct{ Cmd []string }
	return json.Unmarshal(encode(spec.Config), &config) == nil && len(config.Cmd) == 4 && config.Cmd[0] == "self-update" && config.Cmd[1] == container
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
