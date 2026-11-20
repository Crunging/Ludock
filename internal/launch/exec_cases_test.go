package launch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

const (
	execStart   = `^POST /v[0-9.]+/exec/[^/]+/start$`
	execInspect = `^GET /v[0-9.]+/exec/[^/]+/json$`
	execCreate  = `^POST /v[0-9.]+/containers/[^/]+/exec$`
)

// scriptedCommands runs commands named by their first argument: "ok" prints
// hello, "exit N" exits N, "block" waits for release, and "big" prints more
// than MaxOutput and then waits for release. Started blocking commands are
// announced on started; finished reports each command's end.
type scriptedCommands struct {
	started, finished chan string
	release           chan struct{}
}

func scripted(t *testing.T, engine *fakedocker.Engine) *scriptedCommands {
	s := &scriptedCommands{started: make(chan string, 8), finished: make(chan string, 8), release: make(chan struct{})}
	// Registered after the engine, so blocked commands end before it closes.
	t.Cleanup(func() { close(s.release) })
	engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		defer func() { s.finished <- cmd[0] }()
		switch cmd[0] {
		case "ok":
			stdout([]byte("hello"))
		case "exit":
			code, _ := strconv.Atoi(cmd[1])
			return code
		case "big":
			stdout(bytes.Repeat([]byte("x"), MaxOutput+1))
			s.started <- cmd[0]
			<-s.release
		case "block":
			s.started <- cmd[0]
			<-s.release
		}
		return 0
	}
	return s
}

func (s *scriptedCommands) unfinished() bool {
	select {
	case <-s.finished:
		return false
	default:
		return true
	}
}

// Each case a file command can meet, as the file pages see it: the command's
// own outcome, the caller or the time limit ending it, and Docker failing at
// each step. A file command never waits for or stops a command it gave up
// on; the file access that ran it removes its helper.
func TestExecCases(t *testing.T) {
	run := func(t *testing.T, cmd []string, options ExecOptions) (*fakedocker.Engine, *scriptedCommands, []byte, error) {
		engine, client, id := setup(t)
		s := scripted(t, engine)
		options.Cmd = cmd
		out, err := Exec(ctx, client, id, options)
		return engine, s, out, err
	}
	t.Run("success", func(t *testing.T) {
		if _, _, out, err := run(t, []string{"ok"}, ExecOptions{}); err != nil || string(out) != "hello" {
			t.Fatal(string(out), err)
		}
	})
	t.Run("non-zero exit", func(t *testing.T) {
		if _, _, _, err := run(t, []string{"exit", "2"}, ExecOptions{}); !errors.Is(err, ErrUnsafe) {
			t.Fatal(err)
		}
		if _, _, _, err := run(t, []string{"exit", strconv.Itoa(helper.FileNameTaken)}, ExecOptions{}); !errors.Is(err, ErrNameTaken) {
			t.Fatal(err)
		}
		if _, _, _, err := run(t, []string{"exit", strconv.Itoa(helper.FileArchiveFull)}, ExecOptions{}); !apperr.Is(err, "ARCHIVE_NO_SPACE") {
			t.Fatal(err)
		}
	})
	t.Run("output over the limit returns without waiting", func(t *testing.T) {
		_, s, _, err := run(t, []string{"big"}, ExecOptions{})
		if !errors.Is(err, ErrOutputLimit) || !s.unfinished() {
			t.Fatal(err)
		}
	})
	t.Run("access refused before or after create", func(t *testing.T) {
		refused := errors.New("refused")
		for refuseAt := 1; refuseAt <= 2; refuseAt++ {
			calls := 0
			_, s, _, err := run(t, []string{"ok"}, ExecOptions{AssertAccess: func() error {
				if calls++; calls == refuseAt {
					return refused
				}
				return nil
			}})
			if err != refused || !s.unfinished() {
				t.Fatal(refuseAt, err)
			}
		}
	})
	t.Run("caller cancelled before create", func(t *testing.T) {
		_, client, id := setup(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := Exec(cancelled, client, id, ExecOptions{Cmd: []string{"ok"}}); err != context.Canceled {
			t.Fatal(err)
		}
	})
	t.Run("caller cancelled while running leaves it running", func(t *testing.T) {
		engine, client, id := setup(t)
		s := scripted(t, engine)
		cancelled, cancel := context.WithCancel(ctx)
		go func() { <-s.started; cancel() }()
		if _, err := Exec(cancelled, client, id, ExecOptions{Cmd: []string{"block"}}); err != context.Canceled || !s.unfinished() {
			t.Fatal(err)
		}
	})
	t.Run("time limit passes while running leaves it running", func(t *testing.T) {
		engine, client, id := setup(t)
		s := scripted(t, engine)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"block"}, Limit: 300 * time.Millisecond}); err != ErrTimeout || !s.unfinished() {
			t.Fatal(err)
		}
		if info, err := client.Inspect(ctx, id); err != nil || !info.State.Running {
			t.Fatal("the helper was stopped", err)
		}
	})
	t.Run("create never answered", func(t *testing.T) {
		previous := StepLimit
		StepLimit = 200 * time.Millisecond
		t.Cleanup(func() { StepLimit = previous })
		engine, client, id := setup(t)
		engine.AddFault(execCreate, 0, true, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); err != ErrDockerTimeout {
			t.Fatal(err)
		}
	})
	t.Run("start provably not sent", func(t *testing.T) {
		engine, client, id := setup(t)
		scripted(t, engine)
		calls := 0
		_, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}, AssertAccess: func() error {
			if calls++; calls == 2 {
				engine.Refuse(200 * time.Millisecond)
			}
			return nil
		}})
		if !docker.NotSent(err) {
			t.Fatal(err)
		}
	})
	t.Run("start sent and never answered", func(t *testing.T) {
		engine, client, id := setup(t)
		engine.HangExecStarts = 1
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}, Limit: 300 * time.Millisecond}); err != ErrTimeout {
			t.Fatal(err)
		}
	})
	t.Run("start refused", func(t *testing.T) {
		engine, client, id := setup(t)
		engine.AddFault(execStart, 500, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); docker.StatusCode(err) != 500 {
			t.Fatal(err)
		}
	})
	t.Run("start answer lost returns at once", func(t *testing.T) {
		engine, client, id := setup(t)
		arrived := make(chan struct{}, 1)
		engine.Hang = func(request string) bool {
			if regexp.MustCompile(execStart).MatchString(request) {
				arrived <- struct{}{}
				return true
			}
			return false
		}
		outage := make(chan struct{})
		go func() { <-arrived; engine.Outage(time.Second); close(outage) }()
		_, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}})
		var open *docker.StreamOpenError
		select {
		case <-outage:
			t.Fatal("waited for Docker to come back")
		default:
		}
		if !errors.As(err, &open) || !open.RequestStarted {
			t.Fatal(err)
		}
		<-outage
	})
	t.Run("stream cut by Docker restarting", func(t *testing.T) {
		for _, rideOut := range []bool{false, true} {
			engine, client, id := setup(t)
			s := scripted(t, engine)
			outage := make(chan struct{})
			go func() { <-s.started; engine.Outage(time.Second); close(outage) }()
			commandCtx := ctx
			if rideOut {
				commandCtx = docker.RideOut(ctx, nil)
			}
			_, err := Exec(commandCtx, client, id, ExecOptions{Cmd: []string{"block"}})
			select {
			case <-outage:
				if !rideOut {
					t.Fatal("waited for Docker to come back")
				}
			default:
			}
			// The single inspection finds Docker unreachable, or the exec already
			// forgotten; riding it out, the restarted Docker no longer knows it.
			if _, public := apperr.As(err); public || err == nil || (!docker.Unavailable(err) && !docker.IsNotFound(err)) ||
				(rideOut && !docker.IsNotFound(err)) {
				t.Fatal(rideOut, err)
			}
			<-outage
		}
	})
	t.Run("inspection fails on Docker's side", func(t *testing.T) {
		engine, client, id := setup(t)
		scripted(t, engine)
		engine.AddFault(execInspect, 500, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); docker.StatusCode(err) != 500 {
			t.Fatal(err)
		}
		engine.AddFault(execInspect, 500, false, 1)
		if out, err := Exec(docker.RideOut(ctx, nil), client, id, ExecOptions{Cmd: []string{"ok"}}); err != nil || string(out) != "hello" {
			t.Fatal(err)
		}
	})
	t.Run("exec unknown when inspected", func(t *testing.T) {
		engine, client, id := setup(t)
		scripted(t, engine)
		engine.AddFault(execInspect, 404, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); !docker.IsNotFound(err) {
			t.Fatal(err)
		}
	})
}
