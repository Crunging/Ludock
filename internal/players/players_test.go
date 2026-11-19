package players

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// A console command changes who is online while a reading is still running.
// The reading asked for after the command runs once that one ends, so the
// change shows without waiting for the next poll.
func TestCheckDuringReadingReadsAgain(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	answered, release := make(chan struct{}), make(chan struct{})
	online := "Mara"
	port, err := fakedocker.ServeRCON("secret", func(string) string {
		name := online
		if name == "Mara" {
			answered <- struct{}{}
			<-release
		}
		return "There are 1 of a max of 10 players online: " + name
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port), "ludock.query": "disabled"},
		Env:    []string{"RCON_PASSWORD=secret"}})
	players, server := gameService(t, engine)
	players.Check(ctx, server)
	<-answered
	online = "Theo"
	players.Check(ctx, server)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		players.mu.Lock()
		reading := players.readings[server]
		players.mu.Unlock()
		if reading != nil && len(reading.Names) == 1 && reading.Names[0] == "Theo" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the second check was dropped:", reading)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// gameService reads players from the games on engine, which has one server,
// and returns that server's ID.
func gameService(t *testing.T, engine *fakedocker.Engine) (*Service, string) {
	t.Helper()
	ctx := context.Background()
	d := dbtest.Open(t)
	client := docker.New(engine.Socket)
	store := &identity.Store{DB: d}
	observer := &servers.Observer{Docker: client}
	service := &servers.Service{DB: d, Identity: store, Authz: &authz.Service{DB: d, Servers: store}, Observer: observer}
	admin := &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	d.CreateUser(ctx, db.UserRecord{User: db.User{ID: admin.ID, Username: admin.Username, Role: admin.Role, CreatedAt: 1}, PasswordHash: "x"})
	views, err := service.List(ctx, admin)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	players := &Service{Servers: service, Identity: store,
		Source: &GameSource{Identity: store, Observer: observer, Runtime: &console.Runtime{Docker: client}, Locks: locks.New()}}
	t.Cleanup(players.Stop)
	return players, views[0].ID
}

// A Palworld console password of a few digits occurs inside an online
// player's Steam ID. The list still reads, a name holding the password shows
// it redacted, and a name cut to the length limit partway into the password
// shows none of it.
func TestSecretInsidePlayerID(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	port, err := fakedocker.ServeRCON("4242", func(string) string {
		return "name,playeruid,steamid\nAda,1234567890,76561198042420001\nx4242y,1234567891,76561198000000002\n" +
			strings.Repeat("a", 63) + "4242,1234567892,76561198000000003\n"
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "palworld", Image: "thijsvanloef/palworld-server-docker", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.query": "disabled"},
		Env:    []string{"RCON_ENABLED=true", "RCON_PORT=" + fmt.Sprint(port), "ADMIN_PASSWORD=4242"}})
	players, server := gameService(t, engine)
	identified, err := players.Identity.Get(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	reading, err := players.Source.Read(context.Background(), *identified)
	if err != nil || reading == nil || reading.Failed {
		t.Fatal(reading, err)
	}
	if reading.Online != 3 || strings.Join(reading.Names, ",") != "Ada,x[redacted]y,"+strings.Repeat("a", 63) {
		t.Fatal(reading.Players)
	}
}

// Other packages ask how many players a server has. Only a recent, readable
// reading of the server's current container run counts as known.
func TestOnlineNeedsFreshReading(t *testing.T) {
	run := servers.Container{ID: "c1", StateSince: 100}
	now := time.Now().UnixMilli()
	for name, c := range map[string]struct {
		reading *Reading
		online  int
		known   bool
	}{
		"none":      {nil, 0, false},
		"current":   {&Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: now, Players: games.Players{Online: 2}}, 2, true},
		"empty":     {&Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: now}, 0, true},
		"failed":    {&Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: now, Failed: true}, 0, false},
		"stale":     {&Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: now - Fresh.Milliseconds() - 1}, 0, false},
		"restarted": {&Reading{ContainerID: "c1", StartedAt: 50, CheckedAt: now, Players: games.Players{Online: 2}}, 0, false},
	} {
		service := &Service{readings: map[string]*Reading{}}
		if c.reading != nil {
			service.readings["s1"] = c.reading
		}
		if online, known := service.Online("s1", run); online != c.online || known != c.known {
			t.Errorf("%s: %d %v", name, online, known)
		}
	}
}

// Silence counts only after the game answered in the same container run; a
// reading whose query never ran keeps the last outcome, which goes unknown
// once it is old.
func TestGameAnswers(t *testing.T) {
	query := &games.StatusQuery{Protocol: "a2s", Port: 2457}
	run := servers.Container{ID: "c1", StateSince: 100, State: "running", Query: query}
	now := time.Now().UnixMilli()
	service := &Service{readings: map[string]*Reading{}}
	reading := func(answer Answer, at int64) *Reading {
		r := &Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: at, Answer: answer}
		if answer != NotAsked {
			r.AnswerAt = at
		}
		if answer == Answered {
			r.AnsweredAt = at
		}
		return r
	}
	check := func(step string, want Game) {
		t.Helper()
		if got, _ := service.Game("s1", run); got != want {
			t.Errorf("%s: got %d, want %d", step, got, want)
		}
	}
	check("no reading", Waiting)
	service.store("s1", reading(Silent, now))
	check("silent before answering", Waiting)
	service.store("s1", reading(Answered, now))
	check("answered", Answering)
	service.store("s1", reading(NotAsked, now))
	check("query skipped", Answering)
	service.store("s1", reading(Silent, now))
	check("silent after answering", NotAnswering)
	service.readings["s1"].AnswerAt = now - Fresh.Milliseconds() - 1
	check("stale", Unknown)
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 200, State: "running", Query: query}); got != Waiting {
		t.Errorf("restarted: %d", got)
	}
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 100, State: "running"}); got != NotChecked {
		t.Errorf("no query: %d", got)
	}
}

type countingSource struct {
	Source
	reads int
	run   servers.Container
}

func (c *countingSource) Read(context.Context, identity.Server) (*Reading, error) {
	c.reads++
	return &Reading{ContainerID: c.run.ID, StartedAt: c.run.StateSince, CheckedAt: time.Now().UnixMilli()}, nil
}

// A monitored game's status is read every Interval while someone watches
// and every IdleInterval otherwise, since each reading starts a helper.
func TestMonitoredReadsSlowWhenUnwatched(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	players, server := gameService(t, engine)
	current, _ := players.Servers.Refresh(ctx)
	source := &countingSource{Source: players.Source}
	for _, container := range current {
		source.run = container
	}
	if source.run.Query == nil {
		t.Fatal("no status query")
	}
	players.Source = source
	players.Monitored = func(context.Context) map[string]bool { return map[string]bool{server: true} }
	age := func(by time.Duration) {
		players.mu.Lock()
		players.readings[server].CheckedAt = time.Now().Add(-by).UnixMilli()
		players.mu.Unlock()
	}
	players.Poll(ctx, false)
	age(Interval)
	players.Poll(ctx, false)
	if source.reads != 1 {
		t.Fatal("unwatched reads:", source.reads)
	}
	players.Poll(ctx, true)
	age(IdleInterval)
	players.Poll(ctx, false)
	if source.reads != 3 {
		t.Fatal("reads:", source.reads)
	}
}
