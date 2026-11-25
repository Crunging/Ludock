package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/selfupdate"
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

// A server page opened while a pass is still reading another server's
// players is read once that pass ends, not at the next tick.
func TestViewerDuringAPassReadsPlayers(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	release := make(chan struct{})
	reads := map[string]chan struct{}{"first": make(chan struct{}, 8), "second": make(chan struct{}, 8)}
	for _, name := range []string{"first", "second"} {
		port, err := fakedocker.ServeRCON("secret", func(string) string {
			reads[name] <- struct{}{}
			if name == "first" {
				<-release
			}
			return "There are 0 of a max of 10 players online:"
		})
		if err != nil {
			t.Fatal(err)
		}
		engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "itzg/minecraft-server", Running: true,
			Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.query": "disabled"},
			Env:    []string{"RCON_PORT=" + strconv.Itoa(port), "RCON_PASSWORD=secret"}})
	}
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	defer a.Stop(time.Now().Add(time.Minute))
	var released sync.Once
	defer released.Do(func() { close(release) })
	if _, err := a.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	logical, _ := a.Identity.List(ctx, "")
	ids := map[string]string{}
	for _, server := range logical {
		ids[server.DisplayName] = server.ID
	}
	follow := func(name string) func() {
		id := ids[name]
		return a.Events.Subscribe(&events.Subscriber{Validate: func(context.Context) *db.SessionUser { return nil },
			Send: func(events.Event) {}, Close: func(string) {}, Follows: func(server string) bool { return server == id }})
	}
	defer follow("first")()
	select {
	case <-reads["first"]:
	case <-time.After(5 * time.Second):
		t.Fatal("the first page wasn't read")
	}
	defer follow("second")()
	released.Do(func() { close(release) })
	select {
	case <-reads["second"]:
	case <-time.After(5 * time.Second):
		t.Fatal("the second page waited for the pass reading the first")
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
	engine.AddFault(`^DELETE .*/containers/[^/]+$`, 500, false, 2)
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
			t.Fatal("startup recovery never finished")
		}
	}
	if engine.Container(leftover) != nil {
		t.Fatal("left the console helper")
	}
}

// Startup recovery that can't reach Docker retries, and its log says
// Docker is unreachable so the owner knows where to look. Once Docker
// answers, recovery finishes and work can take servers.
func TestStartupRetrySaysDockerIsUnreachable(t *testing.T) {
	var logged lockedBuffer
	logging.SetOutput(&logged, &logged)
	defer logging.SetOutput(os.Stdout, os.Stderr)
	socket := filepath.Join(t.TempDir(), "docker.sock")
	a, err := New(config.Config{DockerSocket: socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.Start(ctx)
	defer a.Stop(time.Now().Add(time.Minute))
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(logged.String(), "Could not recover interrupted operations"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("startup recovery never retried", logged.String())
		}
	}
	if !strings.Contains(logged.String(), `"docker":"unreachable"`) {
		t.Fatal(logged.String())
	}
	engine, err := fakedocker.StartSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if release, err := a.Locks.Acquire([]string{"server:x"}, nil); err == nil {
			release()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("startup recovery never finished")
		}
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
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

// A Ludock that comes up during its own update tells the update's helper it
// opened its data before startup cleanup that writes, which waits for the
// update to end: here an unpack's staging record whose removal is a write.
func TestStartSignalsAnUpdateBeforeCleanupThatWrites(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	operation := "0d3e7b1c-5a2f-4c6e-9b8d-1f2a3b4c5d6e"
	own := engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: "ludock", Running: true})
	engine.AddContainer(fakedocker.GameOptions{Name: selfupdate.HelperName(operation), Image: "ludock", Running: true,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "self-update", "ludock.update.operation": operation}})
	d := dbtest.Open(t)
	for key, value := range map[string]any{
		"ludock.update":    map[string]string{"id": operation, "state": "handed_over", "container": own, "from": "1.0.0", "target": "sha256:" + strings.Repeat("3", 64)},
		"unpack-stage:one": map[string]string{"server": "gone", "root": "/data", "archive": "/data/mods.zip", "id": "one"},
	} {
		if err := d.SetSetting(ctx, nil, key, value); err != nil {
			t.Fatal(err)
		}
	}
	a, err := New(config.Config{DockerSocket: engine.Socket, SelfContainer: own, MountID: own}, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.Start(runCtx)
	defer a.Stop(time.Now().Add(time.Minute))
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		ready := false
		for _, id := range engine.SortedIDs() {
			ready = ready || engine.Container(id).Config.Labels["ludock.internal"] == "self-update-ready"
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Ludock never said it opened its data")
		}
	}
}
