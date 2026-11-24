package launch

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
)

// scripted runs scripted commands until the test ends.
func scripted(t *testing.T, engine *fakedocker.Engine) *fakedocker.Scripted {
	s := engine.Script(MaxOutput + 1)
	// Registered after the engine, so blocked commands end before it closes.
	t.Cleanup(func() { close(s.Release) })
	return s
}

// Each case a file command can meet, as the file pages see it: the command's
// own outcome, the caller or the time limit ending it, and Docker failing at
// each step. A file command never waits for or stops a command it gave up
// on; the file access that ran it removes its helper.
func TestExecCases(t *testing.T) {
	run := func(t *testing.T, cmd []string, options ExecOptions) (*fakedocker.Engine, *fakedocker.Scripted, []byte, error) {
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
	t.Run("exit recorded after the stream ends", func(t *testing.T) {
		previous := ExitWait
		ExitWait = 300 * time.Millisecond
		t.Cleanup(func() { ExitWait = previous })
		engine, client, id := setup(t)
		scripted(t, engine)
		engine.ExitDelay = 150 * time.Millisecond
		if out, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); err != nil || string(out) != "hello" {
			t.Fatal(string(out), err)
		}
		// Still running once the wait is over is uncertain.
		engine.ExitDelay = time.Second
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); !errors.Is(err, ErrUnsafe) {
			t.Fatal(err)
		}
	})
	t.Run("output over the limit returns without waiting", func(t *testing.T) {
		_, s, _, err := run(t, []string{"big"}, ExecOptions{})
		if !errors.Is(err, ErrOutputLimit) || !s.Unfinished() {
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
			if err != refused || !s.Unfinished() {
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
		go func() { <-s.Started; cancel() }()
		if _, err := Exec(cancelled, client, id, ExecOptions{Cmd: []string{"block"}}); err != context.Canceled || !s.Unfinished() {
			t.Fatal(err)
		}
	})
	t.Run("time limit passes while running leaves it running", func(t *testing.T) {
		engine, client, id := setup(t)
		s := scripted(t, engine)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"block"}, Limit: 300 * time.Millisecond}); err != ErrTimeout || !s.Unfinished() {
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
		engine.AddFault(fakedocker.ExecCreate, 0, true, 1)
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
		engine.AddFault(fakedocker.ExecStart, 500, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); docker.StatusCode(err) != 500 {
			t.Fatal(err)
		}
	})
	t.Run("start answer lost returns at once", func(t *testing.T) {
		engine, client, id := setup(t)
		arrived := engine.HangRequests(fakedocker.ExecStart)
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
			go func() { <-s.Started; engine.Outage(time.Second); close(outage) }()
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
		engine.AddFault(fakedocker.ExecInspect, 500, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); docker.StatusCode(err) != 500 {
			t.Fatal(err)
		}
		engine.AddFault(fakedocker.ExecInspect, 500, false, 1)
		if out, err := Exec(docker.RideOut(ctx, nil), client, id, ExecOptions{Cmd: []string{"ok"}}); err != nil || string(out) != "hello" {
			t.Fatal(err)
		}
	})
	t.Run("exec unknown when inspected", func(t *testing.T) {
		engine, client, id := setup(t)
		scripted(t, engine)
		engine.AddFault(fakedocker.ExecInspect, 404, false, 1)
		if _, err := Exec(ctx, client, id, ExecOptions{Cmd: []string{"ok"}}); !docker.IsNotFound(err) {
			t.Fatal(err)
		}
	})
}
