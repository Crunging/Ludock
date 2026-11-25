package files

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
)

// UploadEntry is one file of a batch upload; its Size bytes follow the
// previous entry's in the body. Name is its path below the batch's folder.
// Without Replace, a name that is taken is refused with launch.ErrNameTaken.
// Err, when set, refuses it unread.
type UploadEntry struct {
	Name    string
	Size    int64
	Replace bool
	Err     error
}

// ErrUploadUnconfirmed is a file whose upload stopped after it may have
// been committed.
var ErrUploadUnconfirmed = apperr.New("UPLOAD_UNCONFIRMED", 500, "The upload may have completed")

var errNotUploaded = apperr.New("UPLOAD_STOPPED", 409, "The upload stopped before this file")

// ErrBatchTooLarge refuses a batch past what the page sends, before any of
// it is read: refused files are read past, so a batch bounds that reading.
var ErrBatchTooLarge = apperr.New("UPLOAD_BATCH_TOO_LARGE", 413, fmt.Sprintf("Upload at most %d files and %s at once; a larger file goes on its own.",
	helper.UploadBatchFiles, datasize.Format(helper.UploadBatchBytes)))

// BatchFits reports whether files of these sizes may go in one batch.
func BatchFits(sizes []int64) bool {
	var total int64
	for _, size := range sizes {
		if size < 0 || size > 1<<53-1 {
			return false
		}
		total += size
	}
	return len(sizes) <= helper.UploadBatchFiles && (len(sizes) == 1 || total <= helper.UploadBatchBytes)
}

// UploadFiles writes entries below one folder through one helper command,
// which makes the folders they need as New folder does. Each file commits
// only after its trailer, which is sent after its whole body and an access
// check. It returns each entry's result, and the error that stopped the
// batch, after which later entries were not written.
func (s *Service) UploadFiles(ctx context.Context, target Target, rootID, folder string, entries []UploadEntry, body io.Reader,
	assertAccess func() error) ([]error, error) {
	results := make([]error, len(entries))
	// Every result left nil by a failure is that failure.
	fail := func(err error) ([]error, error) {
		for index := range results {
			if results[index] == nil {
				results[index] = err
			}
		}
		return results, err
	}
	for index, entry := range entries {
		if entry.Err == nil {
			entry.Err = validateUploadName(entry.Name)
		}
		if entry.Err == nil && (entry.Size < 0 || entry.Size > 1<<53-1) {
			entry.Err = Error("INVALID_UPLOAD_SIZE", 400)
		}
		results[index] = entry.Err
	}
	root, normalized, err := resolve(target, rootID, folder)
	for index, entry := range entries {
		if results[index] == nil && err == nil {
			_, results[index] = joinChecked(normalized, entry.Name)
		}
	}
	sizes := make([]int64, len(entries))
	for index, entry := range entries {
		sizes[index] = entry.Size
	}
	if err == nil && !BatchFits(sizes) {
		err = ErrBatchTooLarge
	}
	if err != nil {
		return fail(err)
	}
	input := &batchInput{body: body, done: make(chan struct{}), entries: entries, ids: make([]string, len(entries)), sent: make([]bool, len(entries))}
	if !slices.Contains(results, nil) {
		return results, nil
	}
	access, err := s.Acquire(ctx, target, root, false, assertAccess)
	if err == nil {
		// Uploads have no size limit by default, so a file that would fill the
		// disk is refused before anything is written; the helper checks each
		// file again as the batch fills the disk.
		var names []string
		for index, entry := range entries {
			if results[index] == nil {
				names = append(names, entry.Name)
			}
		}
		var space *helper.FileSpace
		if space, err = access.prepareUpload(ctx, target, root, normalized, names); err != nil {
			// Its sweep may still be removing files.
			access.Cleanup(true)
		} else {
			// The rest may fit once earlier files replace larger ones. A file
			// below the folder may be on another disk, which the helper
			// measures as it writes.
			if index := slices.Index(results, nil); !strings.Contains(entries[index].Name, "/") &&
				entries[index].Size > space.Available-helper.UploadSpare(space.Total) {
				results[index] = uploadNoSpace(*space, entries[index].Size)
			}
		}
	}
	if err != nil {
		return fail(err)
	}
	var files []helper.UploadFile
	var accepted []int
	for index, entry := range entries {
		if results[index] == nil {
			input.ids[index] = identity.NewUUID()
			files = append(files, helper.UploadFile{Name: entry.Name, Size: entry.Size, UploadID: input.ids[index], Replace: entry.Replace})
			accepted = append(accepted, index)
		}
	}
	if len(files) == 0 {
		access.Cleanup(false)
		return results, nil
	}
	// Each file is committed only while the game is still the run the
	// helper's mounts were proven for: a restart onto a replaced folder would
	// leave the helper writing to the old one.
	input.check = func() error {
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		inspection, err := s.Docker.Inspect(ctx, target.ContainerID)
		if err == nil && (signature(inspection) != access.signature || inspection.State.StartedAt != access.startedAt) {
			err = Error("FILE_TARGET_CHANGED", 409)
		}
		return err
	}
	input.trailer = fileList(files)
	acks := &uploadAcks{limit: len(files), ready: make(chan struct{}, len(files)), folders: make(chan struct{}, len(files)),
		files: make(chan struct{}, 1)}
	input.ready, input.folders, input.files = acks.ready, acks.folders, acks.files
	_, err = access.exec(ctx, helper.FileRequest{Operation: "upload-files", Root: root.Path, Path: normalized},
		launch.ExecOptions{Input: input, Stdout: acks})
	// Exec can return while its input is still being read; once stopped, no
	// further trailer is handed on, so sent covers every file that may be
	// committed.
	sent := input.stop()
	if err == nil && len(acks.results) != len(files) {
		err = launch.ErrUnsafe
	}
	for position, index := range accepted {
		switch {
		case position < len(acks.results):
			results[index] = acks.result(acks.results[position], entries[index].Size)
		case sent[index]:
			results[index] = ErrUploadUnconfirmed
		case err != nil && isRefusal(err):
			results[index] = err
		default:
			results[index] = errNotUploaded
		}
	}
	// The helper removes a file's staging file unless it was cut off or
	// reports it could not, and answers that may be lost don't say how far it
	// got, so every unanswered file's is removed too. Cleanup has no
	// user-controlled input and runs even after cancellation or revocation.
	var staged []helper.UploadFile
	for position, file := range files {
		if position >= len(acks.results) || acks.results[position].Leftover {
			staged = append(staged, helper.UploadFile{Name: file.Name, UploadID: file.UploadID})
		}
	}
	removeStaged := func(access *Access) error {
		// It runs even once the uploader's access is gone.
		access = &Access{service: s, container: access.container, blocked: access.blocked}
		_, err := access.exec(context.WithoutCancel(ctx), helper.FileRequest{Operation: "upload-files-cleanup", Root: root.Path,
			Path: normalized}, launch.ExecOptions{Input: bytes.NewReader(fileList(staged)), Limit: time.Minute})
		if err != nil {
			log.Warn("Failed to clean up an upload temporary file", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
		}
		return err
	}
	if err == nil {
		var cleanupErr error
		if len(staged) > 0 {
			cleanupErr = removeStaged(access)
		}
		access.Cleanup(cleanupErr != nil)
		return results, nil
	}
	// A failed upload may still be writing, so its helper is removed before
	// the locks are released, as for Delete, and a fresh one cleans up.
	access.Cleanup(true)
	if len(staged) > 0 {
		settled := context.WithoutCancel(ctx)
		if cleanup, cleanupErr := s.Acquire(settled, target, root, false, nil); cleanupErr == nil {
			cleanup.Cleanup(removeStaged(cleanup) != nil)
		} else {
			log.Warn("Failed to clean up an upload temporary file", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
		}
	}
	if len(acks.results) < len(accepted) {
		return results, err
	}
	// Every file was answered, such as a large one the helper refused and
	// then stopped instead of reading past, so later batches can still go.
	return results, nil
}

// validateUploadName checks a path below an upload's folder: each name in it
// as a new name would be.
func validateUploadName(name string) error {
	for _, part := range strings.Split(name, "/") {
		if err := ValidateName(part); err != nil {
			return err
		}
	}
	return nil
}

// fileList is a list of files as the helper reads it from stdin: its length,
// then its JSON.
func fileList(files []helper.UploadFile) []byte {
	list, _ := json.Marshal(files)
	return append(binary.BigEndian.AppendUint64(nil, uint64(len(list))), list...)
}

// isRefusal reports a public error that says nothing was written.
func isRefusal(err error) bool {
	public, ok := apperr.As(err)
	return ok && public.Status < 500
}

// batchInput is a batch's stdin: each accepted entry's body, then its
// trailer once the helper has staged the file and asks for it, and access
// and the game are confirmed. Refused entries are read past.
type batchInput struct {
	body  io.Reader
	check func() error
	ready <-chan struct{}
	// folders has a token for each missing folder the helper asks to make
	// before the files, and files one once their bodies may follow.
	folders, files <-chan struct{}
	begun          bool
	done           chan struct{}
	entries        []UploadEntry
	ids            []string
	index          int
	left           int64
	started        bool
	trailer        []byte

	mu sync.Mutex
	// sent marks entries whose trailer was handed on, which may be committed.
	sent    []bool
	stopped bool
}

var errInputStopped = errors.New("upload input stopped")

// stop hands on no more trailers and returns which entries were sent.
func (b *batchInput) stop() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.stopped {
		close(b.done)
	}
	b.stopped = true
	return slices.Clone(b.sent)
}

func (b *batchInput) Read(p []byte) (int, error) {
	for {
		if len(b.trailer) > 0 {
			n := copy(p, b.trailer)
			b.trailer = b.trailer[n:]
			return n, nil
		}
		// A folder is made only once access and the game are confirmed again,
		// as each file is before it is published.
		if !b.begun {
			select {
			case <-b.folders:
				b.mu.Lock()
				err := errInputStopped
				if !b.stopped {
					if err = nil; b.check != nil {
						err = b.check()
					}
				}
				b.mu.Unlock()
				if err != nil {
					return 0, err
				}
				b.trailer = []byte{helper.FolderGo}
			case <-b.files:
				b.begun = true
			case <-b.done:
				return 0, errInputStopped
			}
			continue
		}
		if b.index >= len(b.entries) {
			return 0, io.EOF
		}
		if !b.started {
			b.left, b.started = b.entries[b.index].Size, true
		}
		if b.ids[b.index] == "" {
			if _, err := io.CopyN(io.Discard, b.body, b.left); err != nil {
				return 0, err
			}
			b.index, b.started = b.index+1, false
			continue
		}
		if b.left > 0 {
			n, err := b.body.Read(p[:min(int64(len(p)), b.left)])
			b.left -= int64(n)
			if errors.Is(err, io.EOF) {
				err = nil
				if b.left > 0 {
					err = io.ErrUnexpectedEOF
				}
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		select {
		case <-b.ready:
		case <-b.done:
		}
		b.mu.Lock()
		err := errInputStopped
		if !b.stopped {
			if err = nil; b.check != nil {
				err = b.check()
			}
			b.sent[b.index] = err == nil
		}
		b.mu.Unlock()
		if err != nil {
			return 0, err
		}
		b.trailer = []byte(b.ids[b.index])
		b.index, b.started = b.index+1, false
	}
}

// uploadAcks reads the helper's line for each file as it arrives, so files
// committed before a failure are known.
type uploadAcks struct {
	limit   int
	pending []byte
	results []helper.UploadResult
	// ready has a token for each file the helper asked the trailer of, and
	// folders for each folder it asked to make; files, once it has made them.
	ready          chan struct{}
	readies        int
	folders, files chan struct{}
	asked          int
	began          bool
}

func (a *uploadAcks) Write(p []byte) (int, error) {
	a.pending = append(a.pending, p...)
	for {
		line, rest, found := bytes.Cut(a.pending, []byte{'\n'})
		if !found {
			break
		}
		var result helper.UploadResult
		if json.Unmarshal(line, &result) != nil {
			return 0, errors.New("unexpected upload output")
		}
		a.pending = rest
		// Each of a batch's folders holds at least one of its files.
		if result.Folder {
			if a.began || a.asked >= a.limit {
				return 0, errors.New("unexpected upload output")
			}
			a.asked++
			a.folders <- struct{}{}
			continue
		}
		if result.Files {
			if a.began {
				return 0, errors.New("unexpected upload output")
			}
			a.began = true
			a.files <- struct{}{}
			continue
		}
		if result.Ready {
			if a.readies >= a.limit {
				return 0, errors.New("unexpected upload output")
			}
			a.readies++
			a.ready <- struct{}{}
			continue
		}
		if len(a.results) >= a.limit {
			return 0, errors.New("unexpected upload output")
		}
		a.results = append(a.results, result)
	}
	if len(a.pending) > 4096 {
		return 0, errors.New("unexpected upload output")
	}
	return len(p), nil
}

func (a *uploadAcks) result(result helper.UploadResult, size int64) error {
	switch {
	case result.OK:
		return nil
	case result.Code == helper.FileNoSpace && result.Space != nil:
		return uploadNoSpace(*result.Space, size)
	case result.Code == helper.FileUncertain:
		return ErrUploadUnconfirmed
	case result.Code == helper.FileNoSpace:
		return apperr.New("NOT_ENOUGH_SPACE", 507, "Not enough free space on the server’s disk.")
	}
	if err := launch.FileExitError(result.Code); err != nil {
		return err
	}
	return launch.ErrUnsafe
}

func uploadNoSpace(space helper.FileSpace, size int64) error {
	return apperr.New("NOT_ENOUGH_SPACE", 507, fmt.Sprintf("Not enough free space: the file needs %s, and %s is free on the server’s disk, of which Ludock leaves %s for the game.",
		datasize.Format(size), datasize.Format(max(space.Available, 0)), datasize.Format(helper.UploadSpare(space.Total))))
}

// prepareUpload removes the staging files cut-off uploads left in folder and
// the folders below it that entries go in, which may be what fills the disk,
// then measures its free space.
func (a *Access) prepareUpload(ctx context.Context, target Target, root Root, folder string, names []string) (*helper.FileSpace, error) {
	var files []helper.UploadFile
	for _, name := range names {
		files = append(files, helper.UploadFile{Name: name})
	}
	// Only a list goes in, so the bound stays a command's without input.
	out, err := a.exec(ctx, helper.FileRequest{Operation: "upload-prepare", Root: root.Path, Path: folder},
		launch.ExecOptions{Input: bytes.NewReader(fileList(files)), Limit: time.Minute})
	if err != nil {
		return nil, err
	}
	var prepared helper.UploadPrepared
	if err := json.Unmarshal(out, &prepared); err != nil {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	logStuck(target, prepared.Stuck)
	if prepared.Total <= 0 {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	return &prepared.FileSpace, nil
}

// logStuck says once per upload that its folder holds staging files of cut-off
// uploads that could not be removed, without their paths.
func logStuck(target Target, stuck int) {
	if stuck > 0 {
		log.Warn("Some files left by interrupted uploads could not be removed", logging.F("server", target.ServerID), logging.F("count", stuck))
	}
}
