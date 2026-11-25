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
	"github.com/Crunging/Ludock/internal/helper"
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

// EntryACLs returns an entry's access and default ACLs in helper.ACLText's
// form, or "" for one it hasn't. Only a folder has a default ACL.
func EntryACLs(header *tarstream.Header) (access, def string, err error) {
	for record, target := range map[string]*string{helper.ACLAccessRecord: &access, helper.ACLDefaultRecord: &def} {
		// An empty record is no ACL, as PAX has one override a global record.
		if text := header.PAX[record]; text != "" {
			value, parseErr := helper.ParseACLText(text)
			if parseErr != nil {
				err = parseErr
				continue
			}
			*target, _ = helper.ACLText(value)
		}
	}
	if err != nil || (def != "" && header.Type != "directory") {
		return "", "", fail("UNSAFE_ARCHIVE", "The archive contains invalid access control lists (ACLs).")
	}
	return access, def, nil
}

// MappedHeader renames an entry with fresh, numeric-only PAX metadata and
// its ACLs. This keeps large Linux IDs and dates past 2038 without
// retaining source path or link overrides.
func MappedHeader(header *tarstream.Header, name string) (*tarstream.Header, error) {
	uid, gid, mtime, err := EntryMetadata(header)
	if err != nil {
		return nil, err
	}
	access, def, err := EntryACLs(header)
	if err != nil {
		return nil, err
	}
	pax := map[string]string{"uid": tarstream.FormatSeconds(float64(uid)), "gid": tarstream.FormatSeconds(float64(gid)),
		"mtime": tarstream.FormatSeconds(mtime)}
	for record, text := range map[string]string{helper.ACLAccessRecord: access, helper.ACLDefaultRecord: def} {
		if text != "" {
			pax[record] = text
		}
	}
	return &tarstream.Header{Name: name, Type: header.Type, Size: header.Size, Mode: header.Mode, UID: uid, GID: gid,
		MtimeSeconds: mtime, PAX: pax}, nil
}

// ValidateEntry accepts only regular files and directories inside the
// recorded roots.
func ValidateEntry(header *tarstream.Header, roots []Root) error {
	name := strings.TrimSuffix(header.Name, "/")
	parts := strings.Split(name, "/")
	if _, _, _, err := EntryMetadata(header); err != nil {
		return err
	}
	if _, _, err := EntryACLs(header); err != nil {
		return err
	}
	if strings.Contains(header.Name, "\x00") || strings.Contains(header.Name, "\\") || strings.HasPrefix(header.Name, "/") ||
		slices.ContainsFunc(parts, func(part string) bool { return part == ".." || part == "." || part == "" }) ||
		len(parts) > 64 || textutil.Length(name) > 4096 {
		return fail("UNSAFE_ARCHIVE", "The archive contains an unsafe path.")
	}
	if name != "snapshot" && (!strings.HasPrefix(name, "snapshot/") || !slices.ContainsFunc(roots, func(root Root) bool { return parts[1] == root.ID })) {
		return fail("UNSAFE_ARCHIVE", "The archive has a file outside the server’s data folders.")
	}
	if header.Type != "file" && header.Type != "directory" {
		return fail("UNSAFE_ARCHIVE", "Ludock backs up and restores only ordinary files and folders, and found something else, such as a symbolic link, a hard link, or a device file.")
	}
	// Backups leave Ludock's own folders out, and restores never write them.
	if slices.ContainsFunc(parts, helper.Reserved) {
		return fail("UNSAFE_ARCHIVE", "The archive has a file or folder named like one of Ludock’s own work folders (.ludock-…), which backups never hold.")
	}
	if header.Size < 0 || header.Size > 1<<53-1 {
		return fail("UNSAFE_ARCHIVE", "The archive has an invalid entry size.")
	}
	if (name == "snapshot" || len(parts) == 2) && header.Type != "directory" {
		return fail("BACKUP_ROOT_NOT_DIRECTORY", "One of the server’s data folders is a single mounted file, which Ludock can’t back up. Mount the folder that holds it instead.")
	}
	return nil
}

// maxEntries bounds an archive's entries, its snapshot header included.
// Tests lower it.
var maxEntries = 100000

// errGrown is an archive larger than its recorded size.
var errGrown = fail("INVALID_BACKUP", "The backup archive is larger than when it was made.")

// ValidateStream checks a whole archive and every recorded root.
func ValidateStream(input io.Reader, roots []Root, maxBytes int64, onChunk func([]byte)) (map[string]int64, error) {
	return validateStream(input, roots, maxBytes, onChunk, nil)
}

// validateStream is ValidateStream that also shows onEntry each valid entry.
func validateStream(input io.Reader, roots []Root, maxBytes int64, onChunk func([]byte), onEntry func(*tarstream.Header)) (map[string]int64, error) {
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
		if names[name] || len(names) >= maxEntries {
			return fail("UNSAFE_ARCHIVE", "The archive has duplicate paths or too many entries.")
		}
		names[name] = true
		if onEntry != nil {
			onEntry(header)
		}
		total += header.Size
		if total > maxBytes {
			return errGrown
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
			return errGrown
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
			return nil, fail("INVALID_BACKUP", "The archive is missing one of the server’s data folders.")
		}
	}
	return staged, nil
}

// Validate checks a published archive and its checksum, and returns the
// bytes each root's restore stages. progress, if set, runs as it reads.
func (s *Storage) Validate(directory, id string, roots []Root, maxBytes int64, checksum string, progress func()) (map[string]int64, error) {
	file, err := s.Open(directory, id)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	staged, err := ValidateStream(file, roots, maxBytes, func(chunk []byte) {
		hash.Write(chunk)
		if progress != nil {
			progress()
		}
	})
	if err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return nil, fail("BACKUP_CHECKSUM", "The backup archive is damaged (its checksum doesn’t match). Nothing was changed.")
	}
	return staged, nil
}
