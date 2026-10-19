package launch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
)

// MaxOutput bounds buffered helper output.
const MaxOutput = 4 * 1024 * 1024

// Errors from helper commands. Messages are public.
var (
	ErrTimeout     = apperr.New("FILE_OPERATION_TIMEOUT", 409, "The file operation timed out. Refresh and try again.")
	ErrFailed      = apperr.New("FILE_OPERATION_FAILED", 409, "File operation failed")
	ErrUnsafe      = apperr.New("UNSAFE_OR_CHANGED_FILE_PATH", 409, "The path changed, is unsafe, or cannot be accessed")
	ErrOutputLimit = apperr.New("FILE_OUTPUT_LIMIT", 409, "File operation failed")
	ErrUpload      = apperr.New("FILE_UPLOAD_FAILED", 400, "File operation failed")
)

// ExecOptions configures one helper command.
type ExecOptions struct {
	Cmd []string
	// Input is streamed to stdin; Trailer follows it only after the whole
	// input arrived and every access check passed, so the helper commits the
	// upload only then.
	Input   io.Reader
	Trailer []byte
	// AssertAccess runs before the exec, after it is created, and between
	// input chunks.
	AssertAccess func() error
}

// limitedBuffer does not embed bytes.Buffer, whose ReadFrom would bypass
// the limit inside io.Copy.
type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > MaxOutput {
		return 0, ErrOutputLimit
	}
	return b.buffer.Write(p)
}

// Exec runs a helper command in a helper container and returns its stdout.
// ctx cancellation stops the command: commands without input are cut off
// immediately, uploads get five seconds to remove their staging file.
func Exec(ctx context.Context, client *docker.Client, container string, options ExecOptions) ([]byte, error) {
	check := func() error {
		if options.AssertAccess != nil {
			return options.AssertAccess()
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	execID, err := client.CreateExec(ctx, container, docker.ExecCreate{Cmd: options.Cmd, AttachStdout: true,
		AttachStderr: true, AttachStdin: options.Input != nil})
	if err != nil {
		return nil, err
	}
	if err := check(); err != nil {
		return nil, err
	}
	limit := time.Minute
	if options.Input != nil {
		limit = 30 * time.Minute
	}
	// Docker can accept the connection and never answer it; the deadline and
	// cancellation also end the opening so the caller can remove the helper
	// and release its locks.
	deadline, cancelDeadline := context.WithTimeout(ctx, limit)
	defer cancelDeadline()
	conn, err := client.StartExec(deadline, execID)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if deadline.Err() != nil {
			return nil, ErrTimeout
		}
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
			fail(ErrTimeout)
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

	written := make(chan struct{})
	if options.Input == nil {
		close(written)
	} else {
		go func() {
			defer close(written)
			buffer := make([]byte, 64*1024)
			for {
				n, readErr := options.Input.Read(buffer)
				if current() != nil {
					break
				}
				if err := check(); err != nil {
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
					if len(options.Trailer) > 0 && current() == nil {
						if _, err := conn.Write(options.Trailer); err != nil {
							fail(ErrUpload)
						}
					}
					break
				}
				if readErr != nil {
					fail(ErrUpload)
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
	demuxErr := docker.Demux(conn, &stdout, nil)
	// The helper only succeeds after the whole input and its trailer. An
	// input still being read means it already failed; the caller's body
	// unblocks once the request ends, so do not wait for it here.
	select {
	case <-written:
	default:
		fail(ErrUpload)
	}
	if err := current(); err != nil {
		return nil, err
	}
	if demuxErr != nil {
		if errors.Is(demuxErr, ErrOutputLimit) {
			return nil, ErrOutputLimit
		}
		return nil, ErrFailed
	}
	info, err := client.InspectExec(context.WithoutCancel(ctx), execID)
	if err != nil {
		return nil, err
	}
	if info.Running || info.ExitCode == nil || *info.ExitCode != 0 {
		return nil, ErrUnsafe
	}
	return stdout.buffer.Bytes(), nil
}
