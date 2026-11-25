package jobs

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
)

// commandServer adds a Minecraft server with an RCON console that records
// the commands it receives and answers with reply.
func (f *fixture) commandServer(t *testing.T, reply string) (string, string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var received []string
	port, err := fakedocker.ServeRCON("rcon-secret", func(command string) string {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, command)
		return reply
	})
	if err != nil {
		t.Fatal(err)
	}
	containerID := f.engine.AddContainer(fakedocker.GameOptions{Name: "console", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port)},
		Env:    []string{"RCON_PASSWORD=rcon-secret", "DB_PASS=db-secret-value"}})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	views, err := f.jobs.Servers.List(ctx, f.admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Name == "console" {
			return view.ID, containerID, func() []string {
				mu.Lock()
				defer mu.Unlock()
				return append([]string(nil), received...)
			}
		}
	}
	t.Fatal("console server not discovered")
	return "", "", nil
}

// scheduleCommand saves a command schedule for owner and runs the operation
// it would queue.
func (f *fixture) scheduleCommand(t *testing.T, serverID, owner, command string) *ops.Job {
	t.Helper()
	server, _ := f.jobs.Identity.Get(ctx, serverID)
	input := fmt.Sprintf(`{"kind":"recurring","action":"command","command":%q,"createBackup":false,"enabled":true,"time":"08:00","timezone":"UTC","days":[1]}`, command)
	scheduleID := "44444444-4444-4444-8444-444444444444"
	if _, err := f.jobs.DB.Exec(`INSERT INTO schedules(id,server_id,owner_id,input_json,binding_revision,created_at,changed_at) VALUES(?,?,?,?,?,1,1)`,
		scheduleID, serverID, owner, input, server.BindingRevision); err != nil {
		t.Fatal(err)
	}
	job, err := f.runner.Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: serverID, ActorID: owner, Kind: "command",
		BindingRevision: server.BindingRevision, Input: map[string]any{"scheduleId": scheduleID, "scheduleRevision": 1}})
	if err != nil {
		t.Fatal(err)
	}
	return finished(t, f.runner, job.ID)
}

// A scheduled command reaches the game once, and only a redacted, bounded,
// single-line reply is kept; the operation never stores the command.
func TestScheduledCommandKeepsRedactedReply(t *testing.T) {
	f := setup(t)
	serverID, _, received := f.commandServer(t, "Saved the game\nusing db-secret-value "+strings.Repeat("é", 400))
	job := f.scheduleCommand(t, serverID, "admin", "save-all")
	if job.Status != ops.Succeeded || !job.Result.Sent {
		t.Fatalf("%+v", job)
	}
	if got := received(); len(got) != 1 || got[0] != "save-all" {
		t.Fatal(got)
	}
	reply := job.Result.Reply
	if !strings.HasPrefix(reply, "Saved the game using [redacted] é") || !strings.HasSuffix(reply, "é…") ||
		len(reply) > MaxCommandReply || strings.Contains(reply, "db-secret") {
		t.Fatal(reply)
	}
	var input string
	f.jobs.DB.QueryRow("SELECT input_json FROM operations WHERE id=?", job.ID).Scan(&input)
	if strings.Contains(input, "save-all") {
		t.Fatal(input)
	}
}

// A server stopped after its command was queued is skipped, not failed.
func TestScheduledCommandSkipsStoppedServer(t *testing.T) {
	f := setup(t)
	serverID, containerID, received := f.commandServer(t, "")
	f.engine.Update(containerID, func(c *fakedocker.Container) { c.State.Running, c.State.Status = false, "exited" })
	job := f.scheduleCommand(t, serverID, "admin", "save-all")
	if job.Status != ops.Skipped || job.Result.Guidance != "Skipped: the server wasn't running" || len(received()) != 0 {
		t.Fatalf("%+v", job)
	}
}

// An owner who lost console access after the run was queued sends nothing.
func TestScheduledCommandRefusedWithoutConsoleAccess(t *testing.T) {
	f := setup(t)
	serverID, _, received := f.commandServer(t, "")
	operator := db.User{ID: "operator", Username: "operator", Role: db.RoleMember, CreatedAt: 1}
	f.jobs.DB.CreateUser(ctx, db.UserRecord{User: operator, PasswordHash: "x"})
	if err := f.jobs.Authz.Share(ctx, f.admin, f.adminSession, operator.ID, serverID, []authz.Capability{authz.ServerView, authz.SchedulesManage}); err != nil {
		t.Fatal(err)
	}
	job := f.scheduleCommand(t, serverID, operator.ID, "say hello")
	if job.Status != ops.Failed || job.Error != "You can’t do that on this server" || len(received()) != 0 {
		t.Fatalf("%+v %v", job, received())
	}
}

// A command whose server another request holds is skipped at once, never
// sent after that work.
func TestScheduledCommandSkipsBusyServer(t *testing.T) {
	f := setup(t)
	serverID, _, received := f.commandServer(t, "")
	release, err := f.jobs.Locks.Acquire([]string{locks.ServerKey(serverID)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	started := time.Now()
	job := f.scheduleCommand(t, serverID, "admin", "save-all")
	if job.Status != ops.Skipped || job.Result.Guidance != "Skipped: the server was busy with other work" ||
		time.Since(started) > commandTimeout/2 || len(received()) != 0 {
		t.Fatalf("%+v", job)
	}
}
