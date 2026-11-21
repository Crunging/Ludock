package players

import (
	"bufio"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
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
		Source: &GameSource{Servers: service, Runtime: &console.Runtime{Docker: client}, Locks: locks.New()}}
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
	reading, err := players.Source.Read(context.Background(), *identified, Ask{Players: true})
	if err != nil || reading == nil || reading.Failed {
		t.Fatal(reading, err)
	}
	if reading.Online != 3 || strings.Join(reading.Names, ",") != "Ada,x[redacted]y,"+strings.Repeat("a", 63) {
		t.Fatal(reading.Players)
	}
	// Kick and ban still reach the player by their ID.
	if id, ok := reading.ID("x[redacted]y"); id != "76561198000000002" || !ok {
		t.Fatal(id, ok)
	}
}

// A Minecraft name holding the console password is shown redacted and is no
// longer an account name that kick or ban can take.
func TestSecretInsideAccountName(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	port, err := fakedocker.ServeRCON("4242", func(string) string {
		return "There are 2 of a max of 10 players online: Ada, x4242y"
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port), "ludock.query": "disabled"},
		Env:    []string{"RCON_PASSWORD=4242"}})
	players, server := gameService(t, engine)
	identified, err := players.Identity.Get(context.Background(), server)
	if err != nil {
		t.Fatal(err)
	}
	reading, err := players.Source.Read(context.Background(), *identified, Ask{Players: true})
	if err != nil || reading == nil || reading.Failed || strings.Join(reading.Names, ",") != "Ada,x[redacted]y" {
		t.Fatal(reading, err)
	}
	if id, ok := reading.ID("Ada"); id != "Ada" || !ok {
		t.Fatal(id, ok)
	}
	if id, ok := reading.ID("x[redacted]y"); ok {
		t.Fatal(id)
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
	// A query's count shows, but a schedule waiting for an empty server
	// doesn't take it over the console's list.
	service := &Service{readings: map[string]*Reading{"s1": {ContainerID: "c1", StartedAt: 100, CheckedAt: now, Via: ViaQuery, partial: true,
		Players: games.Players{Online: 1}}}}
	if online, known := service.Online("s1", run); online != 1 || !known {
		t.Error("query count:", online, known)
	}
	if _, _, known := service.ListedAt("s1", run); known {
		t.Error("a schedule took a query-only count")
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
	// An answer outlasts an idle poll's gap, then goes stale.
	service.readings["s1"].AnswerAt = now - IdleInterval.Milliseconds()
	check("between idle polls", NotAnswering)
	service.readings["s1"].AnswerAt = now - answerFresh.Milliseconds() - 1
	check("stale", Unknown)
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 200, State: "running", Query: query}); got != Waiting {
		t.Errorf("restarted: %d", got)
	}
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 100, State: "running"}); got != NotChecked {
		t.Errorf("no query: %d", got)
	}
}

// A game without a status query answers through its image's health check.
// Docker keeps only its latest five results, so a check failing for longer
// than that still counts as not answering after it once passed in this run;
// one that never passed is still waiting, and a new run starts over.
func TestGameAnswersHealthCheck(t *testing.T) {
	service := &Service{}
	run := servers.Container{ID: "c1", StateSince: 100, State: "running", HealthAnswers: true, Health: "starting"}
	check := func(step string, want Game, wantAt int64) {
		t.Helper()
		if got, at := service.Game("s1", run); got != want || (wantAt != 0 && at != wantAt) {
			t.Errorf("%s: got %d at %d, want %d at %d", step, got, at, want, wantAt)
		}
	}
	check("starting", Waiting, 0)
	run.Health = "unhealthy"
	check("unhealthy before passing", Waiting, 0)
	run.Health, run.HealthyAt = "healthy", 500
	check("healthy", Answering, 500)
	run.Health, run.HealthyAt = "unhealthy", 0
	check("past Docker's results", NotAnswering, 500)
	// The last pass of an earlier run doesn't count in this one.
	run.StateSince, run.Health, run.HealthyAt = 1000, "unhealthy", 500
	check("restarted", Waiting, 0)
	// Healthy with no pass left in Docker's results still counts as one.
	run.StateSince, run.Health, run.HealthyAt = 2000, "healthy", 0
	check("healthy without a pass", Answering, 0)
	run.Health = "unhealthy"
	check("unhealthy after that", NotAnswering, 0)
	run.HealthAnswers = false
	check("no health check", NotChecked, 0)
}

// An answer an earlier Ludock saw in this run counts as one: until a query
// says, the game is unknown, and silent after it, not answering; it counts
// for neither another run nor a game without a status check.
func TestRecalledAnswerCounts(t *testing.T) {
	query := &games.StatusQuery{Protocol: "a2s", Port: 2457}
	run := servers.Container{ID: "c1", StateSince: 100, State: "running", Query: query}
	service := &Service{readings: map[string]*Reading{}}
	service.Recall("s1", run, 50)
	if got, at := service.Game("s1", run); got != Unknown || at != 50 {
		t.Errorf("before a query: %d at %d", got, at)
	}
	now := time.Now().UnixMilli()
	service.store("s1", &Reading{ContainerID: "c1", StartedAt: 100, CheckedAt: now, Answer: Silent, AnswerAt: now})
	if got, at := service.Game("s1", run); got != NotAnswering || at != 50 {
		t.Errorf("silent: %d at %d", got, at)
	}
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 200, State: "running", Query: query}); got != Waiting {
		t.Errorf("new run: %d", got)
	}
	if got, _ := service.Game("s1", servers.Container{ID: "c1", StateSince: 100, State: "running"}); got != NotChecked {
		t.Errorf("no check: %d", got)
	}
	health := servers.Container{ID: "c2", StateSince: 100, State: "running", HealthAnswers: true, Health: "unhealthy"}
	service.Recall("s2", health, 60)
	if got, at := service.Game("s2", health); got != NotAnswering || at != 60 {
		t.Errorf("health check: %d at %d", got, at)
	}
}

type countingSource struct {
	Source
	reads, lists int
	run          servers.Container
}

func (c *countingSource) Read(_ context.Context, _ identity.Server, ask Ask) (*Reading, error) {
	c.reads++
	if ask.Players {
		c.lists++
	}
	return &Reading{ContainerID: c.run.ID, StartedAt: c.run.StateSince, CheckedAt: time.Now().UnixMilli(), partial: !ask.Players}, nil
}

// A monitored game's status is read every Interval while a page showing it
// is open and every IdleInterval otherwise, such as while only another
// server's page is, since each reading starts a helper.
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
	all := func(string) bool { return true }
	// Another server's page is open: this one is read as if nobody watched.
	other := func(id string) bool { return id != server }
	players.Poll(ctx, nil)
	age(Interval)
	players.Poll(ctx, nil)
	players.Poll(ctx, other)
	if source.reads != 1 {
		t.Fatal("unwatched reads:", source.reads)
	}
	players.Poll(ctx, all)
	age(IdleInterval)
	players.Poll(ctx, nil)
	if source.reads != 3 || source.lists != 1 {
		t.Fatal("reads:", source.reads, "lists:", source.lists)
	}
	// A page opening after a query-only reading gets the list at once, and
	// the list then counts as fresh.
	players.Poll(ctx, all)
	players.Poll(ctx, all)
	if source.reads != 4 || source.lists != 2 {
		t.Fatal("reads:", source.reads, "lists:", source.lists)
	}
}

// A Minecraft server's console writes two lines to the game's log for every
// connection. Once the console has listed the players, a status query that
// says nobody is online, or whose player sample names the same ones, stands
// in for the command; a change, a sample that doesn't name everyone, a
// check after a command, or a reading taken while nobody watches never uses
// the console's list in place of the current one.
func TestStatusQueryStandsInForTheList(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	image := "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("ab", 32)
	engine.AddImage(image)
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, stdout func([]byte)) {
		helper.RunNetworkConsole(stdin, writerFunc(stdout))
	}
	var mu sync.Mutex
	online, sampled, commands := []string{}, true, 0
	rcon, _ := fakedocker.ServeRCON("secret", func(string) string {
		mu.Lock()
		defer mu.Unlock()
		commands++
		return fmt.Sprintf("There are %d of a max of 10 players online: %s", len(online), strings.Join(online, ", "))
	})
	query, _ := fakedocker.ServeMinecraftPlayers(func() ([]string, int, int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !sampled {
			return nil, len(online), 10, true
		}
		return slices.Clone(online), len(online), 10, true
	})
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.query.port": fmt.Sprint(query)},
		Env:    []string{"RCON_PASSWORD=secret", fmt.Sprintf("RCON_PORT=%d", rcon)}})
	players, server := gameService(t, engine)
	players.Source.(*GameSource).Runtime.Images = &launch.Images{Docker: players.Source.(*GameSource).Runtime.Docker, Configured: image}
	identified, err := players.Identity.Get(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	var previous *Reading
	read := func(ask Ask) *Reading {
		ask.Previous = previous
		reading, err := players.Source.Read(ctx, *identified, ask)
		if err != nil || reading == nil || reading.Failed {
			t.Fatal(reading, err)
		}
		previous = reading
		return reading
	}
	expect := func(step string, reading *Reading, wantCommands int, via Via, names ...string) {
		t.Helper()
		mu.Lock()
		got := commands
		mu.Unlock()
		if got != wantCommands || reading.Via != via || strings.Join(reading.Names, ",") != strings.Join(names, ",") {
			t.Fatalf("%s: %d commands, %+v", step, got, reading)
		}
	}
	expect("first list", read(Ask{Players: true}), 1, ViaConsole)
	expect("still nobody", read(Ask{Players: true}), 1, ViaConsole)
	mu.Lock()
	online = []string{"Mara"}
	mu.Unlock()
	expect("someone joined", read(Ask{Players: true}), 2, ViaConsole, "Mara")
	expect("same players", read(Ask{Players: true}), 2, ViaConsole, "Mara")
	expect("after a command", read(Ask{Players: true, Fresh: true}), 3, ViaConsole, "Mara")
	mu.Lock()
	online = []string{"Mara", "Theo"}
	mu.Unlock()
	expect("another joined", read(Ask{Players: true}), 4, ViaConsole, "Mara", "Theo")
	expect("unwatched, same players", read(Ask{}), 4, ViaConsole, "Mara", "Theo")
	mu.Lock()
	sampled = false
	mu.Unlock()
	expect("sample hidden", read(Ask{Players: true}), 5, ViaConsole, "Mara", "Theo")
	mu.Lock()
	online, sampled = []string{"Mara"}, true
	mu.Unlock()
	if reading := read(Ask{}); !reading.partial || reading.Online != 1 {
		t.Fatalf("unwatched, someone left: %+v", reading)
	}
	expect("unwatched, someone left", previous, 5, ViaQuery)
	expect("watched after a count", read(Ask{Players: true}), 6, ViaConsole, "Mara")
}

type writerFunc func([]byte)

func (w writerFunc) Write(data []byte) (int, error) {
	w(data)
	return len(data), nil
}

type blockingSource struct {
	Source
	mu      sync.Mutex
	reads   int
	release chan struct{}
}

func (b *blockingSource) Read(ctx context.Context, _ identity.Server, _ Ask) (*Reading, error) {
	b.mu.Lock()
	b.reads++
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, errSkipped
}

// A refused kick waits for a reading that starts after it asks, not one
// already running on the list it compared, and never longer than its limit.
func TestCheckAndWaitWaitsForANewReading(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	players, server := gameService(t, engine)
	source := &blockingSource{Source: players.Source, release: make(chan struct{})}
	players.Source = source
	reads := func() int {
		source.mu.Lock()
		defer source.mu.Unlock()
		return source.reads
	}
	players.Check(ctx, server)
	for deadline := time.Now().Add(5 * time.Second); reads() != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no reading started")
		}
	}
	returned := make(chan int, 1)
	go func() {
		players.CheckAndWait(ctx, server, time.Minute)
		returned <- reads()
	}()
	release := func() {
		t.Helper()
		select {
		case source.release <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatal("no reading to release")
		}
	}
	// The running reading ends; the one asked for starts and holds the
	// caller until it ends too.
	time.Sleep(50 * time.Millisecond)
	release()
	for deadline := time.Now().Add(5 * time.Second); reads() != 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the reading asked for never started")
		}
	}
	select {
	case got := <-returned:
		t.Fatal("returned while the reading asked for ran, after", got)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the reading ended")
	}
	start := time.Now()
	players.CheckAndWait(ctx, server, 100*time.Millisecond)
	if time.Since(start) > 5*time.Second {
		t.Fatal("waited past the limit")
	}
	close(source.release)
}

// One game's status query hangs. Each pass returns without it, the other
// server is read again on its own interval, and the hung one gets no second
// reading while its first runs; a page opened meanwhile gets its list once
// the query ends.
func TestHungReadingHoldsUpNoOther(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.AddContainer(fakedocker.GameOptions{Name: "hung", Image: "itzg/minecraft-server", Running: true})
	players, hung := gameService(t, engine)
	engine.AddContainer(fakedocker.GameOptions{Name: "other", Image: "itzg/minecraft-server", Running: true})
	current, _ := players.Servers.RefreshNow(ctx)
	views, err := players.Servers.List(ctx, &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin})
	if err != nil || len(views) != 2 {
		t.Fatal(views, err)
	}
	other := views[0].ID
	if other == hung {
		other = views[1].ID
	}
	source := &stuckSource{Source: players.Source, stuck: hung, runs: current, started: make(chan struct{}),
		release: make(chan struct{}), reads: map[string]int{}}
	players.Source = source
	released := false
	t.Cleanup(func() {
		if !released {
			close(source.release)
		}
	})
	defer func(wait time.Duration) { passWait = wait }(passWait)
	passWait = 10 * time.Millisecond
	players.Monitored = func(context.Context) map[string]bool { return map[string]bool{hung: true} }
	read := func(want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			players.mu.Lock()
			_, busy := players.reading[other]
			stored := players.readings[other] != nil
			players.mu.Unlock()
			if source.count(other) == want && stored && !busy {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("the other server waited on the hung one:", source.count(other))
			}
		}
	}
	all := func(string) bool { return true }
	players.Poll(ctx, nil)
	<-source.started
	players.Poll(ctx, all)
	read(1)
	players.mu.Lock()
	players.readings[other].CheckedAt = time.Now().Add(-Interval).UnixMilli()
	players.mu.Unlock()
	players.Poll(ctx, all)
	read(2)
	if got := source.count(hung); got != 1 {
		t.Fatal("hung server readings:", got)
	}
	released = true
	close(source.release)
	for deadline := time.Now().Add(5 * time.Second); source.count(hung) != 2 || source.listed() != 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page's list was dropped:", source.count(hung), source.listed())
		}
	}
}

// stuckSource holds the stuck server's first reading until release and
// reads the rest at once.
type stuckSource struct {
	Source
	stuck            string
	runs             map[string]servers.Container
	started, release chan struct{}
	mu               sync.Mutex
	reads            map[string]int
	lists            int
}

func (s *stuckSource) count(serverID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[serverID]
}

func (s *stuckSource) listed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

func (s *stuckSource) Read(ctx context.Context, server identity.Server, ask Ask) (*Reading, error) {
	s.mu.Lock()
	s.reads[server.ID]++
	first := s.reads[server.ID] == 1
	if server.ID == s.stuck && ask.Players {
		s.lists++
	}
	s.mu.Unlock()
	if server.ID == s.stuck && first {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
		return nil, errSkipped
	}
	run := s.runs[server.ContainerID]
	return &Reading{ContainerID: run.ID, StartedAt: run.StateSince, CheckedAt: time.Now().UnixMilli(), partial: !ask.Players}, nil
}
