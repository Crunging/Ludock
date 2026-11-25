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
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
)

// UploadEntry is one file of a batch upload; its Size bytes follow the
// previous entry's in the body. Err, when set, refuses it unread.
type UploadEntry struct {
	Name string
	Size int64
	Err  error
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

// UploadFiles writes entries into one folder through one helper command.
// Each file commits
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
			entry.Err = ValidateName(entry.Name)
		}
		if entry.Err == nil && (entry.Size < 0 || entry.Size > 1<<53-1) {
			entry.Err = Error("INVALID_UPLOAD_SIZE", 400)
		}
		results[index] = entry.Err
	}
	root, normalized, err := resolve(target, rootID, folder)
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
		var space *helper.FileSpace
		if space, err = access.space(ctx, root, normalized); err != nil {
			access.Cleanup(false)
		} else {
			// The rest may fit once earlier files replace larger ones.
			if index := slices.Index(results, nil); entries[index].Size > space.Available-helper.UploadSpare(space.Total) {
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
			files = append(files, helper.UploadFile{Name: entry.Name, Size: entry.Size, UploadID: input.ids[index]})
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
	list, _ := json.Marshal(files)
	input.trailer = binary.BigEndian.AppendUint64(nil, uint64(len(list)))
	input.trailer = append(input.trailer, list...)
	acks := &uploadAcks{limit: len(files), ready: make(chan struct{}, len(files))}
	input.ready = acks.ready
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
	if err == nil {
		access.Cleanup(false)
		return results, nil
	}
	// A failed upload may still be writing, so its helper is removed before
	// the locks are released, as for Delete. The helper removes its staging
	// file unless it was cut off, and answers that may be lost don't say how
	// far it got, so a fresh helper then removes every unanswered file's.
	// Cleanup has no user-controlled input and runs even after cancellation or
	// revocation.
	access.Cleanup(true)
	if len(acks.results) < len(accepted) {
		var staged []helper.UploadFile
		for _, file := range files[len(acks.results):] {
			staged = append(staged, helper.UploadFile{UploadID: file.UploadID})
		}
		settled := context.WithoutCancel(ctx)
		cleanup, cleanupErr := s.Acquire(settled, target, root, false, nil)
		if cleanupErr == nil {
			_, cleanupErr = cleanup.request(settled, helper.FileRequest{Operation: "upload-files-cleanup", Root: root.Path, Path: normalized,
				Files: staged}, nil, nil)
			cleanup.Cleanup(cleanupErr != nil)
		}
		if cleanupErr != nil {
			log.Warn("Failed to clean up an upload temporary file", logging.F("container", target.ContainerID[:min(12, len(target.ContainerID))]))
		}
		return results, err
	}
	// Every file was answered, such as a large one the helper refused and
	// then stopped instead of reading past, so later batches can still go.
	return results, nil
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
	body    io.Reader
	check   func() error
	ready   <-chan struct{}
	done    chan struct{}
	entries []UploadEntry
	ids     []string
	index   int
	left    int64
	started bool
	trailer []byte

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
	// ready has a token for each file the helper asked the trailer of.
	ready   chan struct{}
	readies int
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

func (a *Access) space(ctx context.Context, root Root, folder string) (*helper.FileSpace, error) {
	out, err := a.request(ctx, helper.FileRequest{Operation: "space", Root: root.Path, Path: folder}, nil, nil)
	if err != nil {
		return nil, err
	}
	var space helper.FileSpace
	if err := json.Unmarshal(out, &space); err != nil || space.Total <= 0 {
		return nil, Error("FILE_OPERATION_FAILED", 409)
	}
	return &space, nil
}
