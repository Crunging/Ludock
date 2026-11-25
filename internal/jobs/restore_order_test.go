package jobs

import (
	"bufio"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/mountinfo"
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
	if state.StageMissing {
		out += " stage missing"
	}
	if state.PutBack {
		out += " put back"
	}
	if state.Kept {
		out += " kept"
	}
	if state.KeptUnknown {
		out += " unknown"
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
	// during runs before the helper commands are traced.
	during func(*testing.T, *fixture)
	// status and journal describe the job when it ends.
	status, journal string
	// left says a stage folder is left behind; restored, that the backup's
	// data is live; elsewhere, that the server was started outside Ludock,
	// which never starts it again.
	left, restored, elsewhere bool
	trace                     []string
	// guidance starts what Activity says the data was left as; kept, that
	// it also names the set-aside folder.
	guidance string
	kept     bool
	// down: the data isn't safe, so the server stays stopped.
	down bool
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

// startedDuring starts the server elsewhere as a step begins, and the step
// runs on, as a long move does, until its helper is stopped.
func startedDuring(operation, root string) func(*testing.T, *fixture) {
	return changedDuring(operation, root, (*fixture).startElsewhere)
}

// replaceElsewhere binds the server to a replacement, as discovery does
// when another manager starts one and leaves the stopped original.
func (f *fixture) replaceElsewhere() {
	f.jobs.DB.Exec(`UPDATE logical_servers SET container_id='replacement', binding_revision=binding_revision+1 WHERE id=?`, f.serverID)
}

// writeElsewhere starts another container that can write to the data.
func (f *fixture) writeElsewhere() {
	f.engine.AddContainer(fakedocker.GameOptions{Name: "sidecar", Image: "busybox", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: f.data, Destination: "/data", RW: true}}})
}

// stalledStart leaves the first move's exec start unanswered, as a stalled
// Docker can, and starts the server elsewhere once it is sent.
func stalledStart(t *testing.T, f *fixture) afterHook {
	var mu sync.Mutex
	extracted := 0
	return func(_ *fakedocker.Container, operation, _ string, code int) int {
		if operation != "extract" {
			return code
		}
		mu.Lock()
		defer mu.Unlock()
		// The extracts run at once.
		if extracted++; extracted == 2 {
			arrived := f.engine.HangRequests(fakedocker.ExecStart)
			go func() {
				<-arrived
				f.engine.HangRequests("^$")
				f.startElsewhere()
				started := time.Now()
				t.Cleanup(func() {
					if took := time.Since(started); took > 5*time.Second {
						t.Errorf("waited %v on an unanswered start with the server running", took)
					}
				})
			}()
		}
		return code
	}
}

func changedDuring(operation, root string, change func(*fixture)) func(*testing.T, *fixture) {
	return func(t *testing.T, f *fixture) {
		run, hit := f.engine.Exec, false
		f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
			var request struct{ Operation, Root string }
			if len(cmd) > 3 && cmd[2] == "restore" {
				json.Unmarshal([]byte(cmd[3]), &request)
			}
			if hit || request.Operation != operation || filepath.Base(request.Root) != root {
				return run(c, cmd, stdin, stdout, stderr)
			}
			hit = true
			change(f)
			started := time.Now()
			for time.Since(started) < 5*time.Second {
				if helper := f.engine.Container(c.ID); helper == nil || !helper.State.Running {
					if took := time.Since(started); took > 2*time.Second {
						t.Errorf("the step ran on for %v after the change", took)
					}
					return 137
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Error("the step ran on after the change")
			return run(c, cmd, stdin, stdout, stderr)
		}
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
			"cleanup world: rolling_back rolled_back,rolled_back unsafe",
			"kept world: rolling_back rolled_back,rolled_back unsafe", "kept config: rolling_back rolled_back,rolled_back unsafe"}
	}
	worldRolledBack := []string{"cleanup config: rolling_back old_moved,staging unsafe",
		"rollbackClean world: rolling_back old_moved,staging unsafe",
		"rollbackOld world: rolling_back rolling_back,staging unsafe",
		"cleanup world: rolling_back rolled_back,staging unsafe",
		"kept world: rolling_back rolled_back,staging unsafe", "kept config: rolling_back rolled_back,staging unsafe"}
	itemSwapped := []string{"space game: none", "check game: none", "space game: staging staging", "stage game: staging staging",
		"extract game: staging staging", "moveOld game: replacing moving_old unsafe", "moveNew game: replacing old_moved unsafe"}
	scenarios := []restoreScenario{
		{name: "succeeds", status: ops.Succeeded, journal: "cleaned", restored: true, trace: slices.Concat(swapped, committed)},
		{name: "item succeeds", item: true, status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(itemSwapped, []string{"cleanup game: committed replaced"})},
		{name: "item swap fails", item: true, after: failedAt(helper.RestoreMoveNew, "game"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(itemSwapped, []string{"rollbackClean game: rolling_back old_moved unsafe",
				"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe", "kept game: rolling_back rolled_back unsafe"})},
		// Without a helper nothing was staged, so there is nothing to roll back.
		{name: "item helper not created", item: true, status: ops.Failed, journal: "rolled_back",
			after: func(t *testing.T, f *fixture) afterHook {
				return once(helper.RestoreCheck, "game", func(_ *fakedocker.Container, code int) int {
					f.engine.AddFault("POST .*/containers/create$", 500, false, 1)
					return code
				})
			}, trace: []string{"space game: none", "check game: none"}},
		{name: "stage fails", after: failedAt(helper.RestoreStage, "config"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(checked, unstaged)},
		{name: "extract fails", after: failedAt("extract", "world"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(extracted, unstaged)},
		// An interrupted move out leaves current data in place, which the
		// rollback must not remove.
		{name: "move out fails", after: failedAt(helper.RestoreMoveOld, "world"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"cleanup config: rolling_back moving_old,staging unsafe", "rollbackOld world: rolling_back rolling_back,staging unsafe",
				"cleanup world: rolling_back rolled_back,staging unsafe", "kept world: rolling_back rolled_back,staging unsafe",
				"kept config: rolling_back rolled_back,staging unsafe"})},
		{name: "first move in fails", after: failedAt(helper.RestoreMoveNew, "world"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(worldSwapped, worldRolledBack)},
		// Something makes a file in the emptied world folder during the
		// rollback, which sets it aside, but the rollback's result is lost.
		{name: "rollback's result lost", status: ops.Failed, journal: "rolling_back rolling_back,staging kept unsafe", left: true, kept: true, down: true,
			guidance: "The restore was stopped partway while its previous data was being put back", trace: slices.Concat(worldSwapped, []string{"cleanup config: rolling_back old_moved,staging unsafe",
				"rollbackClean world: rolling_back old_moved,staging unsafe", "rollbackOld world: rolling_back rolling_back,staging unsafe",
				"kept world: rolling_back rolling_back,staging unsafe", "kept config: rolling_back rolling_back,staging unsafe"}),
			after: func(t *testing.T, f *fixture) afterHook {
				fail := once(helper.RestoreMoveNew, "world", func(*fakedocker.Container, int) int { return 1 })
				write := once(helper.RestoreRollbackClean, "world", func(_ *fakedocker.Container, code int) int {
					os.WriteFile(filepath.Join(f.data, "world", "large.dat"), []byte("made meanwhile"), 0o644)
					return code
				})
				lose := once(helper.RestoreRollbackOld, "world", func(*fakedocker.Container, int) int { return 1 })
				return func(c *fakedocker.Container, op, root string, code int) int {
					return lose(c, op, root, write(c, op, root, fail(c, op, root, code)))
				}
			}},
		{name: "last move in fails", after: failedAt(helper.RestoreMoveNew, "config"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(swapped, rolledBack("old_moved"))},
		// A failed command that took its helper with it rolls back through a
		// new one.
		{name: "helper exits", status: ops.Failed, journal: "rolled_back put back", trace: slices.Concat(worldSwapped, worldRolledBack),
			after: func(t *testing.T, f *fixture) afterHook {
				return once(helper.RestoreMoveNew, "world", func(c *fakedocker.Container, _ int) int { f.engine.Exit(c.ID, 1); return 1 })
			}},
		{name: "cleanup fails once", after: failedAt(helper.RestoreCleanup, "world"), status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(swapped, []string{"cleanup world: committed replaced,replaced"}, committed)},
		// A server started elsewhere never reads a stage folder, so the
		// rollback still removes those; it leaves roots with live data moved
		// for later.
		{name: "started after staging", after: startedAfter(helper.RestoreStage, "config"), status: ops.Failed, journal: "rolled_back",
			elsewhere: true, guidance: restoreNothing, trace: slices.Concat(checked, unstaged)},
		{name: "started after unpacking", after: startedAfter("extract", "config"), status: ops.Failed, journal: "rolled_back",
			elsewhere: true, guidance: restoreNothing, trace: slices.Concat(extracted, unstaged)},
		// The stopped server is checked again between moving a root's data
		// out and the backup's in.
		{name: "started after a move out", after: startedAfter(helper.RestoreMoveOld, "world"), status: ops.Failed, journal: "rolling_back old_moved,staging unsafe",
			left: true, elsewhere: true, guidance: "The restore was stopped partway.", trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"cleanup config: rolling_back old_moved,staging unsafe", "kept world: rolling_back old_moved,staging unsafe",
				"kept config: rolling_back old_moved,staging unsafe"})},
		// One started during a move stops it within about a second, with
		// nothing more moved.
		{name: "started during a move out", during: startedDuring(helper.RestoreMoveOld, "world"), status: ops.Failed, journal: "replacing moving_old,staging unsafe",
			left: true, elsewhere: true, guidance: "The restore was stopped partway.", trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"kept world: replacing moving_old,staging unsafe", "kept config: replacing moving_old,staging unsafe"})},
		// Likewise when the server is no longer the one the restore stopped,
		// another container that can write to its data starts, or the move's
		// start is never answered.
		{name: "replaced during a move out", during: changedDuring(helper.RestoreMoveOld, "world", (*fixture).replaceElsewhere), status: ops.Failed,
			journal: "replacing moving_old,staging unsafe", left: true, down: true, guidance: "The restore was stopped partway.",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"kept world: replacing moving_old,staging unsafe", "kept config: replacing moving_old,staging unsafe"})},
		{name: "writer started during a move out", during: changedDuring(helper.RestoreMoveOld, "world", (*fixture).writeElsewhere), status: ops.Failed,
			journal: "replacing moving_old,staging unsafe", left: true, down: true, guidance: "The restore was stopped partway.",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe",
				"kept world: replacing moving_old,staging unsafe", "kept config: replacing moving_old,staging unsafe"})},
		{name: "started at a stalled move out", after: stalledStart, status: ops.Failed, journal: "replacing moving_old,staging unsafe",
			left: true, elsewhere: true, guidance: "The restore was stopped partway.", trace: slices.Concat(extracted,
				[]string{"kept world: replacing moving_old,staging unsafe", "kept config: replacing moving_old,staging unsafe"})},
		{name: "started during a move in", during: startedDuring(helper.RestoreMoveNew, "world"), status: ops.Failed, journal: "replacing old_moved,staging unsafe",
			left: true, elsewhere: true, guidance: "The restore was stopped partway.", trace: slices.Concat(worldSwapped,
				[]string{"kept world: replacing old_moved,staging unsafe", "kept config: replacing old_moved,staging unsafe"})},
		{name: "started before commit", after: startedAfter(helper.RestoreMoveNew, "config"), status: ops.Failed, journal: "rolling_back replaced,replaced unsafe",
			left: true, elsewhere: true, restored: true, guidance: "The restore was stopped partway.", trace: slices.Concat(swapped,
				[]string{"kept world: rolling_back replaced,replaced unsafe", "kept config: rolling_back replaced,replaced unsafe"})},
		{name: "revoked after staging", after: revokedAfter(helper.RestoreStage, "config"), status: ops.Failed, journal: "rolled_back", trace: slices.Concat(checked, unstaged)},
		{name: "revoked after a move out", after: revokedAfter(helper.RestoreMoveOld, "world"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(extracted, []string{"moveOld world: replacing moving_old,staging unsafe"}, worldRolledBack)},
		{name: "revoked before commit", after: revokedAfter(helper.RestoreMoveNew, "config"), status: ops.Failed, journal: "rolled_back put back",
			trace: slices.Concat(swapped, rolledBack("replaced"))},
		{name: "revoked during rollback", status: ops.Failed, journal: "rolled_back put back", trace: slices.Concat(swapped, rolledBack("old_moved")),
			after: func(t *testing.T, f *fixture) afterHook {
				fail := once(helper.RestoreMoveNew, "config", func(*fakedocker.Container, int) int { return 1 })
				revoke := once(helper.RestoreRollbackClean, "config", func(_ *fakedocker.Container, code int) int { f.revoke(t); return code })
				return func(c *fakedocker.Container, op, root string, code int) int {
					return revoke(c, op, root, fail(c, op, root, code))
				}
			}},
		{name: "revoked during cleanup", after: revokedAfter(helper.RestoreCleanup, "world"), status: ops.Succeeded, journal: "cleaned", restored: true,
			trace: slices.Concat(swapped, committed)},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			f := setup(t)
			var input map[string]any
			if scenario.item {
				backupID, rootID := f.itemBackup(t)
				input = map[string]any{"backupId": backupID, "root": rootID, "item": "world/level.dat"}
				os.Chmod(f.data, 0o750)
			} else {
				// The world root and a folder in it are setgid, another folder
				// is sticky, and a program is setuid and setgid.
				world := filepath.Join(f.data, "world")
				os.Chmod(world, 0o755|os.ModeSetgid)
				os.Mkdir(filepath.Join(world, "shared"), 0)
				os.Chmod(filepath.Join(world, "shared"), 0o775|os.ModeSetgid)
				os.Mkdir(filepath.Join(world, "tmp"), 0)
				os.Chmod(filepath.Join(world, "tmp"), 0o777|os.ModeSticky)
				os.WriteFile(filepath.Join(world, "run"), nil, 0)
				os.Chmod(filepath.Join(world, "run"), 0o755|os.ModeSetuid|os.ModeSetgid)
				input = map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
				for _, root := range []string{"world", "config"} {
					os.WriteFile(filepath.Join(f.data, root, "large.dat"), []byte("changed"), 0o644)
					// The backup has the roots' own permissions, 0755.
					os.Chmod(filepath.Join(f.data, root), 0o750)
				}
			}
			if scenario.during != nil {
				scenario.during(t, f)
			}
			var after afterHook
			if scenario.after != nil {
				after = scenario.after(t, f)
			}
			trace := f.traceRestore(after)
			// The server starts again only once the journal is final, and one
			// started elsewhere is never stopped and started again.
			var startedWith []string
			start := f.engine.OnStart
			f.engine.OnStart = func(c *fakedocker.Container) {
				start(c)
				if c.ID == f.game {
					f.mu.Lock()
					startedWith = append(startedWith, f.savedJournal(lastRestore(f)))
					f.mu.Unlock()
				}
			}
			job := f.run(t, "restore", input)
			f.mu.Lock()
			want := []string{scenario.journal}
			if scenario.elsewhere || scenario.down {
				want = nil
			}
			if !slices.Equal(startedWith, want) {
				t.Errorf("started the server with journals %q, want %q", startedWith, want)
			}
			f.mu.Unlock()
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
			if journal := f.savedJournal(job.ID); job.Status != scenario.status || journal != scenario.journal || f.running() == scenario.down ||
				restored != scenario.restored || left != scenario.left {
				t.Errorf("%s, %q, journal %q, restored %v, stage left %v", job.Status, job.Error, journal, restored, left)
			}
			// A whole restore gives each root the backup's permissions as it
			// commits, and every folder its setgid and sticky bits, but never
			// sets setuid or a file's setgid; one that didn't restore, or
			// restored one item, leaves the roots as they were.
			modes := map[string]os.FileMode{"world": 0o750, "config": 0o750}
			if scenario.item {
				modes = map[string]os.FileMode{"": 0o750}
			} else if scenario.restored {
				modes = map[string]os.FileMode{"world": 0o755 | os.ModeSetgid, "config": 0o755, "world/shared": 0o775 | os.ModeSetgid,
					"world/tmp": 0o777 | os.ModeSticky, "world/run": 0o755}
			}
			for name, mode := range modes {
				if info, err := os.Stat(filepath.Join(f.data, name)); err != nil || info.Mode()&^os.ModeType != mode {
					t.Errorf("%q is %v, want %v", name, info.Mode(), mode)
				}
			}
			// A restore that rolled back leaves the data exactly as it was.
			if !scenario.restored && !scenario.left {
				current := map[string]string{"world/level.dat": "newer"}
				if !scenario.item {
					current = map[string]string{"world/large.dat": "changed", "config/large.dat": "changed"}
				}
				for file, want := range current {
					if got := read(t, filepath.Join(f.data, file)); got != want {
						t.Errorf("%s is %q after the rollback, want %q", file, got, want)
					}
				}
			}
			if guidance := job.Result.Guidance; !strings.HasPrefix(guidance, scenario.guidance) ||
				strings.Contains(guidance, helper.SetAsideName(job.ID)) != scenario.kept {
				t.Errorf("guidance %q", guidance)
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
	row, err := f.jobs.Backups.Get(ctx, f.serverID, backup.Result.BackupID)
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
			state.Roots[index].Root = row.Roots[min(index, len(row.Roots)-1)]
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
// and the server starts again, which Activity says. A recovery that can't
// finish leaves the server stopped.
func TestRestoreRecoveryFromEachState(t *testing.T) {
	const (
		nothing  = "Nothing was restored: the server’s data is as it was."
		undone   = "The restore was undone: the server’s data is as it was."
		finished = "Ludock restarted while this ran, after the restore had finished: what it restored is in place."
		// A return under way may have finished unheard.
		pending = "The restore was stopped partway while its previous data was being put back, so Ludock can’t confirm where all of it is: look in the server’s data and in the folder " + helper.StagePrefix + "%s. An administrator can undo the restore from Needs attention; otherwise Ludock undoes it the next time it stops the server for a backup, a restore or an update."
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
			}, want: "newer", guidance: undone, journal: "rolled_back put back", running: true,
			trace: []string{"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe",
				"kept game: rolling_back rolled_back unsafe"}},
		{name: "moving in", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				unpacked(t, f, stage)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				relocate(t, filepath.Join(stage, "new", "world"), filepath.Join(f.data, "world"))
			}, want: "newer", guidance: undone, journal: "rolled_back put back", running: true,
			trace: []string{"rollbackClean game: rolling_back old_moved unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back unsafe", "kept game: rolling_back rolled_back unsafe"}},
		{name: "returning", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolling_back"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
			}, want: "newer", guidance: undone, journal: "rolled_back put back", running: true,
			trace: []string{"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe",
				"kept game: rolling_back rolled_back unsafe"}},
		{name: "committed", state: &restoreRecovery{Status: "committed", Roots: []journal{{Phase: "replaced"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.MkdirAll(filepath.Join(f.data, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("original"), 0o644)
				os.WriteFile(filepath.Join(f.data, "server.properties"), []byte("original"), 0o644)
			}, want: "original", guidance: finished, journal: "cleaned", running: true, trace: []string{"cleanup game: committed replaced"}},
		{name: "rolled back but staged", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}},
			arrange: func(t *testing.T, f *fixture, stage string) {
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
			}, want: "newer", guidance: undone, journal: "rolled_back put back", running: true, trace: []string{"cleanup game: rolling_back rolled_back unsafe",
				"kept game: rolling_back rolled_back unsafe"}},
		{name: "rolled back", state: &restoreRecovery{Status: "rolled_back"}, arrange: func(*testing.T, *fixture, string) {},
			want: "newer", guidance: undone, running: true},
		// The server stays stopped, so the pass for restores that ended
		// partway tries once more as startup finishes, and fails again.
		{name: "rollback fails", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}}, failOn: helper.RestoreRollbackOld,
			arrange: func(t *testing.T, f *fixture, stage string) {
				unpacked(t, f, stage)
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
			}, journal: "rolling_back rolling_back unsafe", guidance: pending,
			trace: []string{"rollbackClean game: rolling_back old_moved unsafe", "rollbackOld game: rolling_back rolling_back unsafe", "kept game: rolling_back rolling_back unsafe",
				"stageExists game: rolling_back rolling_back unsafe", "checkReturn game: rolling_back rolling_back unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"kept game: rolling_back rolling_back unsafe"}},
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
			guidance := job.Result.Guidance
			c.guidance = strings.ReplaceAll(c.guidance, "%s", job.ID)
			if c.running {
				c.guidance += " Ludock started the server again."
			}
			// A restore that had finished ends as succeeded.
			status := ops.Interrupted
			if strings.HasPrefix(guidance, finished) {
				status = ops.Succeeded
			}
			if journal := f.savedJournal(job.ID); job.Status != status || guidance != c.guidance || journal != c.journal ||
				f.running() != c.running || f.staged() == c.running || (c.want != "" && data != c.want+","+c.want) {
				t.Errorf("%s, %q, %q, journal %q, data %s", job.Status, job.Error, guidance, journal, data)
			}
			if got := trace.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.trace, "\n"))
			}
			// Only a restore that didn't finish alerts, once.
			var alerts []string
			if !strings.HasPrefix(guidance, finished) {
				alerts = []string{"mc: Ludock restarted during a restore. Its Activity says what the data was left as."}
			}
			if !slices.Equal(f.alerts, alerts) {
				t.Error("alerts:", f.alerts)
			}
		})
	}
}

// Ludock is killed while a restore moves the server's data out, or while a
// later undo returns it, and then something outside Ludock starts the server,
// whose game writes a new world. Undo stops the server, sets aside each newer
// copy in the way of what it returns, without overwriting an earlier one, and
// starts the server again, saying where the newer copies are. Each case is a
// point between steps where the work was cut; aside is what the set-aside
// folder holds afterwards. Undo checks the stage and what is in the
// originals' way before the stop and again once stopped, and the restore's
// final journal is saved with what both Activity entries say. A mount the
// host added on or inside the newer copy, or on a restored file's folder, or
// an original with a second name, refuses the undo with nothing moved, once
// the server has stopped, since a stopping game can drop either, and the
// server starts again. A
// newer copy found by startup's recovery of the cut restore, with the server
// still stopped, is set aside too, and its Activity says where.
func TestUndoAfterInterruptedMove(t *testing.T) {
	for _, c := range []struct {
		name    string
		state   *restoreRecovery
		arrange func(t *testing.T, f *fixture, stage, aside string)
		aside   map[string]string
		trace   []string
		// mounted is where the host mounted something, below the data;
		// refusal, the error that refuses the undo before the stop.
		mounted, refusal string
		// afterStop: a live entry in the way, which the running game may
		// still change, is refused once the server stops, as main does, and
		// the server starts again.
		afterStop bool
		// failStep fails the undo at that step, and guidance is part of what
		// the restore's Activity then says.
		failStep, guidance string
		// noGame: the started game recreates nothing; startFails: starting
		// the recovered server fails.
		noGame, startFails bool
		// unlinkOnStop is a file the game removes as it stops.
		unlinkOnStop string
		// lostStep runs, but its helper reports failure, as when it is
		// removed before its result is read, which lostHelper does.
		lostStep   string
		lostHelper bool
		// unreadable: the set-aside folder can't be read, so Activity says
		// where to look; checkFails: the checks of what is in the way can't
		// run, which refuses nothing; checkCut: the one after the stop is cut
		// with its helper.
		unreadable, checkFails, checkCut bool
		// finalFails: the final journal's write fails; helperFails: no
		// helper can start once the server stops; cutAfterFinal: startup is
		// cut right after its final journal and runs again; rereadUnsaved:
		// what a failed attempt reads can't be saved.
		finalFails, helperFails, cutAfterFinal, rereadUnsaved bool
		// plain: nothing was set aside, so Activity says only that the
		// previous data is back.
		plain bool
		// recovered: the server stays stopped and startup recovers the cut
		// restore, with no undo.
		recovered bool
	}{
		{name: "moving out", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, aside: map[string]string{"world/level.dat": "new run"},
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
		// An earlier undo set the first run's world aside, then was cut
		// before returning the original; the game ran again since.
		{name: "set aside, not returned", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolling_back"}}},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				os.MkdirAll(filepath.Join(f.data, aside, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "world", "level.dat"), []byte("first run"), 0o644)
			}, aside: map[string]string{"world/level.dat": "first run", "world (2)/level.dat": "new run"},
			trace: []string{"stageExists game: rolling_back rolling_back kept unsafe", "checkReturn game: rolling_back rolling_back kept unsafe",
				"stageExists game: rolling_back rolling_back kept unsafe", "checkReturn game: rolling_back rolling_back kept unsafe", "rollbackOld game: rolling_back rolling_back kept unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
		// Cut once the original was back, before the journal said so.
		{name: "returned", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolling_back"}}},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "world", "level.dat"), []byte("first run"), 0o644)
			}, aside: map[string]string{"world/level.dat": "first run"},
			trace: []string{"stageExists game: rolling_back rolling_back kept unsafe", "checkReturn game: rolling_back rolling_back kept unsafe",
				"stageExists game: rolling_back rolling_back kept unsafe", "checkReturn game: rolling_back rolling_back kept unsafe", "rollbackOld game: rolling_back rolling_back kept unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
		// Cut once the journal said so, before the stage was removed.
		{name: "returned and saved", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "world", "level.dat"), []byte("first run"), 0o644)
			}, aside: map[string]string{"world/level.dat": "first run"},
			trace: []string{"cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
		// Cut likewise, and the owner has since moved the kept copies away.
		{name: "kept copies moved away", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
			}, plain: true, trace: []string{"cleanup game: rolling_back rolled_back unsafe", "kept game: rolling_back rolled_back unsafe"}},
		// Likewise, and an earlier start couldn't read the folder: startup reads
		// it now and doesn't repeat that.
		{name: "kept copies moved away, earlier read failed", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				encoded, _ := json.Marshal(ops.Result{Guidance: "The restore was undone. Ludock couldn’t check the folder " + aside +
					" in the server’s data, where it moves anything in the way of the previous data."})
				f.jobs.DB.Exec(`UPDATE operations SET result_json=? WHERE id=?`, string(encoded), strings.TrimPrefix(filepath.Base(stage), helper.StagePrefix))
			}, plain: true, trace: []string{"cleanup game: rolling_back rolled_back unsafe", "kept game: rolling_back rolled_back unsafe"}},
		// The check after the stop is cut by its time limit, which takes its
		// helper with it: the return still runs.
		{name: "check after the stop cut", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, checkCut: true, aside: map[string]string{"world/level.dat": "new run"},
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe", "kept game: rolling_back rolled_back kept unsafe"}},
		// Likewise, and the owner left a note in its place: Activity says what
		// Ludock moved, not what the folder holds now.
		{name: "owner left a note", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "README.txt"), []byte("moved the save"), 0o644)
			}, aside: map[string]string{"README.txt": "moved the save"},
			trace: []string{"cleanup game: rolling_back rolled_back kept unsafe", "kept game: rolling_back rolled_back kept unsafe"}},
		// Likewise, and the retry fails before moving anything: what the
		// folders show now, not what an earlier attempt saw, is what Activity
		// says.
		{name: "kept copies moved away, cleanup fails", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
			}, failStep: helper.RestoreCleanup, plain: true, guidance: "undone"},
		// Likewise, and what that attempt reads can't be saved: Activity says
		// where to look, not what the earlier read found.
		{name: "kept copies moved away, read unsaved", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
			}, failStep: helper.RestoreCleanup, rereadUnsaved: true, guidance: "couldn’t check the folder"},
		// Likewise, with copies back in the folder after an earlier read found
		// none.
		{name: "copies back, read unsaved", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "level.dat"), []byte("new run"), 0o644)
			}, failStep: helper.RestoreCleanup, rereadUnsaved: true, guidance: "couldn’t check the folder"},
		// Cut likewise, with nothing set aside.
		{name: "returned and saved, nothing kept", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
			}, plain: true, trace: []string{"cleanup game: rolling_back rolled_back unsafe", "kept game: rolling_back rolled_back unsafe"}},
		{name: "mount in the newer copy", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, mounted: "world/mods", refusal: backups.ErrHostMount.Message, afterStop: true},
		{name: "mount on the newer copy", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, mounted: "world", refusal: backups.ErrHostMount.Message, afterStop: true},
		{name: "mount on the file's folder", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Item: "world/level.dat", Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world", "level.dat"), filepath.Join(stage, "old", "item"))
			}, mounted: "world", refusal: backups.ErrHostMount.Message, afterStop: true},
		{name: "hard-linked newer copy", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.WriteFile(filepath.Join(f.data, "server.properties"), []byte("new run"), 0o644)
				os.Link(filepath.Join(f.data, "server.properties"), filepath.Join(f.data, "linked"))
			}, refusal: backups.ErrUndoClashUnsafe.Message, afterStop: true},
		{name: "hard-linked until the game stops", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.WriteFile(filepath.Join(f.data, "server.properties"), []byte("new run"), 0o644)
				os.Link(filepath.Join(f.data, "server.properties"), filepath.Join(f.data, "linked"))
			}, unlinkOnStop: "linked", aside: map[string]string{"server.properties": "new run"},
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
		// The whole folder's live data is set aside, then returning the
		// previous data fails.
		{name: "set aside, then the return fails", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
			}, failStep: helper.RestoreRollbackOld, guidance: "stopped partway"},
		{name: "hard-linked original", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.Link(filepath.Join(stage, "old", "server.properties"), filepath.Join(f.data, "snapshot"))
			}, refusal: backups.ErrUndoClashUnsafe.Message, afterStop: true},
		// The game holds a second name for a staged original and drops it as
		// it stops.
		{name: "original's second name dropped as the game stops", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.Link(filepath.Join(stage, "old", "server.properties"), filepath.Join(f.data, "snapshot"))
			}, unlinkOnStop: "snapshot", plain: true,
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe",
				"kept game: rolling_back rolled_back unsafe"}},
		// The set-aside's outcome is lost with its helper: the folder shows
		// what was kept.
		{name: "set aside, outcome unknown", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "old_moved"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
			}, lostStep: helper.RestoreSetAside, guidance: "stopped partway"},
		// Nothing is in the way, and the set-aside folder can't be read: the
		// return still finishes.
		{name: "set-aside folder unreadable", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				relocate(t, filepath.Join(f.data, "server.properties"), filepath.Join(stage, "old", "server.properties"))
				os.Mkdir(filepath.Join(f.data, aside), 0)
				t.Cleanup(func() { os.Chmod(filepath.Join(f.data, aside), 0o700) })
			}, unreadable: true,
			trace: []string{"stageExists game: replacing moving_old unknown unsafe", "checkReturn game: replacing moving_old unknown unsafe",
				"stageExists game: replacing moving_old unknown unsafe", "checkReturn game: replacing moving_old unknown unsafe",
				"rollbackOld game: rolling_back rolling_back unknown unsafe", "cleanup game: rolling_back rolled_back unknown unsafe",
				"kept game: rolling_back rolled_back unknown unsafe"}},
		// The return happens, but its helper is removed before Ludock hears:
		// Activity doesn't claim the previous data is out of place.
		{name: "return's helper lost", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, lostStep: helper.RestoreRollbackOld, lostHelper: true, guidance: "can’t confirm", aside: map[string]string{"world/level.dat": "new run"}},
		// No helper starts: the undo fails, and both Activity entries say
		// where to look.
		{name: "helper can't start", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, helperFails: true},
		// The final journal's write fails: neither Activity note is saved.
		{name: "final write fails", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, finalFails: true},
		{name: "cleanup fails", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, aside: map[string]string{"world/level.dat": "new run"}, failStep: helper.RestoreCleanup, guidance: "undone"},
		{name: "one file, nothing in its way", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Item: "world/level.dat", Phase: "old_moved"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world", "level.dat"), filepath.Join(stage, "old", "item"))
				os.MkdirAll(filepath.Join(stage, "new"), 0o700)
			}, noGame: true, plain: true, trace: []string{"stageExists game: replacing old_moved unsafe", "stageExists game: replacing old_moved unsafe",
				"setAside game: rolling_back old_moved unsafe", "rollbackOld game: rolling_back rolling_back unsafe", "cleanup game: rolling_back rolled_back unsafe",
				"kept game: rolling_back rolled_back unsafe"}},
		// Startup finishes a restore an earlier attempt had already returned,
		// then can't start the server.
		{name: "recovered, start fails", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolled_back"}}, Kept: true},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "world", "level.dat"), []byte("first run"), 0o644)
				f.engine.AddFault(`^POST .*/containers/`+f.game+`/start`, 500, false, 0)
			}, aside: map[string]string{"world/level.dat": "first run"}, recovered: true, startFails: true},
		// Startup's recovery sets a copy aside, then can't read the folder.
		{name: "recovered, folder unread", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				f.traceRestore(func(_ *fakedocker.Container, op, _ string, code int) int {
					if op == helper.RestoreKept {
						return 1
					}
					return code
				})
			}, recovered: true, unreadable: true, aside: map[string]string{"world/level.dat": "new run"}},
		// Likewise, and startup is cut right after its final journal: the
		// next start still says where to look.
		{name: "recovered, folder unread, cut", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
				f.traceRestore(func(_ *fakedocker.Container, op, _ string, code int) int {
					if op == helper.RestoreKept {
						return 1
					}
					return code
				})
				// What the final journal's write leaves, before anything after it.
				f.jobs.DB.Exec(`CREATE TABLE final_row (result_json TEXT)`)
				f.jobs.DB.Exec(`CREATE TRIGGER final_seen AFTER UPDATE OF recovery_json ON operations
					WHEN NEW.kind='restore' AND json_extract(NEW.recovery_json,'$.restore.status')='rolled_back'
					AND coalesce(json_extract(OLD.recovery_json,'$.restore.status'),'')<>'rolled_back'
					BEGIN INSERT INTO final_row VALUES (NEW.result_json); END`)
			}, recovered: true, unreadable: true, cutAfterFinal: true, aside: map[string]string{"world/level.dat": "new run"}},
		{name: "checks can't run", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, checkFails: true, aside: map[string]string{"world/level.dat": "new run"},
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe", "kept game: rolling_back rolled_back kept unsafe"}},
		// Ludock stopped after a return that moved a copy aside, before it
		// heard: startup has nothing left to move, and still names the folder.
		{name: "recovered after its result was lost", state: &restoreRecovery{Status: "rolling_back", Roots: []journal{{Phase: "rolling_back"}}},
			arrange: func(t *testing.T, f *fixture, stage, aside string) {
				os.MkdirAll(filepath.Join(stage, "old"), 0o700)
				os.MkdirAll(filepath.Join(f.data, aside, "world"), 0o755)
				os.WriteFile(filepath.Join(f.data, aside, "world", "level.dat"), []byte("first run"), 0o644)
			}, aside: map[string]string{"world/level.dat": "first run"}, recovered: true},
		{name: "recovered at startup", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world"), filepath.Join(stage, "old", "world"))
			}, aside: map[string]string{"world/level.dat": "new run"}, recovered: true},
		{name: "one file moving out", state: &restoreRecovery{Status: "replacing", Roots: []journal{{Item: "world/level.dat", Phase: "moving_old"}}},
			arrange: func(t *testing.T, f *fixture, stage, _ string) {
				relocate(t, filepath.Join(f.data, "world", "level.dat"), filepath.Join(stage, "old", "item"))
			}, aside: map[string]string{"level.dat": "new run"},
			trace: []string{"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe",
				"stageExists game: replacing moving_old unsafe", "checkReturn game: replacing moving_old unsafe", "rollbackOld game: rolling_back rolling_back unsafe",
				"cleanup game: rolling_back rolled_back kept unsafe",
				"kept game: rolling_back rolled_back kept unsafe"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			var aside string
			job, _ := f.interruptedRestore(t, c.state, func(stage string) {
				aside = helper.SetAsideName(strings.TrimPrefix(filepath.Base(stage), helper.StagePrefix))
				c.arrange(t, f, stage, aside)
				if !c.recovered {
					if err := f.jobs.Docker.Start(ctx, f.game); err != nil {
						t.Fatal(err)
					}
				}
				// The game makes a new world where it finds none.
				if _, err := os.Stat(filepath.Join(f.data, "world", "level.dat")); os.IsNotExist(err) && !c.noGame {
					os.MkdirAll(filepath.Join(f.data, "world"), 0o755)
					os.WriteFile(filepath.Join(f.data, "world", "level.dat"), []byte("new run"), 0o644)
				}
			})
			checkKept := func() {
				t.Helper()
				if data := read(t, filepath.Join(f.data, "world", "level.dat")) + "," + read(t, filepath.Join(f.data, "server.properties")); data != "newer,newer" {
					t.Errorf("previous data %s", data)
				}
				var kept []string
				filepath.WalkDir(filepath.Join(f.data, aside), func(path string, entry os.DirEntry, err error) error {
					if err == nil && !entry.IsDir() {
						name, _ := filepath.Rel(filepath.Join(f.data, aside), path)
						kept = append(kept, name)
						if read(t, path) != c.aside[name] {
							t.Errorf("%s holds %q, want %q", name, read(t, path), c.aside[name])
						}
					}
					return nil
				})
				if len(kept) != len(c.aside) {
					t.Errorf("set aside %q, want %v", kept, c.aside)
				}
			}
			reworded := ""
			if c.cutAfterFinal {
				// The next start reads the fact, not the words Activity used.
				note := unknownKeptNote
				unknownKeptNote = func(restore *ops.Context) string { return note(restore) + " (reworded)" }
				t.Cleanup(func() { unknownKeptNote = note })
				reworded = " (reworded)"
				f.runner.Stop()
				if _, err := f.jobs.DB.Exec(`UPDATE operations SET status='running', result_json=(SELECT result_json FROM final_row) WHERE id=?`, job.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.runner.Start(ctx, nil); err != nil {
					t.Fatal(err)
				}
				job, _ = f.runner.Get(ctx, job.ID)
			}
			if c.recovered {
				journal, says := "rolled_back put back kept", "Anything Ludock moved"
				if c.unreadable {
					journal, says = "rolled_back put back unknown", "couldn’t check the folder"
				}
				if guidance := job.Result.Guidance; f.savedJournal(job.ID) != journal || f.running() == c.startFails ||
					!strings.Contains(guidance, "undone") || !strings.Contains(guidance, says) || !strings.Contains(guidance, reworded) ||
					!strings.Contains(guidance, aside) {
					t.Errorf("journal %s, running %v, Activity says %q", f.savedJournal(job.ID), f.running(), guidance)
				}
				checkKept()
				return
			}
			// Startup, finding the server running, reads the folders for what
			// it says: a copy saved as kept and moved away since isn't named,
			// nor is an earlier read's failure.
			if c.state.Kept && c.aside == nil && strings.Contains(job.Result.Guidance, aside) {
				t.Errorf("startup says %q", job.Result.Guidance)
			}
			if !f.running() || f.savedJournal(job.ID) == "rolled_back" {
				t.Fatalf("recovered before the undo: %s", f.savedJournal(job.ID))
			}
			var trace *restoreTrace
			if c.checkCut {
				checks := 0
				trace = f.traceRestore(func(helperContainer *fakedocker.Container, op, _ string, code int) int {
					if op == helper.RestoreCheckReturn {
						if checks++; checks == 2 {
							f.engine.RemoveContainer(helperContainer.ID)
							return 1
						}
					}
					return code
				})
			} else if c.checkFails {
				trace = f.traceRestore(func(_ *fakedocker.Container, op, _ string, code int) int {
					if op == helper.RestoreCheckReturn {
						return 1
					}
					return code
				})
			} else {
				trace = f.traceRestore(nil)
			}
			// A final journal saved without both notes fails its write.
			if _, err := f.jobs.DB.Exec(`CREATE TRIGGER final_with_notes BEFORE UPDATE OF recovery_json ON operations
				WHEN NEW.kind='restore' AND json_extract(NEW.recovery_json,'$.restore.status')='rolled_back' AND (
					coalesce(json_extract(NEW.result_json,'$.guidance'),'')='' OR NOT EXISTS (
					SELECT 1 FROM operations o WHERE o.kind='undo_restore' AND o.server_id=NEW.server_id AND
					instr(coalesce(json_extract(o.result_json,'$.undoNote'),''), json_extract(NEW.result_json,'$.guidance'))>0))
				BEGIN SELECT RAISE(ABORT, 'final journal without its notes'); END`); err != nil {
				t.Fatal(err)
			}
			if c.refusal != "" {
				if c.mounted != "" {
					previous := mountinfo.Table
					t.Cleanup(func() { mountinfo.Table = previous })
					mount := filepath.Join(f.data, c.mounted)
					mountinfo.Table = func() ([]byte, error) { return []byte("40 22 0:45 / " + mount + " rw - nfs4 server:/share rw\n"), nil }
				}
				before, journal, live := len(f.engine.Requests), f.savedJournal(job.ID), liveFiles(t, f.data)
				undo := f.run(t, ops.KindUndoRestore, nil)
				stopped := slices.ContainsFunc(f.engine.Requests[before:], func(request string) bool { return strings.Contains(request, f.game+"/stop") })
				if undo.Status != ops.Failed || undo.Error != c.refusal || stopped != c.afterStop || !f.running() ||
					f.savedJournal(job.ID) != journal || !maps.Equal(liveFiles(t, f.data), live) {
					t.Errorf("%s %q, stopped %v, running %v, journal %s", undo.Status, undo.Error, stopped, f.running(), f.savedJournal(job.ID))
				}
				return
			}
			if c.helperFails {
				f.engine.AddFault(`^POST .*/containers/create`, 500, false, 0)
				undo := f.run(t, ops.KindUndoRestore, nil)
				restore, _ := f.runner.Get(ctx, job.ID)
				if undo.Status != ops.Failed || !strings.Contains(restore.Result.Guidance, "couldn’t check the folder "+aside) ||
					!strings.Contains(undo.Result.UndoNote, restore.Result.Guidance) {
					t.Errorf("%s %q, Activity says %q, the undo %q", undo.Status, undo.Error, restore.Result.Guidance, undo.Result.UndoNote)
				}
				return
			}
			if c.finalFails {
				f.jobs.DB.Exec(`DROP TRIGGER final_with_notes`)
				f.jobs.DB.Exec(`CREATE TRIGGER final_fails BEFORE UPDATE OF recovery_json ON operations
					WHEN NEW.kind='restore' AND json_extract(NEW.recovery_json,'$.restore.status')='rolled_back'
					BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
				undo := f.run(t, ops.KindUndoRestore, nil)
				// The undo's note was part of the write that failed.
				if undo.Status != ops.Failed || strings.Contains(undo.Result.UndoNote+undo.Recovery.Undone, "finished undoing") {
					t.Errorf("%s %q, undo note %q", undo.Status, undo.Error, undo.Result.UndoNote+undo.Recovery.Undone)
				}
				return
			}
			if c.failStep != "" || c.lostStep != "" {
				f.mu.Lock()
				f.failStep = c.failStep
				f.mu.Unlock()
				if c.rereadUnsaved {
					// Startup read the folder and saved what it found; an earlier
					// attempt's finding is back, and the next read can't replace it.
					f.jobs.DB.Exec(`UPDATE operations SET recovery_json=json_set(recovery_json,'$.restore.kept',json(IIF(?,'true','false'))) WHERE id=?`, c.state.Kept, job.ID)
					f.jobs.DB.Exec(`CREATE TRIGGER read_unsaved BEFORE UPDATE OF recovery_json ON operations
						WHEN NEW.kind='restore' AND IFNULL(json_extract(OLD.recovery_json,'$.restore.kept'),0)!=IFNULL(json_extract(NEW.recovery_json,'$.restore.kept'),0)
						BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
				}
				if c.lostStep != "" {
					f.traceRestore(once(c.lostStep, "game", func(helperContainer *fakedocker.Container, _ int) int {
						if c.lostHelper {
							f.engine.RemoveContainer(helperContainer.ID)
						}
						return 1
					}))
				}
				undo := f.run(t, ops.KindUndoRestore, nil)
				restore, _ := f.runner.Get(ctx, job.ID)
				_, err := os.Stat(filepath.Join(f.data, aside))
				// It names the folder, which holds copies, and claims nothing
				// about the stage folder it can't know.
				if guidance := restore.Result.Guidance; undo.Status != ops.Failed || !strings.Contains(guidance, c.guidance) ||
					strings.Contains(guidance, aside) == c.plain || (err != nil) != (c.plain || c.rereadUnsaved && c.state.Kept) || strings.Contains(guidance, "still in") ||
					// The work that failed says the same.
					!strings.Contains(undo.Result.UndoNote, guidance) {
					t.Errorf("%s %q, Activity says %q", undo.Status, undo.Error, guidance)
				}
				if c.aside != nil {
					checkKept()
				}
				return
			}
			if c.unlinkOnStop != "" {
				stop := f.engine.OnStop
				f.engine.OnStop = func(container *fakedocker.Container) {
					if stop != nil {
						stop(container)
					}
					if container.ID == f.game {
						os.Remove(filepath.Join(f.data, c.unlinkOnStop))
					}
				}
			}
			undo := f.run(t, ops.KindUndoRestore, nil)
			want := "rolled_back put back kept"
			if c.plain {
				want = "rolled_back put back"
			} else if c.unreadable {
				want = "rolled_back put back unknown"
			}
			if undo.Status != ops.Succeeded || !f.running() || f.staged() || f.savedJournal(job.ID) != want {
				t.Fatalf("%s %q, running %v, journal %s", undo.Status, undo.Error, f.running(), f.savedJournal(job.ID))
			}
			checkKept()
			restore, _ := f.runner.Get(ctx, job.ID)
			for _, note := range []string{undo.Result.UndoNote, restore.Result.Guidance} {
				if c.unreadable {
					if !strings.Contains(note, "couldn’t check the folder "+aside) || !strings.Contains(note, "back in place") {
						t.Errorf("Activity says %q", note)
					}
					continue
				}
				if strings.Contains(note, "kept in") {
					t.Errorf("Activity claims what the folder holds now: %q", note)
				}
				// A folder is named exactly when copies were kept in it.
				if strings.Contains(note, aside) == c.plain || strings.Contains(note, "Anything Ludock moved") == c.plain || !strings.Contains(note, "back in place") {
					t.Errorf("Activity says %q", note)
				}
			}
			if got := trace.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.trace, "\n"))
			}
		})
	}
}

// A restore of two data folders is cut while moving each out, the server is
// started outside Ludock and the game recreates a file in the config folder.
// Undo sets that aside and returns the config folder, then fails on the
// world folder, and reading the world folder's set-aside folder afterwards
// fails too: the config folder's is still read, and the restore's Activity
// says where the newer copy is.
func TestUndoKeepsCopiesAcrossRoots(t *testing.T) {
	f := setup(t)
	twoRootBackup(t, f)
	var aside string
	job, _ := f.interruptedRestore(t, &restoreRecovery{Status: "replacing", Roots: []journal{{Phase: "moving_old"}, {Phase: "moving_old"}}}, func(stage string) {
		name := filepath.Base(stage)
		aside = helper.SetAsideName(strings.TrimPrefix(name, helper.StagePrefix))
		for _, root := range []string{"world", "config"} {
			relocate(t, filepath.Join(f.data, root, "large.dat"), filepath.Join(f.data, root, name, "old", "large.dat"))
		}
		if err := f.jobs.Docker.Start(ctx, f.game); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(f.data, "config", "large.dat"), []byte("new run"), 0o644)
	})
	// The newest folder is undone first.
	fail := once(helper.RestoreRollbackOld, "world", func(*fakedocker.Container, int) int { return 1 })
	unread := once(helper.RestoreKept, "world", func(*fakedocker.Container, int) int { return 1 })
	f.traceRestore(func(c *fakedocker.Container, op, root string, code int) int {
		return unread(c, op, root, fail(c, op, root, code))
	})
	undo := f.run(t, ops.KindUndoRestore, nil)
	restore, _ := f.runner.Get(ctx, job.ID)
	if guidance := restore.Result.Guidance; undo.Status != ops.Failed || !strings.Contains(guidance, "Anything Ludock moved out of the way of the server’s previous data went to the folder "+aside) ||
		read(t, filepath.Join(f.data, "config", aside, "large.dat")) != "new run" {
		t.Errorf("%s %q, journal %s, Activity says %q", undo.Status, undo.Error, f.savedJournal(job.ID), guidance)
	}
}

// liveFiles reads every file in the data, by path, with its contents.
func liveFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			name, _ := filepath.Rel(root, path)
			files[name] = read(t, path)
		}
		return nil
	})
	return files
}

func lastRestore(f *fixture) string {
	var id string
	f.jobs.DB.QueryRow("SELECT id FROM operations WHERE kind='restore' ORDER BY rowid DESC LIMIT 1").Scan(&id)
	return id
}

// A server started elsewhere between a root's move out and move in stops
// the restore before the backup's data goes into the running server's
// folder.
func TestRestoreStoppedBeforeMoveIn(t *testing.T) {
	f := setup(t)
	input := map[string]any{"backupId": twoRootBackup(t, f), "confirmation": "mc"}
	trace := f.traceRestore(startedAfter(helper.RestoreMoveOld, "world")(t, f))
	if job := f.run(t, "restore", input); job.Status != ops.Failed {
		t.Fatalf("%+v", job)
	}
	if slices.ContainsFunc(trace.lines(), func(line string) bool { return strings.HasPrefix(line, "moveNew ") }) {
		t.Fatal("moved the backup's data into a server started elsewhere")
	}
}

// An earlier restore's stage folder below the top of the data, as when
// another server's wider folder holds it, can be its server's only copy of
// the previous data. A whole or single-folder restore of what holds it is
// refused before the server stops, naming the folder; a stage that appears
// after that check is refused in the helper before anything moves, and the
// restore is undone. A backup still runs and says it left the folder out.
func TestRestoreKeepsOtherStages(t *testing.T) {
	f := setup(t)
	backupID, rootID := f.itemBackup(t)
	stage := filepath.Join(f.data, "world", helper.StageName("2f9619ff-8b86-4011-b42d-00c04fc964ff"))
	seed := func() {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(stage, "old"), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(stage, "old", "level.dat"), []byte("previous"), 0o644)
	}
	kept := func(how string) {
		t.Helper()
		if got := read(t, filepath.Join(stage, "old", "level.dat")); got != "previous" {
			t.Fatalf("%s: the other stage holds %q", how, got)
		}
		if got := read(t, filepath.Join(f.data, "world", "level.dat")); got != "newer" {
			t.Fatalf("%s: the live copy is %q", how, got)
		}
	}
	whole := map[string]any{"backupId": backupID, "confirmation": "mc", WarnInput: true}
	item := map[string]any{"backupId": backupID, "root": rootID, "item": "world", "createBackup": true, WarnInput: true}
	seed()
	// Stages at the top, and below a folder Ludock reserves there, are left
	// where they are and hide nothing.
	tops := []string{".ludock-cache/" + helper.StageName("3f9619ff-8b86-4011-b42d-00c04fc964ff")}
	for _, id := range []string{"4f9619ff-8b86-4011-b42d-00c04fc964ff", "5f9619ff-8b86-4011-b42d-00c04fc964ff", "6f9619ff-8b86-4011-b42d-00c04fc964ff"} {
		tops = append(tops, helper.StageName(id))
	}
	for _, top := range tops {
		os.MkdirAll(filepath.Join(f.data, top, "old"), 0o700)
	}
	for name, input := range map[string]map[string]any{"whole": whole, "item": item} {
		before := len(f.engine.Requests)
		job := f.run(t, "restore", input)
		stopped := slices.ContainsFunc(f.engine.Requests[before:], func(request string) bool { return strings.Contains(request, f.game+"/stop") })
		if job.Status != ops.Failed || job.Error != backups.StageInside(f.data+"/world").Message || job.Recovery.WarnUntil != 0 || stopped || !f.running() {
			t.Errorf("%s: %s %q, warned %v, stopped %v", name, job.Status, job.Error, job.Recovery.WarnUntil != 0, stopped)
		}
		kept(name)
	}
	if job := f.run(t, "backup", nil); job.Status != ops.Succeeded || job.Result.LeftOut != f.data+"/world" {
		t.Errorf("backup: %s %v", job.Status, job.Result)
	}
	// A warned backup checks again after the warning; a stage gone by then
	// isn't reported.
	plain, checks := f.engine.Exec, 0
	f.jobs.WarningLead = 100 * time.Millisecond
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		code := plain(c, cmd, stdin, stdout, stderr)
		if len(cmd) > 3 && cmd[2] == "file" && strings.Contains(cmd[3], `"operation":"mounts"`) {
			if checks++; checks == 1 {
				os.RemoveAll(stage)
			}
		}
		return code
	}
	if job := f.run(t, "backup", map[string]any{WarnInput: true}); job.Status != ops.Succeeded || checks != 2 || job.Result.LeftOut != "" {
		t.Errorf("warned backup: %s %v, %d checks", job.Status, job.Result, checks)
	}
	f.engine.Exec = plain
	os.RemoveAll(stage)
	for _, top := range tops {
		os.RemoveAll(filepath.Join(f.data, strings.Split(top, "/")[0]))
	}
	run := f.engine.Exec
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if len(cmd) > 3 && cmd[2] == "restore" && strings.Contains(cmd[3], `"operation":"`+helper.RestoreMoveOld+`"`) {
			seed()
		}
		return run(c, cmd, stdin, stdout, stderr)
	}
	delete(item, "createBackup")
	for name, input := range map[string]map[string]any{"whole": whole, "item": item} {
		delete(input, WarnInput)
		job := f.run(t, "restore", input)
		if job.Status != ops.Failed || job.Error != backups.StageInside("").Message || !f.running() || f.staged() {
			t.Errorf("appeared, %s: %s %q, running %v", name, job.Status, job.Error, f.running())
		}
		kept("appeared, " + name)
		os.RemoveAll(stage)
	}
}
