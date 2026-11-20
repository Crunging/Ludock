package jobs

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

// restoreTrace records each restore helper command as it starts, with the
// journal saved at that moment, as "operation root: status phases".
type restoreTrace struct {
	mu    sync.Mutex
	steps []string
}

func (r *restoreTrace) lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

// traceRestore records restore commands; after, when set, runs once each
// command has run and returns the exit status the restore sees.
func (f *fixture) traceRestore(after func(c *fakedocker.Container, operation, root string, code int) int) *restoreTrace {
	trace := &restoreTrace{}
	run := f.engine.Exec
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if len(cmd) < 4 || cmd[0] != helper.Binary || (cmd[2] != "restore" && cmd[2] != "restore-extract") {
			return run(c, cmd, stdin, stdout, stderr)
		}
		var request struct{ Operation, Root, Stage string }
		json.Unmarshal([]byte(cmd[3]), &request)
		if cmd[2] == "restore-extract" {
			request.Operation = "extract"
		}
		// A single root's helper path ends in its generated ID.
		root := filepath.Base(request.Root)
		if strings.HasPrefix(root, "root-") {
			root = "game"
		}
		trace.mu.Lock()
		trace.steps = append(trace.steps, request.Operation+" "+root+": "+f.savedJournal(strings.TrimPrefix(request.Stage, helper.StagePrefix)))
		trace.mu.Unlock()
		code := run(c, cmd, stdin, stdout, stderr)
		if after != nil {
			code = after(c, request.Operation, root, code)
		}
		return code
	}
	return trace
}

// savedJournal reads an operation's saved restore journal as "status
// phases", with "unsafe" when the data is marked unsafe.
func (f *fixture) savedJournal(id string) string {
	var encoded string
	if err := f.jobs.DB.QueryRow("SELECT recovery_json FROM operations WHERE id=?", id).Scan(&encoded); err != nil {
		return "<" + err.Error() + ">"
	}
	var recovery ops.Recovery
	json.Unmarshal([]byte(encoded), &recovery)
	if len(recovery.Restore) == 0 {
		return "none"
	}
	var state restoreRecovery
	json.Unmarshal(recovery.Restore, &state)
	var phases []string
	for _, entry := range state.Roots {
		phases = append(phases, entry.Phase)
	}
	out := state.Status
	if len(phases) > 0 {
		out += " " + strings.Join(phases, ",")
	}
	if recovery.DataUnsafe {
		out += " unsafe"
	}
	return out
}

// once returns an after hook that runs act after the first command matching
// operation and root, keeping every exit status otherwise.
func once(operation, root string, act func(c *fakedocker.Container, code int) int) func(*fakedocker.Container, string, string, int) int {
	done := false
	var mu sync.Mutex
	return func(c *fakedocker.Container, op, r string, code int) int {
		mu.Lock()
		defer mu.Unlock()
		if done || op != operation || r != root {
			return code
		}
		done = true
		return act(c, code)
	}
}

func (f *fixture) startElsewhere() {
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.State.Running, c.State.Status, c.State.StartedAt = true, "running", time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	})
}

func (f *fixture) revoke(t *testing.T) {
	if err := f.jobs.DB.UpdateUserAccess(ctx, "admin", db.RoleMember, false); err != nil {
		t.Error(err)
	}
}

// sortedExtracts orders each run of extract lines, which run side by side.
func sortedExtracts(lines []string) []string {
	for start := 0; start < len(lines); start++ {
		end := start
		for end < len(lines) && strings.HasPrefix(lines[end], "extract ") {
			end++
		}
		slices.Sort(lines[start:end])
		start = end
	}
	return lines
}

type restoreScenario struct {
	name  string
	item  bool
	after func(*testing.T, *fixture) afterHook
	// status and journal describe the job when it ends.
	status, journal string
	// left says a stage folder is left behind; restored, that the backup's
	// data is live.
	left, restored bool
	trace          []string
}

type afterHook = func(*fakedocker.Container, string, string, int) int

func failedAt(operation, root string) func(*testing.T, *fixture) afterHook {
	return func(*testing.T, *fixture) afterHook {
		return once(operation, root, func(*fakedocker.Container, int) int { return 1 })
	}
}

func startedAfter(operation, root string) func(*testing.T, *fixture) afterHook {
	return func(t *testing.T, f *fixture) afterHook {
		return once(operation, root, func(_ *fakedocker.Container, code int) int { f.startElsewhere(); return code })
	}
}

func revokedAfter(operation, root string) func(*testing.T, *fixture) afterHook {
	return func(t *testing.T, f *fixture) afterHook {
		return once(operation, root, func(_ *fakedocker.Container, code int) int { f.revoke(t); return code })
	}
}

// Each restore helper command starts only once the journal a restart would
// read covers it, and each check of the stopped server and of the
// requester's access stops the restore before the step it guards. A failure
// rolls back newest root first, in its own journaled steps, without access
// checks; cleanup after the commit doesn't check access either. A server
// started elsewhere stops the rollback and is left as found. Traces show each
// command with the journal saved as it started.
func TestRestoreJournalLeadsEachStep(t *testing.T) {
	checked := []string{"space world: none", "space config: none",
		"space world: staging staging,staging", "space config: staging staging,staging",
		"stage world: staging staging,staging", "stage config: staging staging,staging"}
	extracted := slices.Concat(checked, []string{"extract config: staging staging,staging", "extract world: staging staging,staging"})
	worldSwapped := slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
		"moveNew world: replacing old_moved,staging unsafe"})
	swapped := slices.Concat(worldSwapped, []string{"moveOld config: replacing replaced,moving_old unsafe",
		"moveNew config: replacing replaced,old_moved unsafe"})
	committed := []string{"cleanup world: committed replaced,replaced", "cleanup config: committed replaced,replaced"}
	unstaged := []string{"cleanup config: rolling_back staging,staging unsafe", "cleanup world: rolling_back staging,staging unsafe"}
	rolledBack := func(configPhase string) []string {
		return []string{"rollbackClean config: rolling_back replaced," + configPhase + " unsafe",
			"rollbackOld config: rolling_back replaced,rolling_back unsafe",
			"cleanup config: rolling_back replaced,rolled_back unsafe",
			"rollbackClean world: rolling_back replaced,rolled_back unsafe",
			"rollbackOld world: rolling_back rolling_back,rolled_back unsafe",
			"cleanup world: rolling_back rolled_back,rolled_back unsafe"}
	}
	worldRolledBack := []string{"cleanup config: rolling_back old_moved,staging unsafe",
		"rollbackClean world: rolling_back old_moved,staging unsafe",
		"rollbackOld world: rolling_back rolling_back,staging unsafe",
		"cleanup world: rolling_back rolled_back,staging unsafe"}
	itemSwapped := []string{"space game: none", "check game: none", "space game: staging staging", "stage game: staging staging",
		"extract game: staging staging", "moveOld game: replacing moving_old unsafe", "moveNew game: replacing old_moved unsafe"}
	scenarios := []restoreScenario{
		{name: "succeeds", status: ops.Succeeded, journal: "cleaned", restored: true, trace: slices.Concat(swapped, committed)},
		{name: "item succeeds", item: true, status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(itemSwapped, []string{"cleanup game: committed replaced"})},
		{name: "item swap fails", item: true, after: failedAt("moveNew", "game"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(itemSwapped, []string{"rollbackClean game: rolling_back old_moved unsafe",
				"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe"})},
		// Without a helper nothing was staged, so there is nothing to roll back.
		{name: "item helper not created", item: true, status: ops.Failed, journal: "staging staging",
			after: func(t *testing.T, f *fixture) afterHook {
				return once("check", "game", func(_ *fakedocker.Container, code int) int {
					f.engine.AddFault("POST .*/containers/create$", 500, false, 1)
					return code
				})
			}, trace: []string{"space game: none", "check game: none"}},
		{name: "stage fails", after: failedAt("stage", "config"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(checked, unstaged)},
		{name: "extract fails", after: failedAt("extract", "world"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(extracted, unstaged)},
		// An interrupted move out leaves current data in place, which the
		// rollback must not remove.
		{name: "move out fails", after: failedAt("moveOld", "world"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"cleanup config: rolling_back moving_old,staging unsafe", "rollbackOld world: rolling_back rolling_back,staging unsafe",
				"cleanup world: rolling_back rolled_back,staging unsafe"})},
		{name: "first move in fails", after: failedAt("moveNew", "world"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(worldSwapped, worldRolledBack)},
		{name: "last move in fails", after: failedAt("moveNew", "config"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(swapped, rolledBack("old_moved"))},
		// A failed command that took its helper with it rolls back through a
		// new one.
		{name: "helper exits", status: ops.Failed, journal: "rolled_back", trace: slices.Concat(worldSwapped, worldRolledBack),
			after: func(t *testing.T, f *fixture) afterHook {
				return once("moveNew", "world", func(c *fakedocker.Container, _ int) int { f.engine.Exit(c.ID, 1); return 1 })
			}},
		{name: "cleanup fails once", after: failedAt("cleanup", "world"), status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(swapped, []string{"cleanup world: committed replaced,replaced"}, committed)},
		{name: "started after staging", after: startedAfter("stage", "config"), status: ops.Failed, journal: "rolling_back staging,staging unsafe",
			left: true, trace: checked},
		{name: "started after unpacking", after: startedAfter("extract", "config"), status: ops.Failed, journal: "rolling_back staging,staging unsafe",
			left: true, trace: extracted},
		// The stopped server is checked before each root's swap, not between
		// moving its data out and in.
		{name: "started after a move out", after: startedAfter("moveOld", "world"), status: ops.Failed, journal: "rolling_back replaced,staging unsafe",
			left: true, restored: true, trace: worldSwapped},
		{name: "started before commit", after: startedAfter("moveNew", "config"), status: ops.Failed, journal: "rolling_back replaced,replaced unsafe",
			left: true, restored: true, trace: swapped},
		{name: "revoked after staging", after: revokedAfter("stage", "config"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(checked, unstaged)},
		{name: "revoked after a move out", after: revokedAfter("moveOld", "world"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe"}, worldRolledBack)},
		{name: "revoked before commit", after: revokedAfter("moveNew", "config"), status: ops.Failed, journal: "rolled_back",
			trace: slices.Concat(swapped, rolledBack("replaced"))},
		{name: "revoked during rollback", status: ops.Failed, journal: "rolled_back", trace: slices.Concat(swapped, rolledBack("old_moved")),
			after: func(t *testing.T, f *fixture) afterHook {
				fail := once("moveNew", "config", func(*fakedocker.Container, int) int { return 1 })
				revoke := once("rollbackClean", "config", func(_ *fakedocker.Container, code int) int { f.revoke(t); return code })
				return func(c *fakedocker.Container, op, root string, code int) int {
					return revoke(c, op, root, fail(c, op, root, code))
				}
			}},
		{name: "revoked during cleanup", after: revokedAfter("cleanup", "world"), status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(swapped, committed)},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			f := setup(t)
			var input map[string]any
			if scenario.item {
				backupID, rootID := f.itemBackup(t)
				input = map[string]any{"backupId": backupID, "root": rootID, "item": "world/level.dat"}
			} else {
				input = map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
				for _, root := range []string{"world", "config"} {
					os.WriteFile(filepath.Join(f.data, root, "large.dat"), []byte("changed"), 0o644)
				}
			}
			var after afterHook
			if scenario.after != nil {
				after = scenario.after(t, f)
			}
			trace := f.traceRestore(after)
			job := f.run(t, "restore", input)
			restored := read(t, filepath.Join(f.data, "world", "level.dat")) == "original"
			if !scenario.item {
				info, err := os.Stat(filepath.Join(f.data, "world", "large.dat"))
				restored = err == nil && info.Size() == 4<<20
			}
			left := f.staged()
			for _, root := range []string{"world", "config"} {
				entries, _ := os.ReadDir(filepath.Join(f.data, root))
				left = left || slices.ContainsFunc(entries, func(entry os.DirEntry) bool { return strings.HasPrefix(entry.Name(), helper.StagePrefix) })
			}
			// A server started elsewhere is left running; every other restore
			// returns it to running once its data is safe.
			if journal := f.savedJournal(job.ID); job.Status != scenario.status || journal != scenario.journal || !f.running() ||
				restored != scenario.restored || left != scenario.left {
				t.Errorf("%s, %q, journal %q, restored %v, stage left %v", job.Status, job.Error, journal, restored, left)
			}
			if got := sortedExtracts(trace.lines()); !slices.Equal(got, scenario.trace) {
				t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(scenario.trace, "\n"))
			}
		})
	}
}

// interruptedRestore leaves a whole restore as a kill of Ludock would in
// journal state, with its data laid out by arrange, then starts the runner,
// which recovers it.
func (f *fixture) interruptedRestore(t *testing.T, state *restoreRecovery, arrange func(stage string)) (*ops.Job, *restoreTrace) {
	t.Helper()
	backup := f.run(t, "backup", nil)
	row, err := f.jobs.Backups.Get(ctx, f.serverID, backup.Result["backupId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	f.runner.Stop()
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: f.serverID, ActorID: "admin", Kind: "restore",
		BindingRevision: logical.BindingRevision, Input: map[string]any{"backupId": row.ID, "confirmation": "mc"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
		t.Fatal(err)
	}
	stopped, _ := f.jobs.Docker.Inspect(ctx, f.game)
	os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("newer"), 0o644)
	os.WriteFile(filepath.Join(f.data, "server.properties"), []byte("newer"), 0o644)
	arrange(filepath.Join(f.data, helper.StageName(job.ID)))
	running := true
	recovery := ops.Recovery{ContainerID: logical.ContainerID, Fingerprint: logical.BindingFingerprint,
		LockKeys: []string{locks.ServerKey(f.serverID)}, InitialRunning: &running, StopStarted: true, StopSettled: true,
		StoppedStartedAt: stopped.State.StartedAt}
	phase := "stopped"
	if state != nil {
		for index := range state.Roots {
			state.Roots[index].Root = row.Roots[0]
		}
		recovery.Restore, _ = json.Marshal(state)
		recovery.DataUnsafe = state.Status == "replacing" || state.Status == "rolling_back"
		phase = "restore_" + state.Status
	}
	encoded, _ := json.Marshal(recovery)
	if _, err := f.jobs.DB.Exec("UPDATE operations SET status='running',phase=?,recovery_json=? WHERE id=?", phase, string(encoded), job.ID); err != nil {
		t.Fatal(err)
	}
	trace := f.traceRestore(nil)
	if err := f.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	recovered, _ := f.runner.Get(ctx, job.ID)
	return recovered, trace
}

// relocate renames an entry between the data and its stage folder.
func relocate(t *testing.T, from, to string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(to), 0o700)
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

// A restore interrupted by a kill of Ludock is recovered from the journal
// it saved: one that committed is cleaned up, any other is rolled back
// without removing current data that never moved or was already returned,
// and the server starts again. A recovery that can't finish leaves the
// server stopped.
func TestRestoreRecoveryFromEachState(t *testing.T) {
	const (
		nothing  = "Nothing was restored: the server’s data is as it was."
		undone   = "The restore was undone: the server’s data is as it was."
		finished = "The restore had finished: what it restored is in place."
	)
	unpacked := func(t *testing.T, f *fixture, stage string) {
		os.MkdirAll(filepath.Join(stage, "new", "world"), 0o700)
		os.MkdirAll(filepath.Join(stage, "old"), 0o700)
		os.WriteFile(filepath.Join(stage, "new", "world", "level.dat"), []byte("original"), 0o644)
		os.WriteFile(filepath.Join(stage, "new", "server.properties"), []byte("original"), 0o644)
	}
	for _, c := range []struct {
		name   string
		state  *restoreRecovery
		failOn string
		// arrange moves data as the interrupted restore had.
		arrange func(t *testing.T, f *fixture, stage string)
		// want is the live data afterwards; guidance, the outcome
		// Activity shows; journal, the one left saved, rolled_back if empty.
		want, guidance, journal string
		running                 bool
		trace                   []string
	}{
		{name: "before staging", arrange: func(*testing.T, *fixture, string) {}, journal: "none", want: "newer", guidance: nothing, running: true},
		{name: "staging", state: &restoreRecovery{Status: "staging", Roots: []journal{{Phase: "staging"}}}, arrange: unpacked,
			want: "newer", guidance: nothing, running: true, trace: []string{"cleanup game: rolling_back staging unsafe"}},
		{name: "replacing, nothing moved", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "staging"}}}, arrange: unpacked,
			want: "newer", guidance: undone, running: true, trace: []string{"cleanup game: rolling_back staging unsafe"}},
		{name: "moving out", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				unpacked(t, f, stage)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, want: "newer", guidance: undone, running: true,
			trace: []string{"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe"}},
		{name: "moving in", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				unpacked(t, f, stage)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				relocate(t, filepath.Join(stage, "new", "world"), filepath.Join(f.data, "world"))
			}, want: "newer", guidance: undone, running: true,
			trace: []string{"rollbackClean game: rolling_back old_moved unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back unsafe"}},
		{name: "returning", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolling_back"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
			}, want: "newer", guidance: undone, running: true,
			trace: []string{"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe"}},
		{name: "committed", state: &restoreRecovery{Status: "committed", Roots: []journal{{Phase: "replaced"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.MkdirAll(filepath.Join(f.data, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("original"), 0o644)
				os.WriteFile(filepath.Join(f.data, "server.properties"), []byte("original"), 0o644)
			}, want: "original", guidance: finished, journal: "cleaned", running: true, trace: []string{"cleanup game: committed replaced"}},
		{name: "rolled back", state: &restoreRecovery{Status: "rolled_back"}, arrange: func(*testing.T, *fixture, string) {},
			want: "newer", guidance: undone, running: true},
		{name: "rollback fails", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}}, failOn: "rollbackOld",
			arrange: func(t *testing.T, f *fixture, stage string) {
				unpacked(t, f, stage)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
			}, journal: "rolling_back rolling_back unsafe",
			trace: []string{"rollbackClean game: rolling_back old_moved unsafe", "rollbackOld game: rolling_back rolling_back unsafe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			f.mu.Lock()
			f.failStep = c.failOn
			f.mu.Unlock()
			job, trace := f.interruptedRestore(t, c.state, func(stage string) { c.arrange(t, f, stage) })
			data := read(t, filepath.Join(f.data, "world", "level.dat")) + "," + read(t, filepath.Join(f.data, "server.properties"))
			if c.journal == "" {
				c.journal = "rolled_back"
			}
			guidance, _ := job.Result["guidance"].(string)
			if journal := f.savedJournal(job.ID); job.Status != ops.Interrupted || guidance != c.guidance || journal != c.journal ||
				f.running() != c.running || f.staged() == c.running || (c.want != "" && data != c.want+","+c.want) {
				t.Errorf("%s, %q, %q, journal %q, data %s", job.Status, job.Error, guidance, journal, data)
			}
			if got := trace.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.trace, "\n"))
			}
		})
	}
}
