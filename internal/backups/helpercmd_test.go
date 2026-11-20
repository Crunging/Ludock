package backups

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/launch"
)

const (
	execStart   = `^POST /v[0-9.]+/exec/[^/]+/start$`
	execInspect = `^GET /v[0-9.]+/exec/[^/]+/json$`
	execCreate  = `^POST /v[0-9.]+/containers/[^/]+/exec$`
)

// commandHelper is a running helper whose commands are named by their first
// argument: "ok" prints hello, "exit N" exits N, "block" waits for release,
// and "big" prints more than 4 MiB and then waits for release. Blocking
// commands are announced on started.
type commandHelper struct {
	engine  *fakedocker.Engine
	client  *docker.Client
	helpers *Helpers
	id      string
	started chan string
	// next lets one blocked command finish; release, closed at cleanup, all.
	next, release chan struct{}
}

func newCommandHelper(t *testing.T) *commandHelper {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	client := docker.New(engine.Socket)
	h := &commandHelper{engine: engine, client: client, helpers: &Helpers{Docker: client, pollInterval: 1},
		id: engine.AddContainer(fakedocker.GameOptions{Name: "helper", Running: true}), started: make(chan string, 8),
		next: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		switch cmd[0] {
		case "ok":
			stdout([]byte("hello"))
		case "exit":
			code, _ := strconv.Atoi(cmd[1])
			return code
		case "big":
			stdout(bytes.Repeat([]byte("x"), 4*1024*1024+1))
			h.started <- cmd[0]
			h.wait()
		case "block":
			h.started <- cmd[0]
			h.wait()
		}
		return 0
	}
	return h
}

func (h *commandHelper) wait() {
	select {
	case <-h.next:
	case <-h.release:
	}
}

// finishOne lets a blocked command finish, unless cleanup already did.
func (h *commandHelper) finishOne() {
	select {
	case h.next <- struct{}{}:
	case <-h.release:
	}
}

// hangStarts leaves exec starts unanswered and reports each one's arrival.
func (h *commandHelper) hangStarts() chan struct{} {
	arrived := make(chan struct{}, 1)
	h.engine.Hang = func(request string) bool {
		if regexp.MustCompile(execStart).MatchString(request) {
			arrived <- struct{}{}
			return true
		}
		return false
	}
	return arrived
}

func (h *commandHelper) exec(ctx context.Context, cmd []string, assertAccess func() error, limit time.Duration) ([]byte, error) {
	return h.helpers.ExecWithin(ctx, h.id, cmd, assertAccess, limit)
}

// releaseAfter lets blocked commands finish once one has run for wait.
func (h *commandHelper) releaseAfter(wait time.Duration) {
	go func() {
		<-h.started
		time.Sleep(wait)
		h.finishOne()
	}()
}

// Each case a backup or restore helper command can meet. Unlike a file
// command, it outlasts its caller and its stream, and returns only once
// Docker confirms it ended or forgot it, or its time ran out and the helper
// is stopped: the next step writes to the same data.
func TestExecWithinCases(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h := newCommandHelper(t)
		if out, err := h.exec(ctx, []string{"ok"}, nil, time.Minute); err != nil || string(out) != "hello" {
			t.Fatal(string(out), err)
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		h := newCommandHelper(t)
		for _, code := range []int{2, helper.FileNameTaken} {
			if _, err := h.exec(ctx, []string{"exit", strconv.Itoa(code)}, nil, time.Minute); !apperr.Is(err, "DATA_OPERATION_FAILED") {
				t.Fatal(code, err)
			}
		}
	})
	t.Run("output over the limit waits for the command", func(t *testing.T) {
		h := newCommandHelper(t)
		h.releaseAfter(300 * time.Millisecond)
		started := time.Now()
		if _, err := h.exec(ctx, []string{"big"}, nil, time.Minute); !apperr.Is(err, "HELPER_OUTPUT") || time.Since(started) < 250*time.Millisecond {
			t.Fatal(err, time.Since(started))
		}
	})
	t.Run("access refused after create", func(t *testing.T) {
		h := newCommandHelper(t)
		refused := errors.New("refused")
		calls := 0
		if _, err := h.exec(ctx, []string{"ok"}, func() error { calls++; return refused }, time.Minute); err != refused || calls != 1 {
			t.Fatal(err, calls)
		}
	})
	t.Run("caller cancelled before create", func(t *testing.T) {
		h := newCommandHelper(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := h.exec(cancelled, []string{"ok"}, nil, time.Minute); !errors.Is(err, docker.ErrRequestFailed) {
			t.Fatal(err)
		}
	})
	t.Run("caller cancelled while running is ignored", func(t *testing.T) {
		h := newCommandHelper(t)
		cancelled, cancel := context.WithCancel(ctx)
		go func() { <-h.started; cancel(); time.Sleep(300 * time.Millisecond); h.finishOne() }()
		if _, err := h.exec(cancelled, []string{"block"}, nil, time.Minute); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("time limit passes while running stops the helper", func(t *testing.T) {
		h := newCommandHelper(t)
		if _, err := h.exec(ctx, []string{"block"}, nil, 300*time.Millisecond); !apperr.Is(err, "BACKUP_TIMEOUT") {
			t.Fatal(err)
		}
		if info, err := h.client.Inspect(ctx, h.id); err != nil || info.State.Running {
			t.Fatal("the helper still runs", err)
		}
	})
	t.Run("create never answered fails after the step limit", func(t *testing.T) {
		previous := launch.StepLimit
		launch.StepLimit = 50 * time.Millisecond
		t.Cleanup(func() { launch.StepLimit = previous })
		h := newCommandHelper(t)
		h.engine.AddFault(execCreate, 0, true, 1)
		bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if _, err := h.exec(bounded, []string{"ok"}, nil, time.Minute); err != launch.ErrDockerTimeout || bounded.Err() != nil {
			t.Fatal(err)
		}
	})
	t.Run("start provably not sent", func(t *testing.T) {
		h := newCommandHelper(t)
		_, err := h.exec(ctx, []string{"ok"}, func() error { h.engine.Refuse(200 * time.Millisecond); return nil }, time.Minute)
		if !docker.NotSent(err) {
			t.Fatal(err)
		}
	})
	t.Run("no time left", func(t *testing.T) {
		h := newCommandHelper(t)
		for _, limit := range []time.Duration{0, -time.Second} {
			if _, err := h.exec(ctx, []string{"ok"}, nil, limit); !apperr.Is(err, "BACKUP_TIMEOUT") {
				t.Fatal(limit, err)
			}
		}
	})
	t.Run("start sent and never answered", func(t *testing.T) {
		h := newCommandHelper(t)
		h.engine.HangExecStarts = 1
		if _, err := h.exec(ctx, []string{"ok"}, nil, 300*time.Millisecond); !apperr.Is(err, "BACKUP_TIMEOUT") {
			t.Fatal(err)
		}
	})
	t.Run("start refused", func(t *testing.T) {
		h := newCommandHelper(t)
		h.engine.AddFault(execStart, 500, false, 1)
		if _, err := h.exec(ctx, []string{"ok"}, nil, time.Minute); docker.StatusCode(err) != 500 {
			t.Fatal(err)
		}
	})
	t.Run("start answer lost waits for Docker", func(t *testing.T) {
		h := newCommandHelper(t)
		arrived := h.hangStarts()
		var back atomic.Int64
		go func() {
			<-arrived
			back.Store(time.Now().Add(500 * time.Millisecond).UnixNano())
			h.engine.Outage(500 * time.Millisecond)
		}()
		_, err := h.exec(ctx, []string{"ok"}, nil, time.Minute)
		if back.Load() == 0 || time.Now().UnixNano() < back.Load() {
			t.Fatal("returned before Docker came back")
		}
		var open *docker.StreamOpenError
		if !errors.As(err, &open) || !open.RequestStarted {
			t.Fatal(err)
		}
	})
	t.Run("stream cut by Docker restarting", func(t *testing.T) {
		h := newCommandHelper(t)
		// The fake forgets the exec as it cuts the stream, so the first
		// inspection may already find it unknown.
		go func() { <-h.started; h.engine.Outage(500 * time.Millisecond) }()
		_, err := h.exec(ctx, []string{"block"}, nil, time.Minute)
		if !apperr.Is(err, "DATA_OPERATION_FAILED") {
			t.Fatal(err)
		}
	})
	t.Run("inspection fails on Docker's side", func(t *testing.T) {
		h := newCommandHelper(t)
		h.engine.AddFault(execInspect, 500, false, 1)
		if out, err := h.exec(ctx, []string{"ok"}, nil, time.Minute); err != nil || string(out) != "hello" {
			t.Fatal(err)
		}
	})
	t.Run("exec unknown when inspected", func(t *testing.T) {
		h := newCommandHelper(t)
		h.engine.AddFault(execInspect, 404, false, 1)
		if _, err := h.exec(ctx, []string{"ok"}, nil, time.Minute); !apperr.Is(err, "DATA_OPERATION_FAILED") {
			t.Fatal(err)
		}
	})
}
