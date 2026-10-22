package console

import (
	"bufio"
	"context"
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
}

func sessionSetup(t *testing.T) *sessionFixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Env: []string{"RCON_PASSWORD=hunter2-secret"}})
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
	return &sessionFixture{engine: engine, serverID: views[0].ID, friend: friend, admin: admin, adminSession: adminSession, authz: authorization,
		sessions: &Sessions{DB: d, Authz: authorization, Identity: store, Servers: service, Locks: locks.New(),
			Runtime: &Runtime{Docker: client, pollInterval: time.Millisecond}, IdleCheck: 20 * time.Millisecond}}
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
	f.engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		stdout([]byte("There are 0 players (password hunter2-secret)"))
		return 0
	}
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
	f.engine.Exec = func(*fakedocker.Container, []string, *bufio.Reader, func([]byte), func([]byte)) int {
		ran = true
		return 0
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
