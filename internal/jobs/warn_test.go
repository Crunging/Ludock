package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/servers"
)

// interruptedWarning saves a restart that warned players and was waiting
// when Ludock stopped, with recovery as it was saved then, and starts a new
// runner as Ludock's next start would.
func (f *fixture) interruptedWarning(t *testing.T, phase string, recovery ops.Recovery) (*ops.Runner, string) {
	t.Helper()
	return f.interruptedWarningOf(t, "restart", phase, recovery, nil)
}

func (f *fixture) interruptedWarningOf(t *testing.T, kind, phase string, recovery ops.Recovery, input map[string]any) (*ops.Runner, string) {
	t.Helper()
	f.runner.Stop()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	input = maps.Clone(input)
	if input == nil {
		input = map[string]any{}
	}
	input[WarnInput] = true
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: kind,
		BindingRevision: server.BindingRevision, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(recovery)
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running', phase=?, recovery_json=? WHERE id=?", phase, string(encoded), job.ID); err != nil {
		t.Fatal(err)
	}
	next := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(next)
	if err := next.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Stop)
	return next, job.ID
}

// A restart that warned players and was waiting when Ludock stopped resumes
// its wait at the next start and restarts once at the time players were
// given, without warning them again.
func TestWarnedRestartResumesAfterLudockRestarts(t *testing.T) {
	f := setup(t)
	before := f.startedAt()
	until := time.Now().Add(300 * time.Millisecond).UnixMilli()
	runner, id := f.interruptedWarning(t, "warning", ops.Recovery{WarnUntil: until, WarnSent: true})
	job := finished(t, runner, id)
	if job.Status != ops.Succeeded || f.startedAt() == before || !f.running() {
		t.Fatalf("%+v", job)
	}
	if job.Recovery.WarnUntil != until || job.UpdatedAt < until {
		t.Fatal("warned again, or restarted before the time players were given", job.Recovery.WarnUntil, job.UpdatedAt-until)
	}
}

// Ludock stopping while a warned restart waits ends the wait at once and
// leaves the restart running with the time players were given, which the
// next start resumes. This fixture's console never answers, so the restart
// is still trying to send its warning, which the next start sends again.
func TestStoppingDuringAWarningLeavesItToResume(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = time.Minute
	f.runner.Stop()
	running, cancel := context.WithCancel(ctx)
	runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(runner)
	if err := runner.Start(running, nil); err != nil {
		t.Fatal(err)
	}
	before := f.startedAt()
	server, _ := f.jobs.Identity.Get(ctx, f.serverID)
	job, err := runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: server.BindingRevision, Input: map[string]any{WarnInput: true}})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if current, _ := runner.Get(ctx, job.ID); current.Recovery.WarnUntil != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the warning never started")
		}
	}
	cancel()
	stopping := time.Now()
	runner.Stop()
	// Far less than the minute's warning it would otherwise wait out.
	if waited := time.Since(stopping); waited > 20*time.Second {
		t.Fatal("stopping waited", waited)
	}
	current, _ := runner.Get(ctx, job.ID)
	if current.Status != ops.Running || current.Phase != ops.PhaseWarningPlayers || current.Recovery.WarnUntil == 0 || f.startedAt() != before {
		t.Fatalf("%+v", current)
	}
}

// A warned restart found long after its time, as after Ludock was down for
// a while, doesn't restart late and says players were told it would; one
// that already began stopping is reconciled, never resumed.
func TestWarnedRestartTooLateOrStartedIsNotResumed(t *testing.T) {
	f := setup(t)
	before := f.startedAt()
	runner, id := f.interruptedWarning(t, "warning", ops.Recovery{WarnUntil: time.Now().Add(-10 * time.Minute).UnixMilli(), WarnSent: true})
	job := finished(t, runner, id)
	if job.Status != ops.Failed || job.Error != "Ludock wasn’t running when the server was due to restart, so it didn’t. Players had been told it would." ||
		f.startedAt() != before {
		t.Fatalf("%+v", job)
	}

	f = setup(t)
	before = f.startedAt()
	runner, id = f.interruptedWarning(t, "verifying_stop", ops.Recovery{WarnUntil: time.Now().UnixMilli(), WarnSent: true, StopStarted: true, StopSettled: true})
	if job := finished(t, runner, id); job.Status != ops.Interrupted || f.startedAt() != before {
		t.Fatalf("%+v", job)
	}
}

// A failure after players were warned says so, except one the runner acts
// on, such as resources being busy, which keeps its chain so the job is
// queued again instead of failing.
func TestCalledOffKeepsRunnerErrors(t *testing.T) {
	job := &ops.Context{Job: &ops.Job{Kind: "restart", Recovery: ops.Recovery{WarnSent: true}}}
	internal := errors.New("resources busy")
	if err := calledOff(job, internal); err != internal {
		t.Fatal(err)
	}
	err := calledOff(job, errRevoked)
	if err.Error() != "The person who asked for this can no longer use the server. Players were told the server would restart, but it didn’t." {
		t.Fatal(err)
	}
}

// A warned restore starts its helpers before its warning or past its wait,
// so an interruption is recovered, which removes them, instead of resumed,
// which wouldn't.
func TestWarnedRestoreIsRecoveredOncePastItsWait(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = 50 * time.Millisecond
	backup := f.run(t, "backup", nil)
	var mu sync.Mutex
	var phases []string
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if c.CreateRequest.Labels["ludock.internal"] == "backup-helper" {
			var phase string
			f.jobs.DB.QueryRow("SELECT phase FROM operations WHERE kind='restore'").Scan(&phase)
			mu.Lock()
			phases = append(phases, phase)
			mu.Unlock()
		}
	}
	restore := f.run(t, "restore", map[string]any{"backupId": backup.Result.BackupID, "confirmation": "mc", WarnInput: true})
	mu.Lock()
	defer mu.Unlock()
	if restore.Status != ops.Succeeded || len(phases) == 0 {
		t.Fatalf("%+v", restore)
	}
	if job := (&ops.Context{Job: &ops.Job{Kind: "restore", Phase: phases[0], Input: restore.Input,
		Recovery: ops.Recovery{WarnUntil: restore.Recovery.WarnUntil}}}); resumable(job) {
		t.Fatalf("the first helper started in phase %q, which a restart of Ludock resumes", phases[0])
	}
}

// A warned restore reads who is online only after it has validated its
// archive, which can take minutes, so a player who joins meanwhile is warned.
// It reads under the locks it holds, once.
func TestWarnedRestoreWarnsAfterValidating(t *testing.T) {
	f := setup(t)
	f.jobs.WarningLead = 100 * time.Millisecond
	backup := f.run(t, "backup", nil)
	source := f.withPlayers(1)
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	source.onRead = func() { record("players read") }
	started := f.engine.OnStart
	// The first helper is the restore's check for room, after validation.
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if c.CreateRequest.Labels["ludock.internal"] == "backup-helper" {
			record("helper")
		}
	}
	restore := f.run(t, "restore", map[string]any{"backupId": backup.Result.BackupID, "confirmation": "mc", WarnInput: true})
	mu.Lock()
	defer mu.Unlock()
	reads, unheld := source.counts()
	if restore.Status != ops.Succeeded || restore.Recovery.WarnUntil == 0 || reads != 1 || unheld != 0 {
		t.Fatalf("%+v, %d readings, %d without the lock", restore, reads, unheld)
	}
	if len(events) == 0 || events[0] != "helper" {
		t.Fatal("read who is online before validating:", events)
	}
}

// onlineSource reads a set number of players online, or fails to read
// them when online is negative, counting its readings.
type onlineSource struct {
	servers *servers.Service
	mu      sync.Mutex
	online  int
	reads   int
	// unheld counts readings taken without the server's lock.
	unheld int
	// onRead, if set, runs at each reading.
	onRead func()
}

func (s *onlineSource) Lists(servers.Container) bool { return true }

func (s *onlineSource) Read(ctx context.Context, server identity.Server, ask players.Ask) (*players.Reading, error) {
	observed, _, err := s.servers.Observe(ctx, server)
	if err != nil {
		return nil, err
	}
	if s.onRead != nil {
		s.onRead()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if !ask.Held {
		s.unheld++
	}
	return &players.Reading{ContainerID: observed.Container.ID, StartedAt: observed.Container.StateSince, Failed: s.online < 0,
		Players: games.Players{Online: max(s.online, 0)}, CheckedAt: time.Now().UnixMilli()}, nil
}

func (s *onlineSource) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.unheld
}

// withPlayers gives the fixture players online, -1 for a count that can't
// be read.
func (f *fixture) withPlayers(online int) *onlineSource {
	source := &onlineSource{servers: f.jobs.Servers, online: online}
	f.jobs.Players = &players.Service{Servers: f.jobs.Servers, Identity: f.jobs.Identity, Source: source}
	return source
}

// A restart, backup or update asked to warn players reads who is online
// when it reaches its warning, whatever the page showed; an update reads
// under the server lock it already holds: with someone on, or a count it can't
// read, it warns and waits; with nobody on, it goes ahead at once and says
// so. The panel's reading from moments ago of someone online is used
// rather than taken again; one of nobody isn't, since someone may have
// joined since.
func TestWarningReadsPlayersWhenItRuns(t *testing.T) {
	for _, kind := range []string{"restart", "backup", "update"} {
		for _, c := range []struct {
			name   string
			online int
			warns  bool
		}{{"someone", 1, true}, {"nobody", 0, false}, {"unknown", -1, true}} {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				f := setup(t)
				// Long enough that work that skips the wait can't be mistaken
				// for one that waited, however slow the machine.
				f.jobs.WarningLead = 300 * time.Millisecond
				if !c.warns {
					f.jobs.WarningLead = 30 * time.Second
				}
				source := f.withPlayers(c.online)
				started := time.Now()
				input := map[string]any{WarnInput: true}
				if kind == "update" {
					input["request"] = skipBackup["request"]
				}
				job := f.run(t, kind, input)
				waited := time.Since(started) >= f.jobs.WarningLead
				reads, unheld := source.counts()
				if job.Status != ops.Succeeded || reads != 1 || unheld != 0 {
					t.Fatalf("%+v, %d readings, %d without the lock", job, reads, unheld)
				}
				if c.warns {
					if job.Recovery.WarnUntil == 0 || job.Recovery.WarnSkipped || !waited || job.Result.WarnSkipped {
						t.Fatalf("didn't warn and wait: %+v", job)
					}
					return
				}
				if job.Recovery.WarnUntil != 0 || job.Recovery.WarnSent || !job.Recovery.WarnSkipped || waited || !job.Result.WarnSkipped {
					t.Fatalf("warned or waited with nobody online: %+v", job)
				}
			})
		}
	}

	for _, c := range []struct {
		online, reads int
	}{{1, 1}, {0, 2}} {
		f := setup(t)
		f.jobs.WarningLead = 100 * time.Millisecond
		source := f.withPlayers(c.online)
		server, _ := f.jobs.Identity.Get(ctx, f.serverID)
		f.jobs.Players.ReadHeld(ctx, *server)
		if job := f.run(t, "restart", map[string]any{WarnInput: true}); job.Status != ops.Succeeded || job.Recovery.WarnSkipped == (c.online > 0) {
			t.Fatalf("%+v", job)
		}
		if reads, _ := source.counts(); reads != c.reads {
			t.Fatal(c.online, "online:", reads, "readings")
		}
	}
}

// A restart or backup that warned players and was waiting when Ludock
// stopped waits out the time players were given after the next start, even
// if nobody is online by then: a warning sent is never taken back.
func TestResumedWarningIgnoresAnEmptyServer(t *testing.T) {
	for _, kind := range []string{"restart", "backup", "restore"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			var input map[string]any
			if kind == "restore" {
				backup := f.run(t, "backup", nil)
				input = map[string]any{"backupId": backup.Result.BackupID, "confirmation": "mc"}
			}
			source := f.withPlayers(0)
			until := time.Now().Add(300 * time.Millisecond).UnixMilli()
			runner, id := f.interruptedWarningOf(t, kind, ops.PhaseWarning, ops.Recovery{WarnUntil: until, WarnSent: true}, input)
			job := finished(t, runner, id)
			if reads, _ := source.counts(); job.Status != ops.Succeeded || job.Recovery.WarnSkipped || reads != 0 ||
				job.Recovery.WarnUntil != until || job.UpdatedAt < until {
				t.Fatalf("%+v, %d readings", job, reads)
			}
		})
	}
}
