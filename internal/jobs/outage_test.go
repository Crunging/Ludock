package jobs

import (
	"bufio"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
)

// Docker restarts for a few seconds just as the server stops, which cuts the
// stop's answer, during a stop, a restart (with and without a backup), a
// backup, and an update. Each waits for Docker, finds out from Docker what
// happened, finishes, and says in its result that it waited.
func TestWorkRidesOutDockerRestart(t *testing.T) {
	for _, c := range []struct {
		kind    string
		input   map[string]any
		running bool
	}{
		{"stop", nil, false},
		{"restart", nil, true},
		{"restart", map[string]any{"createBackup": true}, true},
		{"backup", nil, true},
		{"update", skipBackup, true},
	} {
		f := setup(t)
		var once sync.Once
		f.engine.OnStop = func(c *fakedocker.Container) {
			if c.ID == f.game {
				once.Do(func() { go f.engine.Outage(2 * time.Second) })
			}
		}
		job := f.run(t, c.kind, c.input)
		if job.Status != ops.Succeeded || job.Result["waitedForDocker"] != true {
			t.Fatalf("%s %v: %+v", c.kind, c.input, job)
		}
		if server := f.containerNamed("mc"); server == nil || server.State.Running != c.running {
			t.Fatalf("%s %v: server %+v", c.kind, c.input, server)
		}
	}
}

// refuseBeforeStop makes Docker refuse connections for duration from the
// moment the server is inspected right before its stop, which follows the
// save step, so the stop request never reaches Docker.
func (f *fixture) refuseBeforeStop(duration time.Duration) {
	f.refuseAfter(duration, "/containers/"+f.game+"/json", "'save_skipped','waiting_for_save'", "%")
}

// refuseAfter makes Docker refuse connections for duration from the first GET
// whose path ends in suffix once the running operation reached one of phases.
// like further matches the operation's saved recovery state.
func (f *fixture) refuseAfter(duration time.Duration, suffix, phases, like string) {
	var mu sync.Mutex
	refused := false
	f.engine.Hang = func(request string) bool {
		mu.Lock()
		defer mu.Unlock()
		if refused || !strings.HasPrefix(request, "GET ") || !strings.HasSuffix(request, suffix) {
			return false
		}
		var saved int
		f.jobs.DB.QueryRow("SELECT COUNT(*) FROM operations WHERE status='running' AND phase IN ("+phases+") AND recovery_json LIKE ?", like).Scan(&saved)
		if saved > 0 {
			refused = true
			f.engine.Refuse(duration)
		}
		return false
	}
}

// Docker goes away just before an update stops the server and is back
// within the time Ludock waits: the stop is sent once Docker answers, after
// checking the server again, and the update finishes.
func TestUpdateStopWaitsForDocker(t *testing.T) {
	f := setup(t)
	f.refuseBeforeStop(2 * time.Second)
	job := f.run(t, "update", skipBackup)
	if job.Status != ops.Succeeded || job.Result["waitedForDocker"] != true {
		t.Fatalf("%+v", job)
	}
}

// Docker stays away past the time Ludock waits, so an update's stop never
// reaches it. The update fails at once instead of holding the server for
// the five minutes a stop that did arrive may take, and the server keeps
// running.
func TestUpdateStopThatNeverArrived(t *testing.T) {
	f := setup(t)
	f.jobs.Docker.OutageLimit = 500 * time.Millisecond
	f.refuseBeforeStop(2 * time.Second)
	started := time.Now()
	job := f.run(t, "update", skipBackup)
	if job.Status != ops.Failed || !strings.HasPrefix(job.Error, "Ludock couldn’t reach Docker") || time.Since(started) > 15*time.Second {
		t.Fatalf("after %v: %+v", time.Since(started), job)
	}
	time.Sleep(2 * time.Second)
	if !f.running() {
		t.Fatal("server stopped")
	}
}

// Docker restarts while a backup copies the server's files, which removes
// the copy's helper and cuts its stream. The backup removes what it had
// written, copies again once Docker is back, and finishes.
func TestBackupCopyRidesOutDockerRestart(t *testing.T) {
	f := setup(t)
	var once sync.Once
	exec := f.engine.Exec
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if strings.Contains(strings.Join(cmd, " "), `"operation":"backup"`) {
			cut := false
			once.Do(func() { cut = true })
			if cut {
				go f.engine.Outage(2 * time.Second)
				time.Sleep(200 * time.Millisecond)
				return 1
			}
		}
		return exec(c, cmd, stdin, stdout, stderr)
	}
	job := f.run(t, "backup", nil)
	if job.Status != ops.Succeeded || job.Result["waitedForDocker"] != true || !f.running() {
		t.Fatalf("%+v", job)
	}
	if backups, err := f.jobs.Backups.List(ctx, f.serverID); err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
}

// Docker goes away just before an update starts the new container, after
// checking that the original is still stopped. The start is checked and sent
// again once Docker answers, and the update finishes.
func TestUpdateReplacementStartWaitsForDocker(t *testing.T) {
	f := setup(t)
	// The last read before the start looks for other writers.
	f.refuseAfter(2*time.Second, "/containers/json", "'creating'", `%"replacement":%`)
	job := f.run(t, "update", skipBackup)
	if job.Status != ops.Succeeded || job.Result["waitedForDocker"] != true {
		t.Fatalf("%+v", job)
	}
	if server := f.containerNamed("mc"); server == nil || server.ID == f.game || !server.State.Running {
		t.Fatalf("%+v", server)
	}
}
