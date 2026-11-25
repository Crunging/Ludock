package jobs

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
)

// serverTrace records, in order, what reached the game: its console's
// commands and Docker's requests to stop, start, or replace it.
type serverTrace struct {
	f        *fixture
	mu       sync.Mutex
	from     int
	commands []tracedCommand
}

type tracedCommand struct {
	// at is how many Docker requests came before it.
	at   int
	text string
}

// consoleTrace gives the fixture's game an RCON console that records every
// command it receives into the trace returned.
func (f *fixture) consoleTrace(t *testing.T) *serverTrace {
	t.Helper()
	trace := &serverTrace{f: f}
	port, err := fakedocker.ServeRCON("rcon-secret", func(command string) string {
		at := len(trace.requests())
		trace.mu.Lock()
		trace.commands = append(trace.commands, tracedCommand{at, strings.Fields(command)[0]})
		trace.mu.Unlock()
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.Config.Labels["ludock.console.host"], c.Config.Labels["ludock.console.port"] = "127.0.0.1", fmt.Sprint(port)
		c.Config.Env = append(c.Config.Env, "RCON_PASSWORD=rcon-secret")
	})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	return trace
}

// requests reads the fake daemon's requests under its lock, which Update
// on Ludock's own container, never removed, takes.
func (s *serverTrace) requests() []string {
	var requests []string
	s.f.engine.Update("ludock", func(*fakedocker.Container) { requests = slices.Clone(s.f.engine.Requests) })
	return requests
}

// start begins the trace at the work about to run.
func (s *serverTrace) start() {
	from := len(s.requests())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.commands = from, nil
}

// lines reads the trace as "console <command>", "stop" and "start" of the
// server, "work" for the first container created after the stop, such as
// the helper that copies the data or the update's replacement, and the
// replacement's "start replacement" and "rename".
func (s *serverTrace) lines() []string {
	requests := s.requests()[s.from:]
	s.mu.Lock()
	defer s.mu.Unlock()
	renamed := map[string]bool{}
	for _, request := range requests {
		if container, ok := strings.CutSuffix(request, "/rename"); ok {
			renamed[path.Base(container)] = true
		}
	}
	var lines []string
	add := func(line string) {
		if len(lines) == 0 || lines[len(lines)-1] != line {
			lines = append(lines, line)
		}
	}
	commands := s.commands
	for index, request := range requests {
		for len(commands) > 0 && commands[0].at <= s.from+index {
			add("console " + commands[0].text)
			commands = commands[1:]
		}
		method, target, _ := strings.Cut(request, " ")
		if method != "POST" {
			continue
		}
		container, action := path.Base(path.Dir(target)), path.Base(target)
		switch {
		case container == s.f.game && (action == "stop" || action == "start"):
			add(action)
		case renamed[container] && (action == "start" || action == "rename"):
			add(action + " replacement")
		case action == "create" && slices.Contains(lines, "stop") && !slices.Contains(lines, "work"):
			add("work")
		}
	}
	for _, command := range commands {
		add("console " + command.text)
	}
	return lines
}

// A game that keeps exiting under its restart policy leaves Docker showing
// its container restarting, between runs too short for anyone to join.
// Stop, and each piece of work that stops the server, treat it as a server
// they can stop: nobody is warned and no save is sent, since no game is
// there to hear either, and Activity says the game couldn't be asked to
// save. The server is stopped, the work done, and the server started again
// as it was, under its restart policy, which Activity says goes on
// restarting it unless the work fixed the cause. Docker keeps the game's
// last exit, a kill here, which isn't taken for a forced stop. Work that
// starts during one of the loop's brief runs, once Ludock has seen it
// restarting and before the game has answered, is the same. A running server is warned and saved first, as the first row
// shows.
func TestWorkOnARestartingServer(t *testing.T) {
	warned := map[string]any{WarnInput: true}
	withWarning := func(input map[string]any) map[string]any {
		input[WarnInput] = true
		return input
	}
	for _, c := range []struct {
		name, kind string
		input      func(*testing.T, *fixture) map[string]any
		// running: a server that runs normally; brief: one caught in a run
		// between restarts; stale: one whose restart Ludock last saw long
		// before that run, which counts as running normally.
		running, brief, stale bool
		trace                 []string
	}{
		{name: "running backup", kind: ops.KindBackup, running: true, trace: []string{"console say", "console save-all", "stop", "work", "start"}},
		{name: "stop", kind: ops.KindStop, trace: []string{"stop"}},
		{name: "backup", kind: ops.KindBackup, trace: []string{"stop", "work", "start"}},
		{name: "backup in a brief run", kind: ops.KindBackup, brief: true, trace: []string{"stop", "work", "start"}},
		{name: "stop in a brief run", kind: ops.KindStop, brief: true, trace: []string{"stop"}},
		{name: "backup in a run long after a restart was seen", kind: ops.KindBackup, brief: true, stale: true,
			trace: []string{"console say", "console save-all", "stop", "work", "start"}},
		{name: "restart with a backup", kind: ops.KindRestart, input: func(*testing.T, *fixture) map[string]any {
			return withWarning(map[string]any{"createBackup": true})
		}, trace: []string{"stop", "work", "start"}},
		{name: "restore", kind: ops.KindRestore, input: func(t *testing.T, f *fixture) map[string]any {
			return withWarning(map[string]any{"backupId": f.run(t, ops.KindBackup, nil).Result.BackupID, "confirmation": "mc"})
		}, trace: []string{"stop", "work", "start"}},
		{name: "undo a restore", kind: ops.KindUndoRestore, input: func(t *testing.T, f *fixture) map[string]any {
			f.stoppedRestore(t)
			return warned
		}, trace: []string{"stop", "work", "start"}},
		{name: "update", kind: ops.KindUpdate, input: func(_ *testing.T, f *fixture) map[string]any {
			f.engine.AddImage("itzg/minecraft-server")
			return withWarning(map[string]any{"request": map[string]any{"skipBackupConfirmation": "mc", "forceRecreate": true}})
		}, trace: []string{"stop", "work", "start replacement", "rename replacement"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.jobs.WarningLead = time.Second
			trace := f.consoleTrace(t)
			input := warned
			if c.input != nil {
				input = c.input(t, f)
			}
			if !c.running {
				f.engine.CrashLoop(f.game, false)
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.ExitCode = 137 })
			}
			if c.brief {
				if c.stale {
					f.jobs.Servers.Observer.Now = func() time.Time { return time.Now().Add(-time.Hour) }
				}
				if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
					t.Fatal(err)
				}
				f.jobs.Servers.Observer.Now = nil
				f.engine.CrashLoop(f.game, true)
			}
			trace.start()
			job := f.run(t, c.kind, input)
			if got := trace.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.trace, "\n"))
			}
			result := job.Result
			if job.Status != ops.Succeeded || result.ForceStopped || result.WarnFailedNote != "" {
				t.Fatalf("%s %q %+v", job.Status, job.Error, result)
			}
			if c.running || c.stale {
				return
			}
			if job.Recovery.WarnUntil != 0 || !result.WarnSkipped || result.SaveStatus != "skipped" || result.SaveReason != "restarting" {
				t.Errorf("warned or saved: %+v", result)
			}
			if running := f.containerNamed("mc").State.Running; running != (c.kind != ops.KindStop) ||
				(c.kind != ops.KindStop && !strings.Contains(result.Guidance, "goes on restarting")) {
				t.Errorf("running %v, guidance %q", running, result.Guidance)
			}
			if c.kind == ops.KindUndoRestore && !strings.Contains(result.UndoNote, "back in place") {
				t.Errorf("not undone: %q", result.UndoNote)
			}
		})
	}

	// Docker started another run just before the stop reached it, and had to
	// kill that one: a forced stop, which a plain stop says and after which a
	// backup goes no further.
	for _, kind := range []string{ops.KindStop, ops.KindBackup} {
		f := setup(t)
		f.engine.CrashLoop(f.game, false)
		f.engine.OnStop = func(c *fakedocker.Container) {
			if c.ID == f.game {
				f.engine.Update(c.ID, func(c *fakedocker.Container) {
					c.State.StartedAt, c.State.ExitCode = time.Now().UTC().Format(time.RFC3339Nano), 137
				})
			}
		}
		job := f.run(t, kind, nil)
		if kind == ops.KindStop && (job.Status != ops.Succeeded || !job.Result.ForceStopped) {
			t.Fatalf("stop: %s %+v", job.Status, job.Result)
		}
		if kind == ops.KindBackup && (job.Status != ops.Failed || !strings.Contains(job.Error, "forced to stop")) {
			t.Fatalf("backup: %s %q", job.Status, job.Error)
		}
	}
}
