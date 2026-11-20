package app

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/events"
)

// A page read just before the background tick does not stand in for the
// tick's own discovery: monitoring and schedules see Docker as it is now.
func TestTickDiscoversAfterARecentRead(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	listings := func() int {
		n := 0
		for _, request := range engine.Requests {
			if strings.Contains(request, "/containers/json") {
				n++
			}
		}
		return n
	}
	if _, err := a.Servers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := listings()
	a.Tick(context.Background())
	a.background.Wait()
	if listings() == before {
		t.Fatal("the tick reused a page read's discovery")
	}
}

// Player counts are read only while someone has a page open that follows
// server changes: a reading can start a console helper on every tick.
func TestTickReadsPlayersOnlyWhileWatched(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	var mu sync.Mutex
	readings := 0
	port, err := fakedocker.ServeRCON("secret", func(string) string {
		mu.Lock()
		readings++
		mu.Unlock()
		return "There are 0 of a max of 10 players online:"
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		// Without a status query, monitoring reads nothing unwatched.
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.query": "disabled"},
		Env:    []string{"RCON_PORT=" + strconv.Itoa(port), "RCON_PASSWORD=secret"}})
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return readings
	}
	a.Tick(context.Background())
	a.background.Wait()
	if count() != 0 {
		t.Fatal("read players with nobody watching")
	}
	unsubscribe := a.Events.Subscribe(&events.Subscriber{Validate: func(context.Context) *db.SessionUser { return nil },
		Send: func(events.Event) {}, Close: func(string) {}})
	defer unsubscribe()
	a.Tick(context.Background())
	a.background.Wait()
	if count() != 1 {
		t.Fatal("did not read players for a watched page", count())
	}
	a.Events.Stop()
}

// The first page to open reads players right away, not at the next tick.
func TestFirstViewerReadsPlayers(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	read := make(chan struct{}, 8)
	port, err := fakedocker.ServeRCON("secret", func(string) string {
		read <- struct{}{}
		return "There are 0 of a max of 10 players online:"
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		// Without a status query, monitoring reads nothing unwatched.
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.query": "disabled"},
		Env:    []string{"RCON_PORT=" + strconv.Itoa(port), "RCON_PASSWORD=secret"}})
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	defer a.Stop(time.Now().Add(time.Minute))
	// Let the startup tick, with nobody watching, pass.
	time.Sleep(500 * time.Millisecond)
	select {
	case <-read:
		t.Fatal("read players before anyone watched")
	default:
	}
	unsubscribe := a.Events.Subscribe(&events.Subscriber{Validate: func(context.Context) *db.SessionUser { return nil },
		Send: func(events.Event) {}, Close: func(string) {}})
	defer unsubscribe()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("waited for the next tick to read players")
	}
}

// A console helper left by an interrupted run may still be sending a
// command. Docker refuses to remove it at first, and no work takes a server
// until it is gone.
func TestStartRemovesLeftoverConsoleHelpers(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	leftover := engine.AddContainer(fakedocker.GameOptions{Name: "ludock-console-1", Image: "ludock", Running: true,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "console-helper"}})
	engine.FailRemoves = 2
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	defer a.Stop(time.Now().Add(time.Minute))
	if release, err := a.Locks.Acquire([]string{"server:x"}, nil); err == nil {
		release()
		t.Fatal("work took a server before the leftover helper was removed")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		release, err := a.Locks.Acquire([]string{"server:x"}, nil)
		if err == nil {
			release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup recovery never finished", err)
		}
	}
	if engine.Container(leftover) != nil {
		t.Fatal("left the console helper")
	}
}

// Docker kills a container ten seconds after asking it to stop. Work that
// keeps a server, such as a request or command still running, holds a
// stopping Ludock only until its deadline; the work is left for startup
// recovery, as a kill would leave it.
func TestStopIsBounded(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.Start(ctx)
	var release func()
	for deadline := time.Now().Add(10 * time.Second); release == nil; time.Sleep(20 * time.Millisecond) {
		release, _ = a.Locks.Acquire([]string{"server:stuck"}, nil)
		if time.Now().After(deadline) {
			t.Fatal("startup recovery never finished")
		}
	}
	t.Cleanup(release)
	cancel()
	started := time.Now()
	if a.Stop(started.Add(300 * time.Millisecond)) {
		t.Fatal("reported a clean stop while work still held a server")
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatal("stopping waited", waited)
	}
}
