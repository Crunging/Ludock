package app

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/servers"
)

// statusSource answers each reading's status query while answering says so.
type statusSource struct{ answering bool }

func (s *statusSource) Lists(servers.Container) bool { return true }

func (s *statusSource) Read(_ context.Context, server identity.Server, _ players.Ask) (*players.Reading, error) {
	now := time.Now().UnixMilli()
	reading := &players.Reading{ContainerID: server.ContainerID, StartedAt: gameRun.StateSince, CheckedAt: now, Via: players.ViaQuery,
		Answer: players.Silent, AnswerAt: now}
	if s.answering {
		reading.Answer, reading.AnsweredAt = players.Answered, now
	}
	return reading, nil
}

// gameRun is the container run statusSource reads; tests set it.
var gameRun servers.Container

// lifetime is one Ludock process's monitoring and player readings over a
// database that outlives it.
type lifetime struct {
	monitor *monitor.Service
	players *players.Service
	events  *[]string
	server  identity.Server
}

// restartable watches one game, with a 60-second wait before alerting and
// recovery on as recover says, and returns a way to start a Ludock process
// over the same database.
func restartable(t *testing.T, source *statusSource, recover bool) func() *lifetime {
	t.Helper()
	ctx := context.Background()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Game", GameType: "valheim"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	session := dbtest.Session(t, d, "admin")
	events := &[]string{}
	start := func() *lifetime {
		p := &players.Service{Identity: store, Source: source}
		t.Cleanup(p.Stop)
		m := &monitor.Service{DB: d, Locks: locks.New(), Runner: &ops.Runner{DB: d}, Store: store,
			Queue: func(_ context.Context, _ *sql.Tx, _ notify.Alert, _, message string) (bool, error) {
				*events = append(*events, message)
				return true, nil
			}}
		watchGames(m, p)
		return &lifetime{monitor: m, players: p, events: events, server: list[0]}
	}
	first := start()
	policy := monitor.DefaultPolicy()
	policy.GraceSeconds, policy.Recover = 60, recover
	if _, err := first.monitor.Configure(ctx, "admin", session, list[0].ID, policy); err != nil {
		t.Fatal(err)
	}
	once := true
	return func() *lifetime {
		if once {
			once = false
			return first
		}
		return start()
	}
}

// check reads the game's status when it has one, then runs a monitor check
// at base+offset.
func (l *lifetime) check(t *testing.T, base, offset int64) {
	t.Helper()
	if gameRun.Query != nil {
		l.players.ReadHeld(context.Background(), l.server)
	}
	l.checkUnread(t, base, offset)
}

// checkUnread runs a monitor check before any new reading, as Ludock's first
// tick does.
func (l *lifetime) checkUnread(t *testing.T, base, offset int64) {
	t.Helper()
	if err := l.monitor.Check(context.Background(), map[string]servers.Container{"mc": gameRun}, base+offset); err != nil {
		t.Fatal(err)
	}
}

func (l *lifetime) restarts(t *testing.T) int {
	t.Helper()
	var count int
	if err := l.monitor.DB.QueryRow("SELECT COUNT(*) FROM operations WHERE actor_id=?", ops.RecoveryActor).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

const (
	silentAlert    = "Game: running, but the game stopped answering."
	restartMessage = "Game: the game stopped answering, so Ludock is restarting it."
)

// A game hangs and Ludock restarts before the alert's wait ends, through
// either signal: its status query, or the image's health check, whose pass
// Docker no longer holds by then. The new process knows the game answered in
// this run, so the hang is not taken for a game still starting: it alerts,
// or with recovery on restarts the game, a full wait after the new process's
// first check, never counting Ludock's downtime as silence. That first check
// may come before any reading; the hang is still announced once.
func TestHungGameIsCaughtAfterLudockRestarts(t *testing.T) {
	for _, signal := range []string{"status query", "health check"} {
		for _, recover := range []bool{false, true} {
			base := time.Now().UnixMilli()
			source := &statusSource{answering: true}
			gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000}
			if signal == "status query" {
				gameRun.Query = &games.StatusQuery{Protocol: "a2s", Port: 2457}
			} else {
				gameRun.HealthAnswers, gameRun.Health, gameRun.HealthyAt = true, "healthy", base
			}
			ludock := restartable(t, source, recover)
			before := ludock()
			before.check(t, base, 1000)
			source.answering = false
			if gameRun.HealthAnswers {
				gameRun.Health = "unhealthy"
			}
			before.check(t, base, 2000)

			// Ludock is down for 50 seconds; the game stays hung, and Docker's
			// health log no longer holds the pass.
			gameRun.HealthyAt = 0
			after := ludock()
			after.checkUnread(t, base, 52_000)
			after.check(t, base, 100_000)
			// Pages don't show the outage before its new wait ends either.
			problems, _ := after.monitor.Problems(context.Background(), base+100_000)
			if len(*after.events) != 0 || after.restarts(t) != 0 || problems[after.server.ID] != nil {
				t.Fatalf("%s, recover %v: alerted for silence Ludock didn't watch: %v %v", signal, recover, *after.events, problems)
			}
			after.check(t, base, 52_000+61_000)
			want, restarts := []string{silentAlert}, 0
			if recover {
				want, restarts = []string{restartMessage}, 1
			}
			if !slices.Equal(*after.events, want) || after.restarts(t) != restarts {
				t.Fatalf("%s, recover %v: %v, %d restarts", signal, recover, *after.events, after.restarts(t))
			}
		}
	}
	gameRun = servers.Container{}
}

// An outage already alerted before Ludock restarted goes on under the same
// alert: the first check after the restart sends nothing new.
func TestAlertedHangOutlastsALudockRestart(t *testing.T) {
	base := time.Now().UnixMilli()
	source := &statusSource{answering: true}
	gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000, Query: &games.StatusQuery{Protocol: "a2s", Port: 2457}}
	t.Cleanup(func() { gameRun = servers.Container{} })
	ludock := restartable(t, source, false)
	before := ludock()
	before.check(t, base, 1000)
	source.answering = false
	before.check(t, base, 2000)
	before.check(t, base, 63_000)
	after := ludock()
	after.check(t, base, 100_000)
	after.check(t, base, 200_000)
	availability, _ := after.monitor.Get(context.Background(), after.server.ID)
	if !slices.Equal(*after.events, []string{silentAlert}) || availability.State.OutageStartedAt != base+2000 {
		t.Fatalf("%v %+v", *after.events, availability.State)
	}
}

// silent reports whether any event says the game stopped answering.
func silent(events []string) bool {
	return slices.ContainsFunc(events, func(event string) bool { return strings.Contains(event, "stopped answering") })
}

// A game that never answered in its run before Ludock restarted, or whose
// container started a new run since, is still waiting after the restart: it
// isn't reported as not answering or restarted, however long it takes, until
// it answers once. (A health check still starting past the wait alerts as
// down, as it always has.)
func TestStartingGameStillWaitsAfterLudockRestarts(t *testing.T) {
	for _, signal := range []string{"status query", "health check"} {
		for _, newRun := range []bool{false, true} {
			base := time.Now().UnixMilli()
			source := &statusSource{answering: newRun}
			gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000}
			if signal == "status query" {
				gameRun.Query = &games.StatusQuery{Protocol: "a2s", Port: 2457}
			} else {
				gameRun.HealthAnswers, gameRun.Health = true, "starting"
				if newRun {
					gameRun.Health, gameRun.HealthyAt = "healthy", base
				}
			}
			ludock := restartable(t, source, true)
			ludock().check(t, base, 1000)

			// While Ludock is down the container restarts, or the game is
			// still starting; it doesn't answer yet.
			if newRun {
				gameRun.StateSince = base + 30_000
			}
			source.answering, gameRun.HealthyAt = false, 0
			if gameRun.HealthAnswers {
				gameRun.Health = "starting"
			}
			after := ludock()
			for offset := int64(52_000); offset < 400_000; offset += 30_000 {
				after.check(t, base, offset)
			}
			if game := after.monitor.Game(after.server.ID, gameRun); game != monitor.GameWaiting || silent(*after.events) || after.restarts(t) != 0 {
				t.Fatalf("%s, new run %v: %d %v", signal, newRun, game, *after.events)
			}
			// Its first answer in this run is what later silence counts from.
			source.answering = true
			if gameRun.HealthAnswers {
				gameRun.Health, gameRun.HealthyAt = "healthy", base+400_000
			}
			after.check(t, base, 400_000)
			source.answering = false
			if gameRun.HealthAnswers {
				gameRun.Health = "unhealthy"
			}
			after.check(t, base, 401_000)
			after.check(t, base, 462_000)
			if events := *after.events; events[len(events)-1] != restartMessage || after.restarts(t) != 1 {
				t.Fatalf("%s, new run %v: %v", signal, newRun, events)
			}
		}
	}
	gameRun = servers.Container{}
}

// A game whose status check is off for its setup, such as a Valheim server
// that isn't public, stays not checked whatever an earlier run saved.
func TestCheckOffStaysNotCheckedAfterLudockRestarts(t *testing.T) {
	base := time.Now().UnixMilli()
	source := &statusSource{answering: true}
	gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000, Query: &games.StatusQuery{Protocol: "a2s", Port: 2457}}
	t.Cleanup(func() { gameRun = servers.Container{} })
	ludock := restartable(t, source, true)
	ludock().check(t, base, 1000)
	gameRun.Query, gameRun.QueryOff = nil, "SERVER_PUBLIC=false"
	after := ludock()
	for _, offset := range []int64{52_000, 120_000, 200_000} {
		after.check(t, base, offset)
	}
	if game := after.monitor.Game(after.server.ID, gameRun); game != monitor.GameNotChecked || len(*after.events) != 0 {
		t.Fatal(game, *after.events)
	}
}

// What an earlier Ludock saved is written once when a run first answers,
// then at most every ten minutes while it keeps answering.
func TestAnswerSavedSparingly(t *testing.T) {
	base := time.Now().UnixMilli()
	source := &statusSource{answering: true}
	gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000, HealthAnswers: true, Health: "healthy", HealthyAt: base}
	t.Cleanup(func() { gameRun = servers.Container{} })
	l := restartable(t, source, false)()
	saved := func() string {
		var value string
		l.monitor.DB.QueryRow("SELECT COALESCE(answered_json,'') FROM availability").Scan(&value)
		return value
	}
	l.check(t, base, 1000)
	first := saved()
	gameRun.HealthyAt = base + 5*60_000
	l.check(t, base, 5*60_000)
	if saved() != first || !strings.Contains(first, `"at":`) {
		t.Fatal(first, saved())
	}
	gameRun.HealthyAt = base + 11*60_000
	l.check(t, base, 11*60_000)
	if saved() == first {
		t.Fatal("never saved a later answer")
	}
}

// Docker is unreachable at Ludock's first check after it restarts around a
// hung game: both waits still start at that first check, so the hang is
// announced once, by the restart.
func TestHangAcrossARestartWithDockerAway(t *testing.T) {
	base := time.Now().UnixMilli()
	source := &statusSource{answering: true}
	gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000, Query: &games.StatusQuery{Protocol: "a2s", Port: 2457}}
	t.Cleanup(func() { gameRun = servers.Container{} })
	ludock := restartable(t, source, true)
	before := ludock()
	before.check(t, base, 1000)
	source.answering = false
	before.check(t, base, 2000)
	after := ludock()
	if err := after.monitor.Check(context.Background(), nil, base+52_000); err != nil {
		t.Fatal(err)
	}
	after.check(t, base, 67_000)
	after.check(t, base, 113_000)
	if !slices.Equal(*after.events, []string{restartMessage}) || after.restarts(t) != 1 {
		t.Fatal(*after.events, after.restarts(t))
	}
}

// A hang already announced before Ludock restarted keeps its recovery
// timing, so a pause between restarts isn't lengthened by Ludock's own.
func TestAnnouncedHangKeepsItsRecoveryTiming(t *testing.T) {
	ctx := context.Background()
	base := time.Now().UnixMilli()
	source := &statusSource{answering: true}
	gameRun = servers.Container{ID: "mc", State: "running", StateSince: base - 60_000, Query: &games.StatusQuery{Protocol: "a2s", Port: 2457}}
	t.Cleanup(func() { gameRun = servers.Container{} })
	ludock := restartable(t, source, true)
	before := ludock()
	before.check(t, base, 1000)
	source.answering = false
	before.check(t, base, 2000)
	before.check(t, base, 63_000)
	if before.restarts(t) != 1 {
		t.Fatal(*before.events)
	}
	// The restart never acted, as when the game answered first.
	if _, err := before.monitor.DB.Exec("UPDATE operations SET status=?,updated_at=?", ops.Skipped, base+64_000); err != nil {
		t.Fatal(err)
	}
	after := ludock()
	after.check(t, base, 100_000)
	if state, _ := after.monitor.Recovery(ctx, after.server.ID); state.SilentSince != base+2000 || state.Pending {
		t.Fatalf("%+v", state)
	}
}

// Ludock ends just as it queues the alert that a server is down, and the
// server is still down when it starts again: the outage is not saved as
// alerted without its alert, so the next process queues it, before the one
// saying the server is running again.
func TestOutageAlertSurvivesALudockRestart(t *testing.T) {
	ctx := context.Background()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{{ContainerID: "mc", Name: "mc", DisplayName: "Game", GameType: "valheim"}}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	session := dbtest.Session(t, d, "admin")
	notifications := &notify.Service{DB: d}
	if err := notifications.Configure(ctx, "admin", session, notify.Discord,
		notify.Config{Enabled: true, URL: "https://discord.com/api/webhooks/1/token"}, false); err != nil {
		t.Fatal(err)
	}
	// ends is a process that stops as its alert is written, before anything
	// it wrote commits.
	start := func(ends bool) *monitor.Service {
		m := &monitor.Service{DB: d, Locks: locks.New(), Store: store, Queue: notifications.QueueIn, Alerted: notifications.Poke}
		if ends {
			m.Queue = func(ctx context.Context, tx *sql.Tx, alert notify.Alert, key, message string) (bool, error) {
				notifications.QueueIn(ctx, tx, alert, key, message)
				return false, errors.New("ended")
			}
		}
		return m
	}
	policy := monitor.DefaultPolicy()
	policy.GraceSeconds = 60
	if _, err := start(false).Configure(ctx, "admin", session, list[0].ID, policy); err != nil {
		t.Fatal(err)
	}
	check := func(m *monitor.Service, state string, at int64) {
		t.Helper()
		m.Check(ctx, map[string]servers.Container{"mc": {ID: "mc", State: state}}, at)
	}
	before := start(true)
	check(before, "running", 1000)
	check(before, "exited", 2000)
	check(before, "exited", 63_000)
	after := start(false)
	check(after, "exited", 100_000)
	check(after, "running", 200_000)
	rows, err := d.Query("SELECT message FROM notification_deliveries ORDER BY created_at, rowid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sent []string
	for rows.Next() {
		var message string
		rows.Scan(&message)
		sent = append(sent, message)
	}
	if !slices.Equal(sent, []string{"Game: the server is down.", "Game: the server is running again."}) {
		t.Fatal(sent)
	}
}
