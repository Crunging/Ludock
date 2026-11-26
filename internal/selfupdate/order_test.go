package selfupdate_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"github.com/Crunging/Ludock/internal/selfupdate/fake"
	"github.com/Crunging/Ludock/internal/update"
)

const (
	tag      = "ghcr.io/crunging/ludock:1"
	oldImage = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	newImage = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	socket   = "/var/run/docker.sock"
)

// rig is one Docker host on the fake daemon, with Ludock's container and a
// game, and the trace of the Docker changes asked for in order, by the
// container's role. A Ludock process starts with each Ludock container and
// dies when it stops, as in its container.
type rig struct {
	t       *testing.T
	engine  *fakedocker.Engine
	client  *docker.Client
	dataDir string
	session string
	old     string
	game    string

	mu    sync.Mutex
	trace []string
	// fail fails the first request traced as each line with its status;
	// at runs a function when a line is first traced, before it is served.
	fail      map[string]int
	at        map[string]func()
	processes map[string]*process
	onStart   func(c *fakedocker.Container)
	// kill, set, is the running helper's end, as with its host; killed
	// closes once a case killed it.
	kill   context.CancelFunc
	killed chan struct{}
}

type process struct {
	svc    *selfupdate.Service
	d      *db.DB
	cancel context.CancelFunc
}

func newRig(t *testing.T) *rig {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	g := &rig{t: t, engine: engine, client: docker.New(engine.Socket), dataDir: t.TempDir(), fail: map[string]int{},
		at: map[string]func(){}, processes: map[string]*process{}, killed: make(chan struct{})}
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateUser(context.Background(), db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	g.session = dbtest.Session(t, d, "admin")
	d.SetSetting(context.Background(), nil, "probe", "before")
	d.Close()

	engine.AddImage(tag)
	check := map[string]any{"Test": []any{"CMD", "/usr/local/bin/ludock", "healthcheck"}}
	engine.SetImageConfig(oldImage, map[string]any{"Healthcheck": check})
	g.old = engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: tag, Running: true, Env: []string{"LUDOCK_DATA_DIR=" + g.dataDir},
		Labels: map[string]string{"com.docker.compose.project": "ludock", "com.docker.compose.service": "ludock"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: engine.Socket, Destination: socket}, {Type: "bind", Source: g.dataDir, Destination: g.dataDir, RW: true}}})
	engine.Update(g.old, func(c *fakedocker.Container) {
		c.Image = oldImage
		c.ExtraConfig = map[string]any{"Healthcheck": check}
		c.HostConfig = map[string]any{"NetworkMode": "bridge", "Binds": []string{engine.Socket + ":" + socket + ":ro", g.dataDir + ":" + g.dataDir},
			"PortBindings":  map[string]any{"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000"}}},
			"RestartPolicy": map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0}}
	})
	g.game = engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: true})
	engine.SetImageConfig(newImage, map[string]any{"Healthcheck": check, "Labels": map[string]any{"org.opencontainers.image.version": "1.1.0"}})
	engine.PublishImage(tag, newImage)

	engine.Intercept = g.intercept
	engine.OnStart = func(c *fakedocker.Container) {
		if g.onStart != nil {
			g.onStart(c)
		}
		if c.CreateRequest.Labels["ludock.internal"] == "" && c.Config.Labels["com.docker.compose.service"] == "ludock" {
			if c.Image == newImage && c.State.Health == nil {
				health(g, c, "healthy")
			}
			go g.boot(c.ID, c.Image, c.State.StartedAt)
		}
	}
	engine.OnStop = func(c *fakedocker.Container) {
		g.mu.Lock()
		running := g.processes[c.ID]
		delete(g.processes, c.ID)
		g.mu.Unlock()
		if running != nil {
			running.cancel()
		}
	}
	fake.Run(engine, fake.Options{Poll: 5 * time.Millisecond, HealthWait: 200 * time.Millisecond,
		Started: func(cancel context.CancelFunc) {
			g.mu.Lock()
			g.kill = cancel
			g.mu.Unlock()
		}})
	return g
}

var (
	versioned     = regexp.MustCompile(`^/v[0-9.]+(/.*)$`)
	containerPath = regexp.MustCompile(`^/containers/([^/]+)(/[a-z]+)?$`)
	verbs         = map[string]string{"/start": "start", "/stop": "stop", "/update": "policy", "/rename": "rename", "": "remove"}
)

func (g *rig) intercept(r *http.Request) int {
	path := r.URL.Path
	if match := versioned.FindStringSubmatch(path); match != nil {
		path = match[1]
	}
	line := ""
	match := containerPath.FindStringSubmatch(path)
	switch {
	case path == "/images/create":
		line = "pull"
	case path == "/containers/create":
		line = "create " + g.role(r.URL.Query().Get("name"))
	case match != nil && r.Method != http.MethodGet && verbs[match[2]] != "":
		line = verbs[match[2]] + " " + g.role(match[1])
	}
	if line == "" {
		return 0
	}
	g.mu.Lock()
	g.trace = append(g.trace, line)
	status, at := g.fail[line], g.at[line]
	delete(g.fail, line)
	delete(g.at, line)
	g.mu.Unlock()
	if at != nil {
		at()
	}
	return status
}

// role names a container by what it is to the update.
func (g *rig) role(reference string) string {
	info := g.engine.Container(reference)
	if info == nil {
		for _, id := range g.engine.SortedIDs() {
			if candidate := g.engine.Container(id); candidate != nil && candidate.Name == "/"+reference {
				info = candidate
			}
		}
	}
	name := reference
	if info != nil {
		if info.ID == g.old {
			return "old"
		}
		name = strings.TrimPrefix(info.Name, "/")
	}
	switch {
	case strings.HasPrefix(name, "ludock-self-update-"):
		return "helper"
	case strings.Contains(name, "-ludock-update-") || name == "ludock":
		return "new"
	}
	return name
}

// boot starts the Ludock of container, as cmd/ludock does: it puts a
// put-back's data copy into place, opens its data, and settles an update.
func (g *rig) boot(container, image string, run ...string) *process {
	ctx, cancel := context.WithCancel(context.Background())
	g.t.Cleanup(cancel)
	putBack := selfupdate.BeforeOpen(ctx, g.client, container, g.dataDir, 2*time.Second)
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		cancel()
		return nil
	}
	g.t.Cleanup(func() { d.Close() })
	version := map[string]string{oldImage: "1.0.0", newImage: "1.1.0"}[image]
	p := &process{d: d, cancel: cancel, svc: &selfupdate.Service{DB: d, Docker: g.client, Own: self.New(g.client, container),
		Updates: &update.Service{Docker: g.client}, Locks: locks.New(), DataDir: g.dataDir, Socket: socket, MountID: container,
		Version: version, Poll: 5 * time.Millisecond, PutBack: putBack}}
	g.mu.Lock()
	g.processes[container] = p
	g.mu.Unlock()
	// A run that already ended, before its process was there to die, has none.
	if info := g.engine.Container(container); len(run) > 0 && (info == nil || !info.State.Running || info.State.StartedAt != run[0]) {
		cancel()
		return nil
	}
	p.svc.Recover(ctx, ctx)
	return p
}

// settle waits for the update to end, as the Ludock under its name records
// it, and returns that Ludock and its container.
func (g *rig) settle(name string) (*process, *docker.ContainerInspect) {
	g.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		for _, id := range g.engine.SortedIDs() {
			info := g.engine.Container(id)
			if info == nil || info.Name != "/"+name || !info.State.Running {
				continue
			}
			g.mu.Lock()
			p := g.processes[info.ID]
			g.mu.Unlock()
			if p != nil && !p.svc.Last(context.Background()).InProgress() && !p.svc.Updating() {
				return p, info
			}
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("the update didn't end: %q", g.lines())
		}
	}
}

func (g *rig) lines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.trace)
}

// migrate is the new version migrating the data as it starts, which the old
// one then can't open.
func (g *rig) migrate() {
	conn, err := sql.Open("sqlite", "file:"+filepath.Join(g.dataDir, "ludock.db"))
	if err == nil {
		_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", db.LatestVersion()+1))
		conn.Close()
	}
	if err != nil {
		g.t.Error(err)
	}
}

func isNew(c *fakedocker.Container) bool {
	return c.Image == newImage && c.CreateRequest.Labels["ludock.internal"] == ""
}

func health(g *rig, c *fakedocker.Container, status string) {
	g.engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: status} })
}

var (
	swap    = []string{"pull", "create helper", "start helper", "create new", "policy old", "stop old", "start new"}
	putBack = []string{"stop new", "remove new", "start old", "policy old", "remove helper"}
)

func join(parts ...[]string) []string { return slices.Concat(parts...) }

// Each case updates Ludock from 1.0.0 to the image its tag points to, and
// checks what ran in what order, which Ludock runs afterwards under its name
// and on which data, and what it recorded. The game is never touched.
func TestSelfUpdateOrder(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		arrange func(g *rig)
		before  func(g *rig, p *process)
		// after, when set, is what the owner does once the helper stopped,
		// with the name the Ludock that settles it runs under.
		after   func(g *rig) string
		refused string
		trace   []string
		image   string
		state   string
		reason  string
		// probe is the data the Ludock that settles it runs on.
		probe string
		data  string
		copy  bool
	}{
		{name: "success", trace: join(swap, []string{"remove old", "policy new", "rename new", "remove helper"}),
			image: newImage, state: selfupdate.StateSucceeded, probe: "after"},
		{name: "the new version reports itself unhealthy, as one that can't use the data does", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the new version reported itself unhealthy, as one whose image is older than this Ludock’s data does", probe: "before"},
		{name: "the new container fails to start", arrange: func(g *rig) { g.fail["start new"] = 500 },
			trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "Docker couldn’t start the new container, for example because its port was still in use", probe: "before"},
		{name: "the new version never reports healthy", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
				}
			}
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the new version didn’t report healthy within 3 minutes", probe: "before"},
		{name: "a put-back without the data copy keeps the newer data, and says so", arrange: func(g *rig) {
			g.fail["start new"] = 500
			g.at["stop new"] = func() { os.Remove(filepath.Join(g.dataDir, selfupdate.CopyName)) }
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataKept,
			reason: "Docker couldn’t start the new container, for example because its port was still in use", probe: "after"},
		{name: "Docker refuses to create the new container", arrange: func(g *rig) { g.fail["create new"] = 400 },
			trace: []string{"pull", "create helper", "start helper", "create new", "remove helper"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t create Ludock’s new container, so nothing was changed", probe: "after"},
		{name: "the pull fails", arrange: func(g *rig) { g.fail["pull"] = 500 }, trace: []string{"pull"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG.",
			probe: "before"},
		{name: "the helper killed mid-swap, then docker compose up -d", arrange: func(g *rig) {
			g.at["start new"] = g.killHelper
			g.fail["start new"] = 500
		}, after: func(g *rig) string {
			g.client.Start(context.Background(), g.old)
			return "ludock"
		}, trace: join(swap, []string{"start old", "remove helper"}), image: oldImage, state: selfupdate.StateInterrupted,
			reason: "the update stopped partway", probe: "after", copy: true},
		{name: "the helper killed mid-swap, then docker compose pull and up -d", arrange: func(g *rig) {
			g.at["start new"] = g.killHelper
			g.fail["start new"] = 500
		}, after: func(g *rig) string {
			// Compose replaces the stopped original with a new container of the new image.
			g.engine.RemoveContainer(g.old)
			id := g.engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: tag, Env: []string{"LUDOCK_DATA_DIR=" + g.dataDir},
				Labels: map[string]string{"com.docker.compose.project": "ludock", "com.docker.compose.service": "ludock"}})
			g.engine.Update(id, func(c *fakedocker.Container) { c.Image = newImage })
			g.client.Start(context.Background(), id)
			return "ludock"
		}, trace: join(swap, []string{"start new", "remove helper"}), image: newImage, state: selfupdate.StateInterrupted,
			reason: "the update stopped partway", probe: "after"},
		{name: "work holds its locks", before: func(g *rig, p *process) {
			if _, err := p.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Fatal(err)
			}
		}, refused: "OPERATION_CONFLICT"},
		{name: "no health check", arrange: func(g *rig) {
			g.engine.SetImageConfig(oldImage, map[string]any{})
			g.engine.Update(g.old, func(c *fakedocker.Container) { c.ExtraConfig = map[string]any{} })
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE"},
		{name: "a host port Docker picks", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.HostConfig["PortBindings"] = map[string]any{"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000-3005"}}}
			})
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			if c.arrange != nil {
				c.arrange(g)
			}
			started := g.engine.Container(g.game).State.StartedAt
			p := g.boot(g.old, oldImage)
			// A write after the copy, which putting the old version back undoes.
			g.at["create helper"] = func() { p.d.SetSetting(ctx, nil, "probe", "after") }
			if c.before != nil {
				c.before(g, p)
			}
			err := p.svc.Start(ctx, "admin", g.session, "")
			if c.refused != "" {
				if !apperr.Is(err, c.refused) || len(g.lines()) != 0 {
					t.Fatalf("Start: %v, trace %q", err, g.lines())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			name := "ludock"
			if c.after != nil {
				select {
				case <-g.killed:
				case <-time.After(5 * time.Second):
					t.Fatal("the helper was never killed")
				}
				name = c.after(g)
			}
			after, running := g.settle(name)
			if got := g.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace\n got %q\nwant %q", got, c.trace)
			}
			if running.Image != c.image {
				t.Errorf("Ludock runs %s", running.Image)
			}
			if game := g.engine.Container(g.game); game.State.StartedAt != started || !game.State.Running {
				t.Error("the game was touched")
			}
			record := after.svc.Last(ctx)
			if record.State != c.state || record.Reason != c.reason || record.Data != c.data {
				t.Errorf("recorded %+v", record)
			}
			var probe string
			if _, err := after.d.Setting(ctx, "probe", &probe); err != nil || probe != c.probe {
				t.Errorf("data %q %v, want %q", probe, err, c.probe)
			}
			if _, err := os.Stat(filepath.Join(g.dataDir, selfupdate.CopyName)); (err == nil) != c.copy {
				t.Errorf("data copy left: %v", err == nil)
			}
			if g.helper() != "" {
				t.Error("the helper is left")
			}
			if release, err := after.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Errorf("work is still held back: %v", err)
			} else {
				release()
			}
		})
	}
}

// helper is the update's helper container's ID, or "".
func (g *rig) helper() string {
	for _, id := range g.engine.SortedIDs() {
		if info := g.engine.Container(id); info != nil && info.Config.Labels["ludock.internal"] == "self-update" {
			return id
		}
	}
	return ""
}

// killHelper ends the helper's run as its host's end would.
func (g *rig) killHelper() {
	g.mu.Lock()
	kill := g.kill
	g.mu.Unlock()
	kill()
	g.engine.Exit(g.helper(), 137)
	close(g.killed)
}
