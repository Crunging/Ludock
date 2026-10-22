package events

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

type recorder struct {
	mu       sync.Mutex
	user     *db.SessionUser
	messages []Event
	closed   bool
}

func (r *recorder) subscriber(d *db.DB) *Subscriber {
	return &Subscriber{
		Validate: func(context.Context) *db.SessionUser { return r.user },
		Send:     func(event Event) { r.mu.Lock(); r.messages = append(r.messages, event); r.mu.Unlock() },
		Close:    func(string) { r.mu.Lock(); r.closed = true; r.mu.Unlock() },
	}
}

func (r *recorder) events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.messages...)
}

type fixture struct {
	engine        *fakedocker.Engine
	hub           *Hub
	authz         *authz.Service
	first, second string
	refreshes     atomic.Int32
	admin, friend *db.SessionUser
	stranger      *db.SessionUser
	// adminSession is the admin's token hash.
	adminSession string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	authorization := &authz.Service{DB: d, Servers: store}
	service := &servers.Service{DB: d, Identity: store, Authz: authorization, Observer: &servers.Observer{Docker: docker.New(engine.Socket)}}
	f := &fixture{engine: engine, authz: authorization,
		admin:    &db.SessionUser{ID: "admin", Role: db.RoleAdmin},
		friend:   &db.SessionUser{ID: "friend", Role: db.RoleViewer},
		stranger: &db.SessionUser{ID: "stranger", Role: db.RoleViewer}}
	for _, user := range []*db.SessionUser{f.admin, f.friend, f.stranger} {
		d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.ID, Role: user.Role, CreatedAt: 1}, PasswordHash: "x"})
	}
	f.adminSession = dbtest.Session(t, d, f.admin.ID)
	f.first = engine.AddContainer(fakedocker.GameOptions{ID: "first", Name: "first", Image: "itzg/minecraft-server", Running: true})
	f.second = engine.AddContainer(fakedocker.GameOptions{ID: "second", Name: "second", Image: "itzg/minecraft-server", Running: true})
	if _, err := service.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	f.hub = &Hub{Docker: docker.New(engine.Socket), Identity: store, Authz: authorization,
		Refresh: func(ctx context.Context) error { f.refreshes.Add(1); _, err := service.Refresh(ctx); return err }}
	t.Cleanup(f.hub.Stop)
	logical, _ := store.List(ctx, "")
	for _, server := range logical {
		if server.ContainerID == "first" {
			authorization.SetUserGrants(ctx, "friend", []authz.GrantInput{{ServerID: server.ID, Capabilities: []authz.Capability{authz.ServerView}}}, f.admin, f.adminSession)
		}
	}
	return f
}

func (f *fixture) client(user *db.SessionUser) *recorder {
	r := &recorder{user: user}
	f.hub.mu.Lock()
	if f.hub.subscribers == nil {
		f.hub.subscribers = map[*Subscriber]bool{}
	}
	f.hub.subscribers[r.subscriber(nil)] = true
	f.hub.mu.Unlock()
	return r
}

func TestDecodeSkipsMalformedAndBoundsRecords(t *testing.T) {
	var events []dockerEvent
	Decode(strings.NewReader("malformed\nnull\n[]\n{\"Action\":\"start\"}\n"), func(event dockerEvent) { events = append(events, event) })
	if len(events) != 1 {
		t.Fatal(events)
	}
	if err := Decode(strings.NewReader(strings.Repeat("a", 1<<20+1)), func(dockerEvent) {}); err == nil {
		t.Fatal("accepted oversized record")
	}
}

func TestScopedDelivery(t *testing.T) {
	f := setup(t)
	owner, allowed, denied := f.client(f.admin), f.client(f.friend), f.client(f.stranger)
	f.hub.dispatch(ctx, []change{{"start", "second", 1}})
	if len(owner.events()) != 1 || len(allowed.events()) != 0 || len(denied.events()) != 0 {
		t.Fatal(owner.events(), allowed.events(), denied.events())
	}
	f.hub.dispatch(ctx, []change{{"stop", "first", 1}})
	if events := allowed.events(); len(events) != 1 || !identity.ValidID(events[0].ServerID) || events[0].Action != "stop" {
		t.Fatal(events)
	}
	// A removed server sends former viewers a content-free refresh.
	f.engine.RemoveContainer("first")
	f.hub.dispatch(ctx, []change{{"destroy", "first", 2}})
	if events := allowed.events(); events[len(events)-1] != (Event{Action: "refresh", Time: 2}) {
		t.Fatal(events)
	}
	if len(denied.events()) != 0 {
		t.Fatal("stranger saw an event")
	}
}

func TestIgnoresExecAndUnknownContainers(t *testing.T) {
	if _, ok := stateChange(dockerEvent{Action: "exec_create: secret-password"}); ok {
		t.Fatal("exec event accepted")
	}
	f := setup(t)
	owner := f.client(f.admin)
	f.hub.dispatch(ctx, []change{{"start", "unknown-container", 1}})
	if len(owner.events()) != 0 {
		t.Fatal(owner.events())
	}
}

// Rechecks grants and accounts before each event.
func TestEventsRecheckAccess(t *testing.T) {
	f := setup(t)
	socket := f.client(f.friend)
	f.authz.SetUserGrants(ctx, "friend", nil, f.admin, f.adminSession)
	f.hub.dispatch(ctx, []change{{"start", "first", 1}})
	if len(socket.events()) != 0 {
		t.Fatal(socket.events())
	}
	f.authz.DB.UpdateUserAccess(ctx, "friend", db.RoleOperator, true)
	f.hub.dispatch(ctx, []change{{"start", "first", 1}})
	if !socket.closed {
		t.Fatal("disabled account not closed")
	}
}

func TestBurstSharesOneRefreshPerBatch(t *testing.T) {
	f := setup(t)
	owner := &recorder{user: f.admin}
	unsubscribe := f.hub.Subscribe(owner.subscriber(nil))
	defer unsubscribe()
	// Let the stream connect before Docker emits the burst.
	time.Sleep(100 * time.Millisecond)
	for _, id := range []string{"first", "second"} {
		f.engine.Update(id, func(c *fakedocker.Container) { c.State.Running = false; c.State.Status = "exited" })
	}
	client := docker.New(f.engine.Socket)
	client.Start(ctx, "first")
	client.Start(ctx, "second")
	client.Stop(ctx, "first", nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(owner.events()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(owner.events()) < 2 || f.refreshes.Load() > 3 {
		t.Fatalf("events %v, refreshes %d", owner.events(), f.refreshes.Load())
	}
}
