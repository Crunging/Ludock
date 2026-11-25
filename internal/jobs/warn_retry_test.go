package jobs

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/ops"
)

// flakyConsole adds a Minecraft server whose RCON console drops the first
// dropped connections, as a console briefly unreachable does, and records
// the commands it receives after that.
func (f *fixture) flakyConsole(t *testing.T, dropped int32) (string, func() []string, *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	var received []string
	port, err := fakedocker.ServeRCON("rcon-secret", func(command string) string {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, command)
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	attempts := &atomic.Int32{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if attempts.Add(1) <= dropped {
				conn.Close()
				continue
			}
			go func() {
				defer conn.Close()
				upstream, err := net.Dial("tcp", fmt.Sprint("127.0.0.1:", port))
				if err != nil {
					return
				}
				defer upstream.Close()
				go io.Copy(upstream, conn)
				io.Copy(conn, upstream)
			}()
		}
	}()
	f.engine.AddContainer(fakedocker.GameOptions{Name: "console", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)},
		Env:    []string{"RCON_PASSWORD=rcon-secret"}})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	views, err := f.jobs.Servers.List(ctx, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "console" {
			return view.ID, func() []string {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), received...)
			}, attempts
		}
	}
	t.Fatal("console server not discovered")
	return "", nil, nil
}

func (f *fixture) warnedRestart(t *testing.T, serverID string) *ops.Job {
	t.Helper()
	server, _ := f.jobs.Identity.Get(ctx, serverID)
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: serverID, ActorID: "admin", Kind: "restart",
		BindingRevision: server.BindingRevision, Input: map[string]any{WarnInput: true}})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// A warning the console couldn't take at first is sent again until it
// goes out, and players get their whole minute from then; until it is sent
// the operation is still warning players, not counting down.
func TestWarningIsRetriedUntilSent(t *testing.T) {
	f := setup(t)
	f.jobs.Poll = 20 * time.Millisecond
	f.jobs.WarningLead = 2 * time.Second
	f.withPlayers(1)
	serverID, received, attempts := f.flakyConsole(t, 3)
	job := f.warnedRestart(t, serverID)
	waitFor(t, f.runner, job.ID, func(current *ops.Job) bool { return current.Phase == ops.PhaseWarningPlayers })
	var sentAt time.Time
	waitFor(t, f.runner, job.ID, func(current *ops.Job) bool {
		if current.Recovery.WarnSent && sentAt.IsZero() {
			sentAt = time.Now()
		}
		return current.Recovery.WarnSent || current.Phase != ops.PhaseWarningPlayers
	})
	done := finished(t, f.runner, job.ID)
	if done.Status != ops.Succeeded || !done.Recovery.WarnSent || done.Recovery.WarnFailed || done.Result.WarnFailedNote != "" ||
		time.Since(sentAt) < 1500*time.Millisecond || attempts.Load() < 4 {
		t.Fatalf("%+v, %d attempts", done, attempts.Load())
	}
	if got := strings.Join(received(), "\n"); !strings.Contains(got, "Server restart in 1 minute.") {
		t.Fatal(got)
	}
}

// A warning that never goes out within its minute of trying lets the work
// go ahead, and Activity says players weren't warned.
func TestWarningThatNeverSendsSaysSo(t *testing.T) {
	f := setup(t)
	f.jobs.Poll = 20 * time.Millisecond
	f.jobs.WarningLead = 500 * time.Millisecond
	f.withPlayers(1)
	serverID, received, _ := f.flakyConsole(t, 1<<30)
	done := finished(t, f.runner, f.warnedRestart(t, serverID).ID)
	if done.Status != ops.Succeeded || done.Recovery.WarnSent || done.Result.WarnFailedNote != "Couldn’t warn players before the restart." {
		t.Fatalf("%+v", done)
	}
	if got := received(); len(got) != 0 {
		t.Fatal(got)
	}
}

// Cancelling while the warning is still being retried calls the work off,
// and players, who never heard a warning, hear no call-off.
func TestCancelWhileRetryingWarning(t *testing.T) {
	f := setup(t)
	f.jobs.Poll = 20 * time.Millisecond
	f.jobs.WarningLead = time.Minute
	f.withPlayers(1)
	serverID, received, attempts := f.flakyConsole(t, 1<<30)
	job := f.warnedRestart(t, serverID)
	waitFor(t, f.runner, job.ID, func(current *ops.Job) bool { return current.Phase == ops.PhaseWarningPlayers && attempts.Load() >= 2 })
	if err := f.jobs.Cancel(ctx, f.admin, f.adminSession, serverID, job.ID); err != nil {
		t.Fatal(err)
	}
	done := finished(t, f.runner, job.ID)
	time.Sleep(100 * time.Millisecond)
	if done.Status != ops.Cancelled || len(received()) != 0 {
		t.Fatalf("%+v, %q", done, received())
	}
}

// Ludock restarted while a restart was still trying to send its warning.
// The restart resumes queued, which doesn't keep that phase, and tries to
// send the warning again rather than counting down one nobody heard.
func TestResumedWarningIsSentAgain(t *testing.T) {
	f := setup(t)
	f.jobs.Poll = 20 * time.Millisecond
	f.jobs.WarningLead = 300 * time.Millisecond
	// Long after the time it would have given, nobody was told, so it isn't
	// too late either.
	for _, until := range []time.Duration{time.Minute, -10 * time.Minute} {
		runner, id := f.interruptedWarning(t, ops.PhaseWarningPlayers, ops.Recovery{WarnUntil: time.Now().Add(until).UnixMilli()})
		done := finished(t, runner, id)
		if done.Status != ops.Succeeded || done.Recovery.WarnSent || done.Result.WarnFailedNote != "Couldn’t warn players before the restart." {
			t.Fatalf("%+v", done)
		}
		runner.Stop()
	}
}

// A server whose sharing is paused, awaiting review of a change to its game
// or data folders, hears only an administrator's own actions: their warned
// restart warns players, but its call-off, sent on Ludock's authority, isn't.
func TestCallOffSkipsAServerAwaitingReview(t *testing.T) {
	f := setup(t)
	f.jobs.Poll = 20 * time.Millisecond
	f.jobs.WarningLead = time.Minute
	f.withPlayers(1)
	serverID, received, _ := f.flakyConsole(t, 0)
	if _, err := f.jobs.DB.Exec("UPDATE logical_servers SET sharing_paused=1 WHERE id=?", serverID); err != nil {
		t.Fatal(err)
	}
	job := f.warnedRestart(t, serverID)
	waitFor(t, f.runner, job.ID, func(current *ops.Job) bool { return current.Recovery.WarnSent })
	if err := f.jobs.Cancel(ctx, f.admin, f.adminSession, serverID, job.ID); err != nil {
		t.Fatal(err)
	}
	finished(t, f.runner, job.ID)
	got := strings.Join(received(), "\n")
	if !strings.Contains(got, "Server restart in 1 minute.") || strings.Contains(got, "cancelled") {
		t.Fatal(got)
	}
}
