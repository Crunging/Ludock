package selfupdate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/Crunging/Ludock/internal/identity"
)

// The data steps of putting the previous version back. Each runs in a
// one-off container from the new image with Ludock's data mount, between
// the old image's `ludock restore-data`, which refuses a folder that already
// holds data:
//
//	self-update-data settled <operation>           exit 3 once the restore is settled
//	self-update-data aside <operation>             move the live data aside
//	self-update-data back <operation>              move it back, when the restore failed
//	self-update-data mark <operation> <outcome>    record that this is settled
//
// Each step is safe to repeat. They run only from the helper of the same
// image, so these arguments are not part of the frozen interface.

// dataFiles are what the data folder holds of Ludock's, the database last:
// its presence in the aside folder means everything was moved.
var dataFiles = []string{"ludock.db-wal", "ludock.db-shm", "ludock.db-journal", "ludock.db.identity-key", "ludock.db"}

// Exit codes of the data steps besides 0 and 1.
const dataSettled = 3

// DataMain runs a data step in dataDir and returns its exit code.
func DataMain(args []string, dataDir string, stderr io.Writer) int {
	if len(args) < 2 || !identity.ValidID(args[1]) {
		fmt.Fprintln(stderr, "Usage: ludock self-update-data aside|back|mark <operation> [outcome]")
		return 2
	}
	aside := filepath.Join(dataDir, asideName(args[1]))
	// Only a settled entry this step wrote counts; anything else under its
	// name is refused rather than taken for settled.
	settled := func() (bool, error) {
		switch readSettled(dataDir, args[1]) {
		case "restored", "kept":
			return true, nil
		}
		if _, err := os.Lstat(filepath.Join(aside, "settled")); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("the settled entry is unreadable")
		}
		return false, nil
	}
	var err error
	switch {
	case (args[0] == "aside" || args[0] == "settled") && len(args) == 2:
		done, settledErr := settled()
		if done {
			return dataSettled
		}
		if err = settledErr; err == nil && args[0] == "aside" {
			err = moveAside(dataDir, aside)
		}
	case args[0] == "back" && len(args) == 2:
		err = moveBack(dataDir, aside)
	case args[0] == "mark" && len(args) == 3 && (args[2] == "restored" || args[2] == "kept"):
		if err = os.MkdirAll(aside, 0o700); err == nil {
			err = writeFile(filepath.Join(aside, "settled"), []byte(args[2]+"\n"))
		}
	default:
		fmt.Fprintln(stderr, "Usage: ludock self-update-data aside|back|mark <operation> [outcome]")
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "Could not move Ludock's data:", err)
		return 1
	}
	return 0
}

// moveAside moves the live data into aside/newer. Once the database is
// there everything was moved, so what is live is an interrupted restore's,
// which the save can make again.
func moveAside(dataDir, aside string) error {
	newer := filepath.Join(aside, "newer")
	if err := os.MkdirAll(newer, 0o700); err != nil {
		return err
	}
	// Each new entry is synced in the folder that holds it.
	for _, folder := range []string{aside, dataDir} {
		if err := syncDir(folder); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(filepath.Join(newer, "ludock.db")); err == nil {
		for _, name := range dataFiles {
			if err := os.RemoveAll(filepath.Join(dataDir, name)); err != nil {
				return err
			}
		}
		return syncDir(dataDir)
	}
	for _, name := range dataFiles {
		live := filepath.Join(dataDir, name)
		if _, err := os.Lstat(live); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(newer, name)); err == nil {
			return fmt.Errorf("%s is both live and aside", name)
		}
		if err := os.Rename(live, filepath.Join(newer, name)); err != nil {
			return err
		}
	}
	if err := syncDir(newer); err != nil {
		return err
	}
	return syncDir(dataDir)
}

// moveBack returns the data moveAside moved, replacing what a failed
// restore left.
func moveBack(dataDir, aside string) error {
	newer := filepath.Join(aside, "newer")
	for _, name := range slices.Backward(dataFiles) {
		kept := filepath.Join(newer, name)
		if _, err := os.Lstat(kept); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, name)); err != nil {
			return err
		}
		if err := os.Rename(kept, filepath.Join(dataDir, name)); err != nil {
			return err
		}
	}
	return syncDir(dataDir)
}

func writeFile(path string, content []byte) error {
	partial := path + ".partial"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	folder, err := os.Open(path)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}
