package db

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/logging"
)

const (
	keyBytes         = 32
	keyDirectoryMode = 0o700
	keyFileMode      = 0o600
	keyCheckSetting  = "identity.key-check"
	keyFilename      = "key"
)

var keyLog = logging.New("identity-key")

// ErrIdentityKey is the generic key failure. It never names host paths.
var ErrIdentityKey = apperr.New("IDENTITY_KEY_INVALID", 500, "Ludock's identity key is missing, unsafe, or does not match this database. Restore the matching .identity-key directory beside the database from your application-data backup; do not generate a replacement key.")

// keyPermissionCode marks an error that says exactly what to fix, so it must
// not be replaced by the generic error.
const keyPermissionCode = "IDENTITY_KEY_PERMISSIONS"

// testing hooks for interrupted writes.
var (
	writeKeyFile = func(f *os.File, key []byte) error { _, err := f.Write(key); return err }
	syncFile     = func(f *os.File) error { return f.Sync() }
)

func ownedByProcess(info *syscall.Stat_t) bool { return int(info.Uid) == os.Getuid() }

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// checkKeyMode rejects owner or other bits that differ from the expected
// mode. Extra group bits, which also reflect a POSIX ACL mask, only produce a
// warning.
func checkKeyMode(target string, isDir bool, mode, expected uint32, warn bool) error {
	mode &= 0o777
	if mode == expected {
		return nil
	}
	kind := "file"
	// Name the key relative to the database's directory so host paths stay out
	// of logs. Clear ACL entries first: chmod alone leaves a base group entry
	// that setfacl -b would make effective again.
	name := filepath.Join(filepath.Base(filepath.Dir(target)), filepath.Base(target))
	if isDir {
		kind = "directory"
		name = filepath.Base(target)
	}
	fix := fmt.Sprintf("in the database's directory, run setfacl -b %s && chmod %o %s", shellQuote(name), expected, shellQuote(name))
	if mode&0o707 != expected {
		return apperr.Newf(keyPermissionCode, 500,
			"Ludock's identity key %s %s has unsafe permissions: expected %04o, found %04o. To fix it, %s",
			kind, name, expected, mode, fix)
	}
	if warn {
		keyLog.Warn(fmt.Sprintf(
			"Ludock's identity key %s %s grants extra access through group bits or a POSIX ACL: expected %04o, found %04o. Starting anyway; to fix it, %s",
			kind, name, expected, mode, fix))
	}
	return nil
}

func openNoFollow(path string, flags int) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

func fstat(f *os.File) (*syscall.Stat_t, error) {
	var info syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func openKeyDirectory(directory string, warn bool) (*os.File, error) {
	f, err := openNoFollow(directory, os.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	info, err := fstat(f)
	if err == nil && (info.Mode&syscall.S_IFMT != syscall.S_IFDIR || !ownedByProcess(info)) {
		err = ErrIdentityKey
	}
	if err == nil {
		err = checkKeyMode(directory, true, info.Mode, keyDirectoryMode, warn)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func syncParentDirectory(directory string) error {
	parent, err := openNoFollow(filepath.Dir(directory), os.O_RDONLY|syscall.O_DIRECTORY)
	if err != nil {
		return err
	}
	defer parent.Close()
	return syncFile(parent)
}

func readPublishedKey(directory string) (key []byte, err error) {
	defer func() {
		if err != nil {
			clear(key)
			key = nil
			if !apperr.Is(err, keyPermissionCode) {
				err = ErrIdentityKey
			}
		}
	}()
	dir, err := openKeyDirectory(directory, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	keyPath := filepath.Join(directory, keyFilename)
	file, err := openNoFollow(keyPath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := fstat(file)
	if err != nil {
		return nil, err
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFREG || info.Nlink != 1 || info.Size != keyBytes || !ownedByProcess(info) {
		return nil, ErrIdentityKey
	}
	if err := checkKeyMode(keyPath, false, info.Mode, keyFileMode, true); err != nil {
		return nil, err
	}
	key = make([]byte, keyBytes+1)
	n, err := io.ReadFull(file, key)
	if err != io.ErrUnexpectedEOF || n != keyBytes {
		return key, ErrIdentityKey
	}
	key = key[:keyBytes]
	// A racing creator may have published the complete directory but not yet
	// synced its new parent entry. Make the winner durable before SQLite can
	// commit a database that depends on it.
	if err := syncFile(dir); err != nil {
		return key, err
	}
	if err := syncParentDirectory(directory); err != nil {
		return key, err
	}
	return key, nil
}

func discardStagingDirectory(directory string) {
	_ = os.Remove(filepath.Join(directory, keyFilename))
	// Crash-only staging directories are ignored on later startups.
	_ = os.Remove(directory)
}

func publishKeyDirectory(directory string, key []byte) (err error) {
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	staging := fmt.Sprintf("%s.stage-%d-%s", directory, os.Getpid(), hex.EncodeToString(suffix[:]))
	if err := os.Mkdir(staging, keyDirectoryMode); err != nil {
		return err
	}
	// Mkdir is subject to the umask; the key check wants exactly 0700.
	if err := os.Chmod(staging, keyDirectoryMode); err != nil {
		os.Remove(staging)
		return err
	}
	published := false
	defer func() {
		if !published {
			discardStagingDirectory(staging)
		}
	}()
	file, err := os.OpenFile(filepath.Join(staging, keyFilename),
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, keyFileMode)
	if err != nil {
		return err
	}
	err = writeKeyFile(file, key)
	if err == nil {
		err = file.Chmod(keyFileMode)
	}
	if err == nil {
		err = syncFile(file)
	}
	file.Close()
	if err != nil {
		return err
	}
	// The published directory is checked again and warns under its real name.
	stagingDir, err := openKeyDirectory(staging, false)
	if err != nil {
		return err
	}
	err = syncFile(stagingDir)
	stagingDir.Close()
	if err != nil {
		return err
	}
	// The source is already nonempty, and a legitimate destination is also
	// nonempty, so POSIX rename cannot replace another creator's winner.
	if err := os.Rename(staging, directory); err != nil {
		if _, statErr := os.Lstat(directory); statErr != nil {
			return ErrIdentityKey
		}
		return nil
	}
	published = true
	return syncParentDirectory(directory)
}

// LoadKey reads or creates the installation key beside the database. The key
// is separate from SQLite so a database-only disclosure cannot be used to
// guess low-entropy secrets included in persisted fingerprints.
func LoadKey(dbPath string, required bool) ([]byte, error) {
	if dbPath == ":memory:" {
		key := make([]byte, keyBytes)
		_, err := rand.Read(key)
		return key, err
	}
	// Database aliases share the same installation key.
	real, err := filepath.EvalSymlinks(dbPath)
	if err != nil {
		return nil, ErrIdentityKey
	}
	real, _ = filepath.Abs(real)
	keyDirectory := real + ".identity-key"
	info, err := os.Lstat(keyDirectory)
	if err == nil {
		if !info.IsDir() {
			return nil, ErrIdentityKey
		}
		return readPublishedKey(keyDirectory)
	}
	if !errors.Is(err, os.ErrNotExist) || required {
		return nil, ErrIdentityKey
	}
	generated := make([]byte, keyBytes)
	defer clear(generated)
	if _, err := rand.Read(generated); err != nil {
		return nil, err
	}
	if err := publishKeyDirectory(keyDirectory, generated); err != nil {
		if apperr.Is(err, keyPermissionCode) {
			return nil, err
		}
		return nil, ErrIdentityKey
	}
	// Reopen and validate the published key, including after a creator race.
	return LoadKey(dbPath, true)
}

func keyedHex(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func keyCheck(key []byte) string { return keyedHex(key, "ludock:identity-key:v2") }

func initializeKeyCheck(tx *sql.Tx, key []byte) error {
	if key == nil {
		return errors.New("identity key is required to initialize the database")
	}
	value, _ := json.Marshal(keyCheck(key))
	_, err := tx.Exec("INSERT INTO settings(key,value_json) VALUES (?,?)", keyCheckSetting, string(value))
	return err
}

func assertKey(q queryer, key []byte) error {
	var actual string
	err := q.QueryRowContext(bg, "SELECT value_json FROM settings WHERE key=?", keyCheckSetting).Scan(&actual)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	expected, _ := json.Marshal(keyCheck(key))
	if subtle.ConstantTimeCompare([]byte(actual), expected) != 1 {
		return ErrIdentityKey
	}
	return nil
}

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func protectDigest(digest string, key []byte, domain string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", errors.New("invalid fingerprint digest")
	}
	return "hmac-sha256:" + keyedHex(key, "ludock:"+domain+":"+digest), nil
}
