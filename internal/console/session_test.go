package console

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

type fakeChannel struct {
	mu       sync.Mutex
	messages []string
	closed   int
}

func (c *fakeChannel) Send(kind, data string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == 0 {
		c.messages = append(c.messages, kind+":"+data)
	}
}
func (c *fakeChannel) Close(code int, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed == 0 {
		c.closed = code
	}
}
func (c *fakeChannel) Open() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.closed == 0 }
func (c *fakeChannel) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.messages, "\n")
}

type sessionFixture struct {
	engine   *fakedocker.Engine
	sessions *Sessions
	serverID string
	friend   *db.SessionUser
	admin    *db.SessionUser
	// adminSession is the admin's token hash.
	adminSession string
	authz        *authz.Service
	// respond answers the server's RCON commands.
	respond func(command string) string
}

func sessionSetup(t *testing.T) *sessionFixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	f := &sessionFixture{respond: func(string) string { return "" }}
	port, err := fakedocker.ServeRCON("hunter2-secret", func(command string) string { return f.respond(command) })
	if err != nil {
		t.Fatal(err)
	}
	// The console connects directly, without a helper container.
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port)},
		Env:    []string{"RCON_PASSWORD=hunter2-secret"}})
	engine.Update(id, func(c *fakedocker.Container) { c.Logs = []string{"joined", "rcon password hunter2-secret ok"} })
	d := dbtest.Open(t)
	admin := &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	friend := &db.SessionUser{ID: "friend", Username: "friend", Role: db.RoleOperator}
	for _, user := range []*db.SessionUser{admin, friend} {
		d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1}, PasswordHash: "x"})
	}
	adminSession := dbtest.Session(t, d, admin.ID)
	client := docker.New(engine.Socket)
	store := &identity.Store{DB: d}
	authorization := &authz.Service{DB: d, Servers: store}
	service := &servers.Service{DB: d, Identity: store, Authz: authorization, Observer: &servers.Observer{Docker: client}}
	views, err := service.List(ctx, admin)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	authorization.SetUserGrants(ctx, friend.ID, []authz.GrantInput{{ServerID: views[0].ID,
		Capabilities: []authz.Capability{authz.ServerView, authz.LogsRead, authz.ConsoleExecute}}}, admin, adminSession)
	f.engine, f.serverID, f.friend, f.admin, f.adminSession, f.authz = engine, views[0].ID, friend, admin, adminSession, authorization
	f.sessions = &Sessions{DB: d, Authz: authorization, Identity: store, Servers: service, Locks: locks.New(),
		Runtime: &Runtime{Docker: client, pollInterval: time.Millisecond}, IdleCheck: 20 * time.Millisecond}
	return f
}

func (f *sessionFixture) validate(user *db.SessionUser) func(context.Context) *db.SessionUser {
	return func(context.Context) *db.SessionUser { return user }
}

func TestLogsAreRedactedAndStopOnRevocation(t *testing.T) {
	f := sessionSetup(t)
	channel := &fakeChannel{}
	streamCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.sessions.Logs(streamCtx, channel, f.validate(f.friend), f.serverID, ""); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(channel.text(), "ok") {
		time.Sleep(5 * time.Millisecond)
	}
	text := channel.text()
	if !strings.Contains(text, "joined") || strings.Contains(text, "hunter2-secret") || !strings.Contains(text, "[redacted]") {
		t.Fatal(text)
	}
	f.authz.SetUserGrants(ctx, f.friend.ID, nil, f.admin, f.adminSession)
	time.Sleep(100 * time.Millisecond)
	if channel.Open() {
		t.Fatal("stream stayed open after revocation")
	}
	cancel()
	<-done
}

// Console runs commands and rejects without access.
func TestConsoleAccess(t *testing.T) {
	f := sessionSetup(t)
	f.respond = func(string) string { return "There are 0 players (password hunter2-secret)" }
	channel := &fakeChannel{}
	input := make(chan string, 2)
	input <- `{"type":"input","data":"list"}`
	input <- `not json`
	close(input)
	f.sessions.Console(ctx, channel, f.validate(f.friend), f.serverID, GameMode, input, "")
	text := channel.text()
	if !strings.Contains(text, "system:Connected to Minecraft RCON") || !strings.Contains(text, "stdout:There are 0 players (password [redacted]") || strings.Contains(text, "hunter2") ||
		!strings.Contains(text, "error:Invalid message format") {
		t.Fatal(text)
	}
	shell := &fakeChannel{}
	f.sessions.Console(ctx, shell, f.validate(f.friend), f.serverID, ShellMode, make(chan string), "")
	if shell.closed != ClosePolicy || !strings.Contains(shell.text(), "unavailable or access was denied") {
		t.Fatal(shell.text())
	}
	var audits int
	f.sessions.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action LIKE 'server.game-command.%'").Scan(&audits)
	if audits != 2 {
		t.Fatal(audits)
	}
}

func TestCommandsRunOnlyOnceAudited(t *testing.T) {
	f := sessionSetup(t)
	ran := false
	f.respond = func(string) string {
		ran = true
		return ""
	}
	if _, err := f.sessions.DB.Exec(`CREATE TRIGGER no_command_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'server.game-command.started' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`); err != nil {
		t.Fatal(err)
	}
	channel := &fakeChannel{}
	input := make(chan string, 1)
	input <- `{"type":"input","data":"stop"}`
	close(input)
	f.sessions.Console(ctx, channel, f.validate(f.friend), f.serverID, GameMode, input, "")
	if ran || !strings.Contains(channel.text(), "could not record this command") {
		t.Fatal(ran, channel.text())
	}
	// A connection that cannot be audited is not opened.
	f.sessions.DB.Exec(`CREATE TRIGGER no_open_audit BEFORE INSERT ON audit_log
		WHEN NEW.action = 'server.game-console.opened' BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	closed := &fakeChannel{}
	f.sessions.Console(ctx, closed, f.validate(f.friend), f.serverID, GameMode, make(chan string), "")
	if closed.closed != CloseInternal || strings.Contains(closed.text(), "Connected to") {
		t.Fatal(closed.closed, closed.text())
	}
}

// Commands the browser sent run even if it leaves while an earlier one is
// still running, as when someone sends a command and opens another page.
// They still need the sender's access when they run.
func TestCommandsFinishAfterTheBrowserLeaves(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		f := sessionSetup(t)
		channel := &fakeChannel{}
		// Like the socket's, the session's context ends when the browser leaves.
		session, leave := context.WithCancel(ctx)
		var mu sync.Mutex
		var ran []string
		f.respond = func(command string) string {
			mu.Lock()
			ran = append(ran, command)
			first := len(ran) == 1
			mu.Unlock()
			if first {
				channel.Close(1001, "")
				leave()
				if revoked {
					f.authz.SetUserGrants(ctx, f.friend.ID, nil, f.admin, f.adminSession)
				}
			}
			return ""
		}
		input := make(chan string, 2)
		input <- `{"type":"input","data":"save-all"}`
		input <- `{"type":"input","data":"stop"}`
		close(input)
		f.sessions.Console(session, channel, f.validate(f.friend), f.serverID, GameMode, input, "")
		want := "save-all,stop"
		if revoked {
			want = "save-all"
		}
		if got := strings.Join(ran, ","); got != want {
			t.Fatalf("revoked %v: ran %q, want %q", revoked, got, want)
		}
	}
}
