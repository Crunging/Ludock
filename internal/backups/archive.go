package backups

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/tarstream"
	"github.com/Crunging/Ludock/internal/textutil"
)

// EntryMetadata returns validated ownership and modification time.
func EntryMetadata(header *tarstream.Header) (uid, gid int64, mtime float64, err error) {
	uid, gid, mtime = header.UID, header.GID, header.MtimeSeconds
	if header.PAX != nil {
		var parseErr error
		parse := func(key string, target *float64) {
			if value, ok := header.PAX[key]; ok {
				if *target, parseErr = parseNumber(value); parseErr != nil {
					err = parseErr
				}
			}
		}
		u, g, m := float64(uid), float64(gid), mtime
		parse("uid", &u)
		parse("gid", &g)
		parse("mtime", &m)
		if u != math.Trunc(u) || g != math.Trunc(g) {
			err = errors.New("fractional owner")
		}
		uid, gid, mtime = int64(u), int64(g), m
	}
	if err != nil || uid < 0 || uid > 4294967294 || gid < 0 || gid > 4294967294 || math.IsNaN(mtime) || math.Abs(mtime) > 8.64e12 {
		return 0, 0, 0, fail("UNSAFE_ARCHIVE", "The archive contains invalid ownership or timestamp metadata.")
	}
	return uid, gid, mtime, nil
}

func parseNumber(value string) (float64, error) {
	var number float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &number); err != nil {
		return 0, err
	}
	return number, nil
}

// MappedHeader renames an entry with fresh, numeric-only PAX metadata. This
// keeps large Linux IDs and dates past 2038 without retaining source path or
// link overrides.
func MappedHeader(header *tarstream.Header, name string) (*tarstream.Header, error) {
	uid, gid, mtime, err := EntryMetadata(header)
	if err != nil {
		return nil, err
	}
	return &tarstream.Header{Name: name, Type: header.Type, Size: header.Size, Mode: header.Mode, UID: uid, GID: gid,
		MtimeSeconds: mtime, PAX: map[string]string{"uid": tarstream.FormatSeconds(float64(uid)),
			"gid": tarstream.FormatSeconds(float64(gid)), "mtime": tarstream.FormatSeconds(mtime)}}, nil
}

// ValidateEntry accepts only regular files and directories inside the
// recorded roots.
func ValidateEntry(header *tarstream.Header, roots []Root) error {
	name := strings.TrimSuffix(header.Name, "/")
	parts := strings.Split(name, "/")
	if _, _, _, err := EntryMetadata(header); err != nil {
		return err
	}
	if strings.Contains(header.Name, "\x00") || strings.Contains(header.Name, "\\") || strings.HasPrefix(header.Name, "/") ||
		slices.ContainsFunc(parts, func(part string) bool { return part == ".." || part == "." || part == "" }) ||
		len(parts) > 64 || textutil.Length(name) > 4096 {
		return fail("UNSAFE_ARCHIVE", "The archive contains an unsafe path.")
	}
	if name != "snapshot" && (!strings.HasPrefix(name, "snapshot/") || !slices.ContainsFunc(roots, func(root Root) bool { return parts[1] == root.ID })) {
		return fail("UNSAFE_ARCHIVE", "The archive contains a path outside its recorded roots.")
	}
	if header.Type != "file" && header.Type != "directory" {
		return fail("UNSAFE_ARCHIVE", "Backups support regular files and directories. Remove symbolic links, hard links, and special files from the selected data roots before retrying.")
	}
	if slices.ContainsFunc(parts, func(part string) bool { return strings.HasPrefix(part, ".ludock-restore-") }) {
		return fail("RESTORE_RECOVERY_REQUIRED", "A previous restore has staging data in this root. Complete its recovery before making a backup.")
	}
	if header.Size < 0 || header.Size > 1<<53-1 {
		return fail("UNSAFE_ARCHIVE", "The archive has an invalid entry size.")
	}
	if (name == "snapshot" || len(parts) == 2) && header.Type != "directory" {
		return fail("BACKUP_ROOT_NOT_DIRECTORY", "Backup roots must be mounted directories; individual file mounts are not supported.")
	}
	return nil
}

var errCapacity = fail("BACKUP_CAPACITY", "The archive exceeds the configured size limit.")

// ValidateStream checks a whole archive and every recorded root.
func ValidateStream(input io.Reader, roots []Root, maxBytes int64, onChunk func([]byte)) (map[string]int64, error) {
	names := map[string]bool{}
	found := map[string]bool{}
	// staged estimates each root's extracted size in 4 KiB filesystem
	// blocks, at least one per entry, so a restore that cannot fit is
	// refused before the server stops. Extraction enforces the reserve.
	staged := map[string]int64{}
	var total, read int64
	err := tarstream.Walk(input, func(header *tarstream.Header, _ io.Reader) error {
		if err := ValidateEntry(header, roots); err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if names[name] || len(names) >= 100000 {
			return fail("UNSAFE_ARCHIVE", "The archive has duplicate paths or too many entries.")
		}
		names[name] = true
		total += header.Size
		if total > maxBytes {
			return errCapacity
		}
		parts := strings.Split(name, "/")
		if len(parts) == 2 {
			found[parts[1]] = true
		}
		if len(parts) >= 2 {
			staged[parts[1]] += max(4096, (header.Size+4095)/4096*4096)
		}
		return nil
	}, func(chunk []byte) error {
		read += int64(len(chunk))
		if read > maxBytes {
			return errCapacity
		}
		if onChunk != nil {
			onChunk(chunk)
		}
		return nil
	})
	if err != nil {
		if _, ok := apperr.As(err); ok {
			return nil, err
		}
		return nil, fail("INVALID_BACKUP", "The backup archive is invalid or truncated.")
	}
	for _, root := range roots {
		if !found[root.ID] {
			return nil, fail("INVALID_BACKUP", "The archive is missing a recorded data root.")
		}
	}
	return staged, nil
}

// Validate checks a published archive and its checksum, and returns the
// bytes each root's restore stages.
func (s *Storage) Validate(directory, id string, roots []Root, maxBytes int64, checksum string) (map[string]int64, error) {
	file, err := s.Open(directory, id)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	staged, err := ValidateStream(file, roots, maxBytes, func(chunk []byte) { hash.Write(chunk) })
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return nil, fail("BACKUP_CHECKSUM", "The backup checksum does not match. Restore was stopped before changing game data.")
	}
	return staged, nil
}
