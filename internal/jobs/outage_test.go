package jobs

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
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
		if job.Status != ops.Succeeded || !job.Result.WaitedForDocker {
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
	if job.Status != ops.Succeeded || !job.Result.WaitedForDocker {
		t.Fatalf("%+v", job)
	}
}

// Docker goes away while an update or a restart stops the server, and
// stays away past the time Ludock waits. Gone before the stop was written,
// the stop never reached Docker: the work fails at once instead of holding
// the server for the five minutes a stop may take, the server keeps
// running, and no stop is left saved for recovery to wait for or finish.
// Gone once a restart's stop was written, before the answer, Docker may have
// it, so the stop stays saved; an update would wait it out.
func TestStopThatNeverArrived(t *testing.T) {
	for _, c := range []struct {
		kind  string
		input map[string]any
		// phases are those of the last inspection before the stop.
		phases  string
		written bool
	}{
		{"update", skipBackup, "'save_skipped','waiting_for_save'", false},
		{"restart", nil, "'stopping'", false},
		{"restart", nil, "", true},
	} {
		t.Run(fmt.Sprint(c.kind, " written ", c.written), func(t *testing.T) {
			f := setup(t)
			f.jobs.Docker.OutageLimit = 500 * time.Millisecond
			if c.written {
				var once sync.Once
				f.engine.Hang = func(request string) bool {
					if !strings.HasPrefix(request, "POST ") || !strings.HasSuffix(request, "/containers/"+f.game+"/stop") {
						return false
					}
					hang := false
					once.Do(func() {
						hang = true
						go f.engine.Disconnect(2 * time.Second)
					})
					return hang
				}
			} else {
				f.refuseAfter(2*time.Second, "/containers/"+f.game+"/json", c.phases, "%")
			}
			started := time.Now()
			job := f.run(t, c.kind, c.input)
			if job.Status != ops.Failed || !strings.HasPrefix(job.Error, "Ludock couldn’t reach Docker") || time.Since(started) > 15*time.Second ||
				job.Recovery.StopStarted != c.written {
				t.Fatalf("after %v: %+v", time.Since(started), job)
			}
			time.Sleep(2 * time.Second)
			if !f.running() {
				t.Fatal("server stopped")
			}
		})
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
	if job.Status != ops.Succeeded || !job.Result.WaitedForDocker || !f.running() {
		t.Fatalf("%+v", job)
	}
	if backups, err := f.jobs.Backups.List(ctx, f.serverID); err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
}

// The connection to Docker drops while a backup copies, as when a proxy in
// front of its socket restarts: Docker keeps the copy's exec, which exits
// with an error once its stream is cut. The backup copies again once Docker
// answers and finishes, instead of blaming the server's files.
func TestBackupCopyRidesOutDroppedConnection(t *testing.T) {
	f := setup(t)
	var once sync.Once
	exec := f.engine.Exec
	f.engine.Exec = func(c *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		if strings.Contains(strings.Join(cmd, " "), `"operation":"backup"`) {
			cut := false
			once.Do(func() { cut = true })
			if cut {
				go f.engine.Disconnect(2 * time.Second)
				time.Sleep(200 * time.Millisecond)
				return 1
			}
		}
		return exec(c, cmd, stdin, stdout, stderr)
	}
	job := f.run(t, "backup", nil)
	if job.Status != ops.Succeeded || !job.Result.WaitedForDocker || !f.running() {
		t.Fatalf("%+v", job)
	}
	if backups, err := f.jobs.Backups.List(ctx, f.serverID); err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
}

// The stream to a restore's unpacking helper is cut: by Docker going away
// for a moment, by the connection alone while Docker answers, as when a
// proxy in front of its socket reloads, or three times running. The helper
// says its input ended early, or read fewer records than were sent, which
// isn't the server's folders refusing.
// Only the stage folders were written, so they are made again and the
// archive unpacked again, at most three times; the restore then finishes,
// or fails saying the connection dropped and nothing was replaced. A helper
// that refuses what it is sent fails the restore without unpacking again.
func TestRestoreUnpackCutByDroppedConnection(t *testing.T) {
	for _, c := range []struct {
		name       string
		cuts       int
		disconnect bool
		status     int
		want       string
	}{
		{"Docker away", 1, true, helper.ExtractCutStatus, ""},
		{"connection alone", 1, false, helper.ExtractCutStatus, ""},
		// Cut between two records, the helper sees a clean end, but reports
		// reading fewer records than were sent.
		{"between records", 1, false, 0, ""},
		// The helper reads only the first record and says so, while the
		// rest is still accepted from the sender.
		{"first record only", 1, false, -1, ""},
		{"three times", 3, false, helper.ExtractCutStatus, "The connection to Docker dropped while the backup was unpacking"},
		{"refused", 1, false, 1, "A folder in the server’s data changed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			if err := os.WriteFile(filepath.Join(f.data, "large.dat"), make([]byte, 16<<20), 0o644); err != nil {
				t.Fatal(err)
			}
			backup := f.run(t, "backup", nil)
			os.WriteFile(filepath.Join(f.data, "large.dat"), []byte("changed"), 0o644)
			input := map[string]any{"backupId": backup.Result.BackupID, "confirmation": "mc"}
			var mu sync.Mutex
			cuts, extracts := 0, 0
			trace := f.traceRestore(nil)
			exec := f.engine.Exec
			f.engine.Exec = func(c2 *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
				if strings.Contains(strings.Join(cmd, " "), "restore-extract") {
					mu.Lock()
					extracts++
					cut := cuts < c.cuts
					cuts++
					mu.Unlock()
					if cut && c.status == -1 {
						first, _ := stdin.ReadBytes('\n')
						code := exec(c2, cmd, bufio.NewReader(bytes.NewReader(first)), stdout, stderr)
						io.Copy(io.Discard, stdin)
						return code
					}
					if cut {
						if c.disconnect {
							go f.engine.Disconnect(2 * time.Second)
						}
						time.Sleep(200 * time.Millisecond)
						return c.status
					}
				}
				return exec(c2, cmd, stdin, stdout, stderr)
			}
			job := f.run(t, "restore", input)
			restored := false
			if info, err := os.Stat(filepath.Join(f.data, "large.dat")); err == nil {
				restored = info.Size() == 16<<20
			}
			if c.want == "" {
				if job.Status != ops.Succeeded || !restored || !f.running() || f.staged() || extracts != 2 || job.Result.WaitedForDocker != c.disconnect {
					t.Fatalf("%+v, %d unpacks", job, extracts)
				}
				if lines := strings.Join(trace.lines(), "\n"); !strings.Contains(lines, "cleanup game: staging staging\nstage game: staging staging") {
					t.Fatal(lines)
				}
				return
			}
			wantExtracts := 1
			if c.status == helper.ExtractCutStatus {
				wantExtracts = 3
			}
			if job.Status != ops.Failed || !strings.HasPrefix(job.Error, c.want) || restored || !f.running() || f.staged() || extracts != wantExtracts {
				t.Fatalf("%+v, %d unpacks", job, extracts)
			}
		})
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
	if job.Status != ops.Succeeded || !job.Result.WaitedForDocker {
		t.Fatalf("%+v", job)
	}
	if server := f.containerNamed("mc"); server == nil || server.ID == f.game || !server.State.Running {
		t.Fatalf("%+v", server)
	}
}
