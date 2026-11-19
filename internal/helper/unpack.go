package helper

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// UnpackLimits bound what one archive may unpack to. Ratio compares the
// unpacked bytes, and a compressed tar's decompressed stream, with the
// archive's size; Floor is allowed beyond the ratio so small archives of
// empty files still unpack.
type UnpackLimits struct {
	Bytes   int64
	Entries int
	Depth   int
	Ratio   int64
	Floor   int64
}

// Unpack is the limits every unpack uses; tests lower it.
var Unpack = UnpackLimits{Bytes: 20 << 30, Entries: 100000, Depth: 64, Ratio: 100, Floor: 1 << 20}

// UnpackDuration bounds one unpack. Staging folders older than twice this
// were left by a helper that was killed and are removed by the next unpack.
const UnpackDuration = 30 * time.Minute

// UnpackResult is what an unpack created, next to the archive.
type UnpackResult struct {
	Name    string `json:"name"`
	Files   int    `json:"files"`
	Skipped int    `json:"skipped"`
}

var (
	errArchiveUnsafe = errors.New("the archive has an unsafe entry")
	errArchiveLimit  = errors.New("the archive is over a limit")
	errArchiveFull   = errors.New("the free-space reserve was reached")
	errNotArchive    = errors.New("the file is not a supported archive or is damaged")
	errStopped       = errors.New("the unpack was stopped")
)

var unpackStage = regexp.MustCompile(`^\.ludock-unpack-[0-9a-f-]{36}$`)

func unpackName(id string) (string, error) {
	if !uploadIDPattern.MatchString(id) {
		return "", errors.New("invalid unpack identifier")
	}
	return ".ludock-unpack-" + strings.ToLower(id), nil
}

// archiveParts splits an entry name into the components it creates. Names a
// backup would refuse are refused here too, so unpacked data never breaks a
// later backup; .ludock- names are reserved for Ludock's own staging.
func archiveParts(name string, depth int) ([]string, error) {
	if name == "" || len(name) > 4096 || strings.ContainsAny(name, "\x00\\") || strings.HasPrefix(name, "/") {
		return nil, errArchiveUnsafe
	}
	var parts []string
	for _, part := range strings.Split(name, "/") {
		switch {
		case part == "" || part == ".":
			continue
		case part == ".." || len(part) > 255 || strings.HasPrefix(part, ".ludock-") ||
			strings.IndexFunc(part, func(r rune) bool { return r < 32 || r == 127 }) >= 0:
			return nil, errArchiveUnsafe
		}
		parts = append(parts, part)
	}
	if len(parts) > depth {
		return nil, errArchiveLimit
	}
	return parts, nil
}

// unpackBase names the new folder after the archive without its extension.
func unpackBase(name string) string {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".tar.gz", ".tgz", ".zip", ".tar", ".gz", ".mcworld", ".mcpack", ".mcaddon", ".mrpack"} {
		if strings.HasSuffix(lower, suffix) && len(name) > len(suffix) {
			return name[:len(name)-len(suffix)]
		}
	}
	return name + " files"
}

type unpacker struct {
	h       *fileHelper
	stage   fd
	owner   *unix.Stat_t
	limits  UnpackLimits
	allowed int64
	reserve int64
	written int64
	checked int64
	entries int
	umask   int
	result  UnpackResult
	buffer  []byte
}

func (u *unpacker) stopped() error {
	if u.h.stop.Load() {
		return errStopped
	}
	return nil
}

// room keeps the reserve free after the next chunk, as restores do.
func (u *unpacker) room(size int64) error {
	var stat unix.Statfs_t
	if err := statfs(int(u.stage), &stat); err != nil {
		return err
	}
	block := max(stat.Bsize, stat.Frsize, 1)
	if FreeBytes(&stat) < (size+block-1)/block*block+block+u.reserve {
		return errArchiveFull
	}
	return nil
}

// directory opens parts below the staging folder, creating missing folders
// owned like a new folder. Nothing in the staging folder is a link, and
// O_NOFOLLOW refuses one put there by another writer.
func (u *unpacker) directory(parts []string) (fd, error) {
	current, err := u.h.t.openAt(u.stage, ".", directoryFlags, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		next, err := u.h.t.openAt(current, part, directoryFlags, 0)
		if isNotExist(err) {
			if err = unix.Mkdirat(int(current), part, 0o777); err == nil {
				if next, err = u.h.t.openAt(current, part, directoryFlags, 0); err == nil {
					err = unix.Fchown(int(next), int(u.owner.Uid), int(u.owner.Gid))
				}
			}
		} else if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			err = errArchiveUnsafe
		}
		u.h.t.close(current)
		if err != nil {
			return -1, err
		}
		current = next
	}
	return current, nil
}

func (u *unpacker) entry(name string) ([]string, error) {
	u.entries++
	if u.entries > u.limits.Entries {
		return nil, errArchiveLimit
	}
	if err := u.stopped(); err != nil {
		return nil, err
	}
	return archiveParts(name, u.limits.Depth)
}

func (u *unpacker) mkdir(parts []string) error {
	directory, err := u.directory(parts)
	if err == nil {
		u.h.t.close(directory)
	}
	return err
}

// file creates a regular file the way an upload creates a new one: the
// helper's default mode and the parent folder's owner, whatever the archive
// records.
func (u *unpacker) file(parts []string, body io.Reader) error {
	if len(parts) == 0 {
		return errArchiveUnsafe
	}
	directory, err := u.directory(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	defer u.h.t.close(directory)
	file, err := u.h.t.openAt(directory, parts[len(parts)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, unix.EEXIST) {
		return errArchiveUnsafe
	}
	if err != nil {
		return err
	}
	defer u.h.t.close(file)
	for {
		if err := u.stopped(); err != nil {
			return err
		}
		n, readErr := body.Read(u.buffer)
		if n > 0 {
			u.written += int64(n)
			if u.written > u.limits.Bytes || u.written > u.allowed {
				return errArchiveLimit
			}
			if u.written-u.checked >= copyChunk || u.checked == 0 {
				if err := u.room(copyChunk); err != nil {
					return err
				}
				u.checked = u.written
			}
			for offset := 0; offset < n; {
				written, err := unix.Write(int(file), u.buffer[offset:n])
				if err != nil {
					return err
				}
				offset += written
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, errArchiveLimit) {
				return errArchiveLimit
			}
			return errNotArchive
		}
	}
	if err := unix.Fchmod(int(file), uint32(0o666&^u.umask)); err != nil {
		return err
	}
	if err := unix.Fchown(int(file), int(u.owner.Uid), int(u.owner.Gid)); err != nil {
		return err
	}
	u.result.Files++
	return nil
}

// preader reads a tracked descriptor by offset. An *os.File would close
// the descriptor from its finalizer, after the tracker may have closed it
// and the number been reused.
type preader fd

func (p preader) ReadAt(buffer []byte, offset int64) (int, error) {
	n, err := unix.Pread(int(p), buffer, offset)
	if err != nil {
		return 0, err
	}
	if n < len(buffer) {
		return n, io.EOF
	}
	return n, nil
}

func (u *unpacker) zip(archive io.ReaderAt, size int64) error {
	reader, err := zip.NewReader(archive, size)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return errNotArchive
	}
	if len(reader.File) > u.limits.Entries {
		return errArchiveLimit
	}
	for _, entry := range reader.File {
		parts, err := u.entry(entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			err = u.mkdir(parts)
		case mode.IsRegular():
			var body io.ReadCloser
			if body, err = entry.Open(); err != nil {
				return errNotArchive
			}
			err = u.file(parts, body)
			body.Close()
		default:
			u.result.Skipped++
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// budget fails once more than allowed bytes have been read from a
// decompressed stream.
type budget struct {
	reader  io.Reader
	allowed int64
	read    int64
}

func (b *budget) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += int64(n)
	if b.read > b.allowed {
		return n, errArchiveLimit
	}
	return n, err
}

func (u *unpacker) tar(input io.Reader) error {
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if errors.Is(err, errArchiveLimit) {
			return err
		}
		if err != nil && !errors.Is(err, tar.ErrInsecurePath) {
			return errNotArchive
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		parts, err := u.entry(header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			err = u.mkdir(parts)
		case tar.TypeReg, tar.TypeGNUSparse:
			err = u.file(parts, reader)
		default:
			u.result.Skipped++
		}
		if err != nil {
			return err
		}
	}
}

func (h *fileHelper) sweepUnpacks(parent fd) {
	children, err := names(parent)
	if err != nil {
		return
	}
	for _, child := range children {
		if !unpackStage.MatchString(child) {
			continue
		}
		if info, err := lstatAt(parent, child); err == nil && isDir(info) &&
			time.Since(time.UnixMilli(int64(mtimeMs(info)))) > 2*UnpackDuration {
			h.removeEntry(parent, child, 0)
		}
	}
}

// unpack extracts an archive into a hidden staging folder beside it, then
// renames the result to a name not in use: the archive's only folder when it
// has one, else a folder named after the archive. A failure removes the
// staging folder, and the result never replaces anything.
func (h *fileHelper) unpack(parent fd, name string) error {
	stageName, err := unpackName(h.request.UploadID)
	if err != nil {
		return err
	}
	if h.request.Reserve == nil || *h.request.Reserve < 0 {
		return errors.New("invalid reserve")
	}
	h.sweepUnpacks(parent)
	descriptor, err := h.t.openAt(parent, name, fileFlags, 0)
	if err != nil {
		return err
	}
	info, err := fstat(descriptor)
	if err != nil {
		return err
	}
	if !isRegular(info) || info.Nlink != 1 {
		return errNotArchive
	}
	owner, err := fstat(parent)
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent), stageName, 0o700); err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			h.removeEntry(parent, stageName, 0)
		}
	}()
	stage, err := h.t.openAt(parent, stageName, directoryFlags, 0)
	if err != nil {
		return err
	}
	limits := Unpack
	allowed := limits.Floor
	if info.Size <= (1<<62-limits.Floor)/max(limits.Ratio, 1) {
		allowed += info.Size * limits.Ratio
	}
	u := &unpacker{h: h, stage: stage, owner: owner, limits: limits, allowed: allowed, reserve: *h.request.Reserve,
		umask: currentUmask(), buffer: make([]byte, 64*1024)}
	archive := preader(descriptor)
	var magic [512]byte
	n, _ := unix.Pread(int(descriptor), magic[:], 0)
	switch {
	case bytes.HasPrefix(magic[:n], []byte("PK\x03\x04")) || bytes.HasPrefix(magic[:n], []byte("PK\x05\x06")):
		err = u.zip(archive, info.Size)
	case bytes.HasPrefix(magic[:n], []byte{0x1f, 0x8b}):
		var decompressed *gzip.Reader
		if decompressed, err = gzip.NewReader(bufio.NewReaderSize(io.NewSectionReader(archive, 0, info.Size), 64*1024)); err != nil {
			return errNotArchive
		}
		err = u.tar(&budget{reader: decompressed, allowed: allowed})
	case n == 512 && bytes.HasPrefix(magic[257:], []byte("ustar")):
		err = u.tar(io.NewSectionReader(archive, 0, info.Size))
	default:
		err = errNotArchive
	}
	if err != nil {
		return err
	}
	if err := u.stopped(); err != nil {
		return err
	}
	// Written data is durable before its name appears.
	if err := unix.Syncfs(int(stage)); err != nil {
		return err
	}
	result, err := h.finishUnpack(parent, name, stageName, stage, owner, u.umask, u.result)
	if err != nil {
		return err
	}
	done = true
	return h.json(result)
}

func (h *fileHelper) finishUnpack(parent fd, archive, stageName string, stage fd, owner *unix.Stat_t, umask int, result UnpackResult) (UnpackResult, error) {
	children, err := names(stage)
	if err != nil {
		return result, err
	}
	source, sourceName, base := parent, stageName, unpackBase(archive)
	if len(children) == 1 {
		if info, err := lstatAt(stage, children[0]); err == nil && isDir(info) {
			source, sourceName, base = stage, children[0], children[0]
		}
	}
	if source == parent {
		if err := unix.Fchmod(int(stage), uint32(0o777&^umask)); err != nil {
			return result, err
		}
		if err := unix.Fchown(int(stage), int(owner.Uid), int(owner.Gid)); err != nil {
			return result, err
		}
	}
	info, err := lstatAt(source, sourceName)
	if err != nil {
		return result, err
	}
	for attempt := 1; ; attempt++ {
		candidate := base
		if attempt > 1 {
			candidate = base + " (" + strconv.Itoa(attempt) + ")"
		}
		if len(candidate) > 255 || attempt > 50 {
			return result, errNameTaken
		}
		if _, err := lstatAt(parent, candidate); err == nil {
			continue
		} else if !isNotExist(err) {
			return result, err
		}
		if err := renameNoReplace(source, sourceName, parent, candidate, info); errors.Is(err, unix.EEXIST) {
			continue
		} else if err != nil {
			return result, err
		}
		result.Name = candidate
		break
	}
	if source == stage {
		if err := unix.Unlinkat(int(parent), stageName, unix.AT_REMOVEDIR); err != nil {
			return result, err
		}
	}
	return result, unix.Fsync(int(parent))
}
