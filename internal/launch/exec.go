package launch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
)

const MaxOutput = 4 * 1024 * 1024

// Errors from helper commands. Messages are public.
var (
	ErrTimeout     = apperr.New("FILE_OPERATION_TIMEOUT", 409, "The file change took too long. Refresh and try again.")
	ErrFailed      = apperr.New("FILE_OPERATION_FAILED", 409, "The file change failed")
	ErrUnsafe      = apperr.New("UNSAFE_OR_CHANGED_FILE_PATH", 409, "The path changed, is unsafe, or cannot be accessed")
	ErrNameTaken   = apperr.New("FILE_NAME_TAKEN", 409, "A file or folder with that name already exists")
	ErrFileChanged = apperr.New("FILE_CHANGED", 409, "The file changed since you opened it, so your edit was not saved. Copy your text, then reopen the file to see the new version.")
	ErrOutputLimit = apperr.New("FILE_OUTPUT_LIMIT", 409, "The file change failed")
	ErrUpload      = apperr.New("FILE_UPLOAD_FAILED", 400, "The file change failed")
)

// ExecErrors are the public errors a helper command fails with.
type ExecErrors struct {
	// Failed is a broken stream; Unsafe a command that failed or whose end
	// Docker didn't confirm.
	Timeout, Failed, Unsafe, OutputLimit *apperr.Error
	// Exits are the public errors of exit codes that report a refusal.
	Exits map[int]*apperr.Error
}

var fileErrors = &ExecErrors{Timeout: ErrTimeout, Failed: ErrFailed, Unsafe: ErrUnsafe, OutputLimit: ErrOutputLimit, Exits: map[int]*apperr.Error{
	helper.FileNameTaken:     ErrNameTaken,
	helper.FileChanged:       ErrFileChanged,
	helper.FileArchiveUnsafe: apperr.New("ARCHIVE_UNSAFE", 422, "The archive wasn’t unpacked: it has entries with unsafe names, such as paths that lead outside its folder."),
	helper.FileArchiveLimit: apperr.New("ARCHIVE_TOO_LARGE", 413, "The archive wasn’t unpacked: it unpacks to more than "+
		datasize.Format(helper.Unpack.Bytes)+" or 100 times its own size, or has too many entries or nested folders."),
	helper.FileArchiveFull: apperr.New("ARCHIVE_NO_SPACE", 409, "The archive wasn’t unpacked: the disk would run short of free space."),
	helper.FileNotArchive:  apperr.New("NOT_AN_ARCHIVE", 415, "This file isn’t a zip, tar, or tar.gz archive, or it is damaged."),
	helper.FileACLNotKept:  apperr.New("ACL_NOT_KEPT", 409, "The file wasn’t replaced: Ludock couldn’t keep its access control list, so the new file would have had wider permissions."),
	helper.FileNotFolder:   apperr.New("NOT_A_FOLDER", 409, "A file has the name of a folder this goes in, so it wasn’t uploaded."),
	helper.FileIsFolder:    apperr.New("IS_A_FOLDER", 409, "A folder has this name, so the file wasn’t uploaded."),
	helper.FileArchiveDeep: apperr.New("ARCHIVE_TOO_DEEP", 409, "The archive wasn’t unpacked: its files would sit deeper than Files can open. Move it to a folder nearer the top and unpack it there."),
	// A 5xx status reports the result as unconfirmed.
	helper.FilePartial:      apperr.New("FILE_PARTIAL", 500, "Only part of the selection was changed"),
	helper.FileUncertain:    apperr.New("FILE_UNCERTAIN", 500, "The file change may have completed"),
	helper.FileMounted:      apperr.New("MOUNTED_FOLDER", 409, "This is, or holds, a folder mounted into the server separately, which Ludock won’t delete, rename, or move. Change what’s inside that folder instead."),
	helper.FileHoldsRestore: apperr.New("RESTORE_FOLDER_INSIDE", 409, "This folder holds a hidden .ludock-restore-… folder, where a restore keeps the server’s previous data, so Ludock won’t delete, rename, or move it. Finish or undo that restore from Activity or Needs attention; if no restore needs the folder, keep what you need from it and delete it on the host."),
}}

// exit is the error of a command Docker reports as ended with info.
func (e *ExecErrors) exit(info *docker.ExecInspect) error {
	if info == nil || info.Running || info.ExitCode == nil {
		return e.Unsafe
	}
	if refusal := e.Exits[*info.ExitCode]; refusal != nil {
		return refusal
	}
	if *info.ExitCode != 0 {
		return e.Unsafe
	}
	return nil
}

type ExecOptions struct {
	Cmd []string
	// Input is streamed to stdin; Trailer follows it only after the whole
	// input arrived and every access check passed, so the helper commits the
	// upload only then. A public error from Input fails the command with it.
	Input   io.Reader
	Trailer []byte
	// Stdout, when set, receives the output as it arrives, including what a
	// command that then fails printed; Exec returns no output. It is not
	// limited.
	Stdout io.Writer
	// AssertAccess runs before the exec, after it is created, and between
	// input chunks; with Limit, also every second while the command runs.
	// A settling command checks it only after the create.
	AssertAccess func() error
	// Limit replaces a command's bound: one minute, or 30 with input.
	Limit time.Duration
	// Errors replaces the file errors.
	Errors *ExecErrors
	// Settle makes the command outlast the caller's context and its stream,
	// for a next step that writes the same data: Exec returns only once
	// Docker confirms it ended or no longer knows it, or Limit passed and the
	// helper is stopped. It takes no Input, and its Limit is used as given.
	Settle bool
	// Poll is how often a settling command is inspected; zero means 250ms.
	Poll time.Duration
	// Watch runs every second while a settling command runs, with a context
	// that ends before Exec returns. An error from it stops the helper, as
	// the Limit does, and Exec returns that error.
	Watch func(context.Context) error
}

var errOutputLimit = errors.New("helper output exceeded its limit")

// limitedBuffer does not embed bytes.Buffer, whose ReadFrom would bypass
// the limit inside io.Copy.
type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > MaxOutput {
		return 0, errOutputLimit
	}
	return b.buffer.Write(p)
}

// Periodic limits check to once a second, since a full check costs several
// database reads. The first call always checks, and once check refuses, every
// later call refuses.
func Periodic(check func() error) func() error {
	return periodic(check, time.Now)
}

func periodic(check func() error, now func() time.Time) func() error {
	if check == nil {
		return nil
	}
	var last time.Time
	var refused error
	return func() error {
		if refused == nil && (last.IsZero() || now().Sub(last) >= time.Second) {
			if refused = check(); refused == nil {
				last = now()
			}
		}
		return refused
	}
}

// Exec runs a helper command in a helper container and returns its stdout.
// Unless it settles, ctx cancellation stops the command: commands without
// input are cut off immediately, uploads get five seconds to remove their
// staging file.
func Exec(ctx context.Context, client *docker.Client, container string, options ExecOptions) ([]byte, error) {
	errs := options.Errors
	if errs == nil {
		errs = fileErrors
	}
	check := func() error {
		if options.AssertAccess != nil {
			return options.AssertAccess()
		}
		return nil
	}
	var execID string
	create := func(ctx context.Context) (err error) {
		execID, err = client.CreateExec(ctx, container, docker.ExecCreate{Cmd: options.Cmd, AttachStdout: true,
			AttachStderr: true, AttachStdin: options.Input != nil})
		return err
	}
	if !options.Settle {
		if err := check(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if err := Step(ctx, "creating a helper command", create); err != nil {
		return nil, err
	}
	if err := check(); err != nil {
		return nil, err
	}
	var err error
	limit := time.Minute
	if options.Limit > 0 || options.Settle {
		limit = options.Limit
	} else if options.Input != nil {
		limit = 30 * time.Minute
	}
	parent := ctx
	if options.Settle {
		parent = context.WithoutCancel(ctx)
	}
	// Docker can accept the connection and never answer it.
	deadline, cancelDeadline := context.WithTimeoutCause(parent, limit, errs.Timeout)
	defer cancelDeadline()
	var watched context.CancelCauseFunc
	if options.Settle && options.Watch != nil {
		deadline, watched = context.WithCancelCause(deadline)
		done := make(chan struct{})
		// Watched from before the start, which can wait unanswered while the
		// helper already runs.
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-deadline.Done():
					return
				case <-ticker.C:
					if err := options.Watch(deadline); err != nil {
						watched(err)
						return
					}
				}
			}
		}()
		// The watch reads the caller's state, so it ends before Exec does.
		defer func() { watched(nil); <-done }()
	}
	var conn *docker.Conn
	if options.Settle {
		conn, err = Start(deadline, client, container, execID, options.Poll)
	} else {
		conn, err = client.StartExec(deadline, execID)
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		} else if err != nil && deadline.Err() != nil {
			err = errs.Timeout
		}
	}
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var mu sync.Mutex
	var failure error
	fail := func(err error) {
		mu.Lock()
		if failure == nil {
			failure = err
		}
		mu.Unlock()
	}
	current := func() error {
		mu.Lock()
		defer mu.Unlock()
		return failure
	}
	var graceOnce sync.Once
	abortSoon := func() {
		graceOnce.Do(func() { time.AfterFunc(5*time.Second, func() { conn.Close() }) })
	}
	stop := context.AfterFunc(deadline, func() {
		if ctx.Err() != nil {
			fail(ctx.Err())
		} else {
			fail(context.Cause(deadline))
		}
		if options.Input != nil {
			// Omit the trailer and send EOF; keep stdout while the helper
			// removes its staging file.
			conn.CloseWrite()
			abortSoon()
		} else {
			conn.Close()
		}
	})
	defer stop()
	if options.Input == nil && options.Limit > 0 && !options.Settle {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			for {
				select {
				case <-finished:
					return
				case <-ticker.C:
					if err := check(); err != nil {
						fail(err)
						conn.Close()
						return
					}
				}
			}
		}()
	}

	// Set once the whole input and its trailer are written, before the write
	// side closes: the helper can read that close and exit before the sender
	// returns from it.
	var sent atomic.Bool
	if options.Input == nil {
		sent.Store(true)
	} else {
		go func() {
			buffer := make([]byte, 64*1024)
			periodic := Periodic(check)
			for {
				n, readErr := options.Input.Read(buffer)
				if current() != nil {
					break
				}
				if err := periodic(); err != nil {
					fail(err)
					break
				}
				if n > 0 {
					if _, err := conn.Write(buffer[:n]); err != nil {
						fail(ErrUpload)
						break
					}
				}
				if readErr == io.EOF {
					// The trailer commits the upload, so access is confirmed after the
					// last body write, which can block, and right before sending it.
					if len(options.Trailer) > 0 && current() == nil {
						if err := check(); err != nil {
							fail(err)
						} else if _, err := conn.Write(options.Trailer); err != nil {
							fail(ErrUpload)
						}
					}
					sent.Store(true)
					break
				}
				if readErr != nil {
					if _, public := apperr.As(readErr); public {
						fail(readErr)
					} else {
						fail(ErrUpload)
					}
					break
				}
			}
			conn.CloseWrite()
			if current() != nil {
				abortSoon()
			}
		}()
	}
	var stdout limitedBuffer
	var output io.Writer = &stdout
	if options.Stdout != nil {
		output = options.Stdout
	}
	demuxErr := docker.Demux(conn, output, nil)
	var info *docker.ExecInspect
	if options.Settle {
		conn.Close()
		info = WaitForExit(client, container, execID, options.Poll, func() bool { return deadline.Err() != nil })
		if cause := context.Cause(deadline); watched != nil && cause != nil && !errors.Is(cause, errs.Timeout) {
			return nil, cause
		}
	} else {
		// The helper only succeeds after the whole input and its trailer, so an
		// input still being read has already failed; don't wait for it.
		if !sent.Load() {
			fail(ErrUpload)
		}
		if err := current(); err != nil {
			return nil, err
		}
	}
	if demuxErr != nil {
		if errors.Is(demuxErr, errOutputLimit) {
			return nil, errs.OutputLimit
		}
		if options.Settle && deadline.Err() != nil {
			return nil, errs.Timeout
		}
		return nil, errs.Failed
	}
	if !options.Settle {
		if info, err = awaitExit(ctx, deadline, client, execID); err != nil {
			return nil, err
		}
	}
	if err := errs.exit(info); err != nil {
		return nil, err
	}
	return stdout.buffer.Bytes(), nil
}

// ExitWait bounds how long a file command whose stream ended waits for
// Docker to record its exit; tests shorten it.
var ExitWait = 5 * time.Second

// awaitExit inspects a command whose stream ended. Docker can close the
// stream a moment before it records the exit, so a command still shown as
// running is inspected again for up to ExitWait.
func awaitExit(ctx, deadline context.Context, client *docker.Client, execID string) (*docker.ExecInspect, error) {
	wait, cancel := context.WithTimeout(deadline, ExitWait)
	defer cancel()
	for {
		info, err := client.InspectExec(context.WithoutCancel(ctx), execID)
		if err != nil || (!info.Running && info.ExitCode != nil) {
			return info, err
		}
		select {
		case <-wait.Done():
			return info, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Start starts an exec in a helper. A start that may have reached Docker
// can leave the command running, so a failed one first waits for it to end,
// stopping the helper once ctx is done, whose cause it then returns.
func Start(ctx context.Context, client *docker.Client, container, execID string, poll time.Duration) (*docker.Conn, error) {
	conn, err := client.StartExec(ctx, execID)
	if err != nil {
		var open *docker.StreamOpenError
		if errors.As(err, &open) && open.RequestStarted {
			WaitForExit(client, container, execID, poll, func() bool { return ctx.Err() != nil })
		}
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, err
	}
	return conn, nil
}

// WaitForExit waits for an exec to finish and returns its record, or nil
// once Docker no longer knows it or, after expired, the helper is stopped.
// Losing the stream does not end the process, so the next step must not
// write concurrently with it; stopping keeps a hang from holding the
// operation.
func WaitForExit(client *docker.Client, container, execID string, poll time.Duration, expired func() bool) *docker.ExecInspect {
	if poll == 0 {
		poll = 250 * time.Millisecond
	}
	for {
		result, err := client.InspectExec(context.Background(), execID)
		if err == nil && !result.Running && result.ExitCode != nil {
			return result
		}
		if docker.IsNotFound(err) {
			return nil
		}
		if expired() && stopHelper(client, container) {
			return nil
		}
		time.Sleep(poll)
	}
}

// stopHelper stops a helper. Docker cannot signal an exec; stopping the
// helper ends its PID namespace, so nothing can still run inside it.
func stopHelper(client *docker.Client, container string) bool {
	ctx := context.Background()
	zero := 0
	if err := client.Stop(ctx, container, &zero); err != nil {
		if status := docker.StatusCode(err); status == 404 {
			return true
		} else if status != 304 {
			return false
		}
	}
	info, err := client.Inspect(ctx, container)
	if docker.IsNotFound(err) {
		return true
	}
	return err == nil && !info.State.Running
}

// FileExitError is the public error of a file helper exit code, or nil for
// success.
func FileExitError(code int) error {
	return fileErrors.exit(&docker.ExecInspect{ExitCode: &code})
}
