package backups

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/tarstream"
	"github.com/Crunging/Ludock/internal/textutil"
)

// ArchiveEntry is one item of a folder inside a backup. A folder's Size and
// Items count everything below it.
type ArchiveEntry struct {
	Name       string
	Type       string
	Size       int64
	Items      int
	ModifiedAt float64
}

var (
	errPathNotFound = fail("BACKUP_PATH_NOT_FOUND", "That file or folder isn’t in this backup.")
	errUnsafePath   = fail("UNSAFE_PATH", "That path isn’t allowed.")
)

// CleanPath checks a path below a data root taken from a request or an
// archive. "" is the root itself.
func CleanPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	parts := strings.Split(value, "/")
	if strings.ContainsAny(value, "\x00\\") || len(parts) > 62 || textutil.Length(value) > 4000 ||
		slices.ContainsFunc(parts, func(part string) bool {
			return part == "" || part == "." || part == ".." || helper.Reserved(part)
		}) {
		return "", errUnsafePath
	}
	return value, nil
}

// archivePath splits a valid entry into its root ID and its path below it.
func archivePath(header *tarstream.Header) (rootID, rest string) {
	name := strings.TrimPrefix(strings.TrimSuffix(header.Name, "/"), "snapshot")
	name = strings.TrimPrefix(name, "/")
	rootID, rest, _ = strings.Cut(name, "/")
	return rootID, rest
}

// walkArchive visits every validated entry of a published archive, its
// snapshot header excluded. Bodies the visitor leaves unread are skipped
// without reading them.
func (s *Storage) walkArchive(directory, id string, roots []Root, visit func(header *tarstream.Header, rootID, rest string, body io.Reader) error) error {
	file, err := s.Open(directory, id)
	if err != nil {
		return err
	}
	defer file.Close()
	count := 0
	err = tarstream.Walk(file, func(header *tarstream.Header, body io.Reader) error {
		if err := ValidateEntry(header, roots); err != nil {
			return err
		}
		if count++; count > maxEntries {
			return fail("UNSAFE_ARCHIVE", "The archive has too many entries.")
		}
		rootID, rest := archivePath(header)
		if rootID == "" {
			return nil
		}
		return visit(header, rootID, rest, body)
	}, nil)
	if errors.Is(err, errStop) {
		return nil
	}
	if _, ok := apperr.As(err); err != nil && !ok {
		return fail("INVALID_BACKUP", "The backup archive is invalid or truncated.")
	}
	return err
}

var errStop = errors.New("stop")

func boolRank(value bool) int {
	if value {
		return 1
	}
	return 0
}

// ListFolder lists one folder of a backup, folders first in natural name
// order. Memory is bounded
// by the folder's own items, at most the archive's entry limit.
func (s *Storage) ListFolder(directory, id string, roots []Root, rootID, folder string) ([]ArchiveEntry, error) {
	if !slices.ContainsFunc(roots, func(root Root) bool { return root.ID == rootID }) {
		return nil, errPathNotFound
	}
	folder, err := CleanPath(folder)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if folder != "" {
		prefix = folder + "/"
	}
	found := folder == ""
	var entries []ArchiveEntry
	index := map[string]int{}
	err = s.walkArchive(directory, id, roots, func(header *tarstream.Header, entryRoot, rest string, _ io.Reader) error {
		if entryRoot != rootID {
			return nil
		}
		if rest == folder && folder != "" {
			if header.Type != "directory" {
				return errPathNotFound
			}
			found = true
			return nil
		}
		below, ok := strings.CutPrefix(rest, prefix)
		if !ok || rest == "" {
			return nil
		}
		name, deeper, nested := strings.Cut(below, "/")
		position, seen := index[name]
		if !seen {
			position = len(entries)
			index[name] = position
			entries = append(entries, ArchiveEntry{Name: name, Type: "directory"})
		}
		entry := &entries[position]
		if !nested || deeper == "" {
			_, _, mtime, _ := EntryMetadata(header)
			entry.Type, entry.ModifiedAt = header.Type, mtime
			if header.Type == "file" {
				entry.Size = header.Size
			}
			return nil
		}
		entry.Items++
		entry.Size += header.Size
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errPathNotFound
	}
	names := make([]string, len(entries))
	for position, entry := range entries {
		names[position] = entry.Name
	}
	keys := textutil.NaturalKeys(names)
	order := make([]int, len(entries))
	for position := range order {
		order[position] = position
	}
	slices.SortFunc(order, func(left, right int) int {
		if folders := cmp.Compare(boolRank(entries[right].Type == "directory"), boolRank(entries[left].Type == "directory")); folders != 0 {
			return folders
		}
		return cmp.Or(bytes.Compare(keys[left], keys[right]), strings.Compare(names[left], names[right]))
	})
	sorted := make([]ArchiveEntry, len(entries))
	for position, index := range order {
		sorted[position] = entries[index]
	}
	return sorted, nil
}

// ReadFile passes one file's size and body from a backup to read, which
// must finish with the body before it returns.
func (s *Storage) ReadFile(directory, id string, roots []Root, rootID, path string, read func(size int64, body io.Reader) error) error {
	if path, err := CleanPath(path); err != nil || path == "" {
		return errUnsafePath
	}
	found := false
	err := s.walkArchive(directory, id, roots, func(header *tarstream.Header, entryRoot, rest string, body io.Reader) error {
		if entryRoot != rootID || rest != path {
			return nil
		}
		if header.Type != "file" {
			return errPathNotFound
		}
		found = true
		if err := read(header.Size, body); err != nil {
			return err
		}
		return errStop
	})
	if err == nil && !found {
		return errPathNotFound
	}
	return err
}

// ValidateItem checks a whole archive and its checksum as Validate does, and
// returns the bytes restoring one item stages.
func (s *Storage) ValidateItem(directory, id string, roots []Root, maxBytes int64, checksum, rootID, path string, progress func()) (int64, error) {
	if clean, err := CleanPath(path); err != nil || clean == "" {
		return 0, errUnsafePath
	}
	file, err := s.Open(directory, id)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	found := false
	var staged int64
	hash := sha256.New()
	_, err = validateStream(file, roots, maxBytes, func(chunk []byte) {
		hash.Write(chunk)
		if progress != nil {
			progress()
		}
	}, func(header *tarstream.Header) {
		entryRoot, rest := archivePath(header)
		if entryRoot != rootID {
			return
		}
		if rest == path || strings.HasPrefix(rest, path+"/") {
			found = found || rest == path
			staged += max(4096, (header.Size+4095)/4096*4096)
		}
	})
	if err != nil {
		return 0, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != checksum {
		return 0, fail("BACKUP_CHECKSUM", "The backup archive is damaged (its checksum doesn’t match). Nothing was changed.")
	}
	if !found {
		return 0, errPathNotFound
	}
	return staged, nil
}
