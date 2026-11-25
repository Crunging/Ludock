package db

import (
	"archive/tar"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
)

// A saved copy of Ludock's data is a tar archive of exactly the files in the
// data folder that Ludock needs: the database and its identity key folder,
// named as they are there, so `tar -xpf` into an empty data folder restores
// it.

// snapshotPrefix names the folders a snapshot is built in, inside the data
// folder; a leftover one is from a save interrupted by a restart.
const snapshotPrefix = ".ludock-save-"

// Snapshot is one consistent copy of the database and its key as a tar
// archive in a file nobody else can open: it is already unlinked.
type Snapshot struct {
	*os.File
	Size     int64
	Checksum string
}

// processIDs are the user and group IDs a save records.
var processIDs = func() (int, int) { return os.Getuid(), os.Getgid() }

// Snapshot copies the database through SQLite's VACUUM INTO, which reads one
// transaction's view while writers carry on, and archives it with the key.
func (d *DB) Snapshot(ctx context.Context) (*Snapshot, error) {
	if d.path == ":memory:" {
		return nil, errors.New("an in-memory database has no data folder")
	}
	folder := filepath.Dir(d.path)
	removeSnapshotLeftovers(folder)
	var suffix [8]byte
	rand.Read(suffix[:])
	work := filepath.Join(folder, snapshotPrefix+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(work, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	copied := filepath.Join(work, "copy.db")
	if _, err := d.ExecContext(ctx, "VACUUM INTO ?", copied); err != nil {
		return nil, err
	}
	if err := settleOperations(ctx, copied); err != nil {
		return nil, err
	}
	database, err := os.Open(copied)
	if err != nil {
		return nil, err
	}
	defer database.Close()
	info, err := database.Stat()
	if err != nil {
		return nil, err
	}
	archive, err := os.OpenFile(filepath.Join(work, "save.tar"), os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	counter := &countingWriter{}
	writer := tar.NewWriter(io.MultiWriter(archive, digest, counter))
	base := filepath.Base(d.path)
	keyFolder := base + ".identity-key"
	now := time.Now().Truncate(time.Second)
	uid, gid := processIDs()
	// USTAR keeps the archive readable by every tar, BusyBox's included. Its
	// IDs end at 7 octal digits; some NAS directory services give larger
	// ones, which only PAX holds.
	format := tar.FormatUSTAR
	if max(uid, gid) >= 1<<21 {
		format = tar.FormatPAX
	}
	entries := []struct {
		header tar.Header
		body   io.Reader
	}{
		{tar.Header{Typeflag: tar.TypeReg, Name: base, Mode: 0o600, Size: info.Size()}, database},
		{tar.Header{Typeflag: tar.TypeDir, Name: keyFolder + "/", Mode: keyDirectoryMode}, nil},
		{tar.Header{Typeflag: tar.TypeReg, Name: keyFolder + "/" + keyFilename, Mode: keyFileMode, Size: int64(len(d.key))}, strings.NewReader(string(d.key))},
	}
	for _, entry := range entries {
		entry.header.Format, entry.header.ModTime, entry.header.Uid, entry.header.Gid = format, now, uid, gid
		if err := writer.WriteHeader(&entry.header); err != nil {
			archive.Close()
			return nil, err
		}
		if entry.body != nil {
			if written, err := io.Copy(writer, entry.body); err != nil || written != entry.header.Size {
				archive.Close()
				return nil, cmp.Or(err, errors.New("database copy changed while archived"))
			}
		}
	}
	if err := writer.Close(); err != nil {
		archive.Close()
		return nil, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		archive.Close()
		return nil, err
	}
	return &Snapshot{File: archive, Size: counter.n, Checksum: hex.EncodeToString(digest.Sum(nil))}, nil
}

// GuidancePath is the JSON path of ops.Result's guidance, which this package
// sits below.
const GuidancePath = "$.guidance"

// settleOperations ends the copy's queued and running operations: restored
// later, they would stop servers long after they were asked for, or recover
// work whose result the copy never saw, such as an archive recorded since.
// A restore's journal is dropped unless it is final: by the time the copy is
// restored, the restore may have gone on, so undoing it from this copy could
// remove the data it left in place. The copy also leaves WAL mode, so the
// database is one file.
func settleOperations(ctx context.Context, path string) error {
	conn, err := sql.Open("sqlite", fileURI(path, nil))
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	if _, err := conn.ExecContext(ctx, "PRAGMA journal_mode = DELETE"); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `UPDATE operations SET status='interrupted', phase='interrupted', updated_at=?,
    error='Ludock’s data was saved while this was waiting or running, and it wasn’t run again after the data was restored.'
    WHERE status IN ('queued','running')`, Now())
	if err != nil {
		return err
	}
	// The guidance names the restore's stage folder (helper.StageName).
	_, err = conn.ExecContext(ctx, `UPDATE operations SET recovery_json=json_set(json_remove(recovery_json,'$.dataUnsafe'),'$.restore',json('{"status":"unknown"}')),
    result_json=json_set(CASE WHEN json_valid(result_json) THEN result_json ELSE '{}' END,'`+GuidancePath+`',?||id||?)
    WHERE kind='restore' AND json_valid(recovery_json) AND json_extract(recovery_json,'$.restore.status') IN ('staging','replacing','rolling_back','committed')`,
		"Ludock’s data was saved before this restore finished, so this copy doesn’t know how it ended, and Ludock won’t change the server’s data for it. "+
			"If one of the server’s data folders still has a folder named .ludock-restore-",
		", it holds what the restore was moving: the server’s previous data in its old folder and the backup’s in new. "+
			"New backups leave that folder out, so they don’t keep that previous data: copy out or put back what you need, then delete the folder on the host.")
	return err
}

// IsOwnSave reports whether archive is a saved copy of this installation's
// data: one holding its identity key.
func (d *DB) IsOwnSave(archive io.Reader) bool {
	name := filepath.Base(d.path) + ".identity-key/" + keyFilename
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if err != nil {
			return false
		}
		if header.Name == name && header.Typeflag == tar.TypeReg && header.Size == keyBytes {
			key := make([]byte, keyBytes)
			defer clear(key)
			_, err := io.ReadFull(reader, key)
			return err == nil && subtle.ConstantTimeCompare(key, d.key) == 1
		}
	}
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// removeSnapshotLeftovers removes the work folders of interrupted saves.
// Saves run one at a time, so none of them is in use.
func removeSnapshotLeftovers(folder string) {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), snapshotPrefix) && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			os.RemoveAll(filepath.Join(folder, entry.Name()))
		}
	}
}

// snapshotLimit bounds what a restore extracts: no real database is larger.
const snapshotLimit = 64 << 30

var (
	errRestoreInUse = apperr.New("RESTORE_DATA_EXISTS", 409,
		"The data folder already has Ludock data. Stop Ludock and move ludock.db and ludock.db.identity-key out of the data folder, then restore again.")
	errRestoreArchive = apperr.New("RESTORE_ARCHIVE", 400,
		"This file isn’t a saved copy of Ludock’s data, or it is damaged. Choose a ludock-data-….tar file from the backup folder or the second folder.")
	errRestoreNewer = apperr.New("RESTORE_NEWER", 400, "This copy was saved by a newer version of Ludock. Restore it with that version or a later one.")
	errRestoreBeta  = apperr.New("RESTORE_BETA", 400, "This copy was saved by a Ludock beta, and Ludock 1.0 starts fresh, so it can’t restore it. Start Ludock with the empty data folder and set up the first administrator. Nothing was changed: the copy, your game servers, and your backup archives are as they were.")
)

// RestoreExitCode is `ludock restore-data`'s exit code for a failed
// restore, which an update's helper of a later version reads (selfupdate's
// RestoreFinal and RestoreExists): 3 for a copy this version can't restore,
// 4 for a data folder that already holds data, 1 for any other error.
func RestoreExitCode(err error) int {
	switch {
	case errors.Is(err, errRestoreArchive), errors.Is(err, errRestoreNewer), errors.Is(err, errRestoreBeta):
		return 3
	case errors.Is(err, errRestoreInUse):
		return 4
	}
	return 1
}

// RestoreSnapshot restores a saved copy into the data folder of dbPath, which
// must not hold a database or key yet. The archive is checked first: only
// the database and its key, a database this version can open, and a key
// that matches it. Nothing is placed until all of that holds.
func RestoreSnapshot(archivePath, dbPath string) (savedAt time.Time, err error) {
	folder := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	keyFolder := base + ".identity-key"
	for _, name := range []string{base, keyFolder, base + "-wal", base + "-journal"} {
		if _, err := os.Lstat(filepath.Join(folder, name)); !errors.Is(err, os.ErrNotExist) {
			return savedAt, errRestoreInUse
		}
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return savedAt, err
	}
	removeSnapshotLeftovers(folder)
	var suffix [8]byte
	rand.Read(suffix[:])
	work := filepath.Join(folder, snapshotPrefix+hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(work, 0o700); err != nil {
		return savedAt, err
	}
	defer os.RemoveAll(work)
	source, err := os.Open(archivePath)
	if err != nil {
		return savedAt, err
	}
	defer source.Close()
	key, err := extractSnapshot(source, work, base, keyFolder, &savedAt)
	defer clear(key)
	if err != nil {
		return savedAt, err
	}
	if err := checkRestored(filepath.Join(work, base), key); err != nil {
		return savedAt, err
	}
	// The database goes first: without its key it never starts, so an
	// interruption between the two can't pass for a new installation.
	if err := placeRestored(work, folder, base); err != nil {
		return savedAt, err
	}
	if err := placeRestored(work, folder, keyFolder); err != nil {
		os.Remove(filepath.Join(folder, base))
		return savedAt, err
	}
	return savedAt, syncPath(folder)
}

// placeRestored moves name from work into folder, never replacing anything
// created there since the restore checked. The caller syncs the folder.
func placeRestored(work, folder, name string) error {
	source, target := filepath.Join(work, name), filepath.Join(folder, name)
	err := unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		// Some filesystems lack RENAME_NOREPLACE.
		if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
			return errRestoreInUse
		}
		err = os.Rename(source, target)
	}
	if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
		return errRestoreInUse
	}
	return err
}

// extractSnapshot writes the archive's database and key into work and
// returns the key. Any other entry, or a missing one, rejects the archive.
func extractSnapshot(source io.Reader, work, base, keyFolder string, savedAt *time.Time) ([]byte, error) {
	reader := tar.NewReader(source)
	var key []byte
	sawDatabase, sawFolder := false, false
	if err := os.Mkdir(filepath.Join(work, keyFolder), keyDirectoryMode); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(work, keyFolder), keyDirectoryMode); err != nil {
		return nil, err
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return key, errRestoreArchive
		}
		switch {
		case header.Name == base && header.Typeflag == tar.TypeReg && !sawDatabase && header.Size <= snapshotLimit:
			sawDatabase = true
			*savedAt = header.ModTime
			if err := writeRestored(filepath.Join(work, base), reader, header.Size, 0o600); err != nil {
				return key, err
			}
		case strings.TrimSuffix(header.Name, "/") == keyFolder && header.Typeflag == tar.TypeDir && !sawFolder:
			sawFolder = true
		case header.Name == keyFolder+"/"+keyFilename && header.Typeflag == tar.TypeReg && key == nil && header.Size == keyBytes:
			key = make([]byte, keyBytes)
			if _, err := io.ReadFull(reader, key); err != nil {
				return key, errRestoreArchive
			}
			if err := writeRestored(filepath.Join(work, keyFolder, keyFilename), strings.NewReader(string(key)), keyBytes, keyFileMode); err != nil {
				return key, err
			}
		default:
			return key, errRestoreArchive
		}
	}
	if !sawDatabase || key == nil {
		return key, errRestoreArchive
	}
	return key, syncPath(filepath.Join(work, keyFolder))
}

func writeRestored(path string, body io.Reader, size int64, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if written, err := io.Copy(file, body); err != nil || written != size {
		return errRestoreArchive
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	return file.Sync()
}

func syncPath(path string) error {
	folder, err := os.Open(path)
	if err != nil {
		return err
	}
	defer folder.Close()
	return folder.Sync()
}

// checkRestored opens the extracted database without writing to it and
// checks that this version can open it, that it is intact, and that key is
// its key.
func checkRestored(path string, key []byte) error {
	restored, err := sql.Open("sqlite", fileURI(path, url.Values{"mode": {"ro"}, "immutable": {"1"}}))
	if err != nil {
		return err
	}
	defer restored.Close()
	switch err := AssertCompatible(bg, restored, LatestVersion()); err {
	case nil:
	case ErrNewerDatabase:
		return errRestoreNewer
	case ErrBetaDatabase:
		return errRestoreBeta
	default:
		return errRestoreArchive
	}
	var result string
	if err := restored.QueryRowContext(bg, "PRAGMA quick_check").Scan(&result); err != nil || result != "ok" {
		return errRestoreArchive
	}
	if err := assertKey(restored, key); err != nil {
		if errors.Is(err, ErrIdentityKey) {
			return errRestoreArchive
		}
		return err
	}
	return nil
}
