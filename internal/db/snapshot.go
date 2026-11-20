package db

import (
	"archive/tar"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	uid, gid := os.Getuid(), os.Getgid()
	entries := []struct {
		header tar.Header
		body   io.Reader
	}{
		{tar.Header{Typeflag: tar.TypeReg, Name: base, Mode: 0o600, Size: info.Size()}, database},
		{tar.Header{Typeflag: tar.TypeDir, Name: keyFolder + "/", Mode: keyDirectoryMode}, nil},
		{tar.Header{Typeflag: tar.TypeReg, Name: keyFolder + "/" + keyFilename, Mode: keyFileMode, Size: int64(len(d.key))}, strings.NewReader(string(d.key))},
	}
	for _, entry := range entries {
		// USTAR keeps the archive readable by every tar, BusyBox's included.
		entry.header.Format, entry.header.ModTime, entry.header.Uid, entry.header.Gid = tar.FormatUSTAR, now, uid, gid
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
)

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
	// The key goes first: a database without its key never starts, so an
	// interruption between the two leaves nothing that looks restored.
	if err := os.Rename(filepath.Join(work, keyFolder), filepath.Join(folder, keyFolder)); err != nil {
		return savedAt, err
	}
	if err := os.Rename(filepath.Join(work, base), filepath.Join(folder, base)); err != nil {
		return savedAt, err
	}
	return savedAt, syncPath(folder)
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
	if err := AssertCompatible(bg, restored, BaseSchemaVersion, LatestVersion()); err != nil {
		if version, _ := pragmaInt(bg, restored, "user_version"); version > int64(LatestVersion()) {
			return errRestoreNewer
		}
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
