package selfupdate_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
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
// game, and the trace of what was asked of Docker in order: mutations by
// the container's role (old, new, helper, its state and undo markers), and
// the data steps of putting the old version back. Inspections aren't traced.
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
	roles map[string]string
	// fail fails the first request traced as each line with its status.
	fail map[string]int
	// pulled, when set, holds the pull until it is closed.
	pulled chan struct{}
	// running is the Ludock process of the container that runs it.
	running *process
	// helpers end and report the end of each helper run.
	helpers []helperRun
}

type helperRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// process is one Ludock: it dies with the container it runs in.
type process struct {
	svc    *selfupdate.Service
	d      *db.DB
	cancel context.CancelFunc
	id     string
}

func newRig(t *testing.T) *rig {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	g := &rig{t: t, engine: engine, client: docker.New(engine.Socket), dataDir: t.TempDir(), roles: map[string]string{}, fail: map[string]int{}}
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CreateUser(context.Background(), db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
	g.session = dbtest.Session(t, d, "admin")
	d.Close()

	engine.AddImage(tag)
	health := map[string]any{"Test": []any{"CMD", "/usr/local/bin/ludock", "healthcheck"}}
	engine.SetImageConfig(oldImage, map[string]any{"Healthcheck": health, "Env": []string{"LUDOCK_DATA_DIR=/data"}})
	g.old = engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: tag, Running: true, Env: []string{"LUDOCK_DATA_DIR=" + g.dataDir},
		Labels: map[string]string{"com.docker.compose.project": "ludock", "com.docker.compose.service": "ludock"},
		Mounts: []docker.MountPoint{{Type: "bind", Source: engine.Socket, Destination: socket}, {Type: "bind", Source: g.dataDir, Destination: g.dataDir, RW: true}}})
	engine.Update(g.old, func(c *fakedocker.Container) {
		c.Image = oldImage
		c.ExtraConfig = map[string]any{"Healthcheck": health}
		c.HostConfig = map[string]any{"NetworkMode": "bridge", "Binds": []string{engine.Socket + ":" + socket + ":ro", g.dataDir + ":" + g.dataDir},
			"RestartPolicy": map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0}}
	})
	g.game = engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: true})
	engine.SetImageConfig(newImage, map[string]any{"Healthcheck": health, "Env": []string{"LUDOCK_DATA_DIR=/data"},
		"Labels": map[string]any{"org.opencontainers.image.version": "1.1.0"}})
	engine.PublishImage(tag, newImage)

	engine.Intercept = g.intercept
	// The Ludock in a container dies with it.
	engine.OnStop = func(c *fakedocker.Container) {
		g.mu.Lock()
		running := g.running
		g.mu.Unlock()
		if running != nil && running.id == c.ID {
			running.cancel()
		}
	}
	fake.Run(engine, fake.Options{Poll: 5 * time.Millisecond, HealthWait: 200 * time.Millisecond,
		Step: func(cmd []string) (int, bool) {
			step := strings.Join(slices.DeleteFunc(slices.Clone(cmd), func(arg string) bool { return strings.Contains(arg, "-") && len(arg) == 36 }), " ")
			if cmd[0] == "restore-data" {
				step = "restore"
			}
			g.add(strings.TrimPrefix(step, "self-update-data "))
			return 0, false
		},
		Started: func(cancel context.CancelFunc, done <-chan struct{}) {
			g.mu.Lock()
			g.helpers = append(g.helpers, helperRun{cancel, done})
			g.mu.Unlock()
		}})
	return g
}

func (g *rig) add(line string) {
	g.mu.Lock()
	g.trace = append(g.trace, line)
	g.mu.Unlock()
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
	switch match := containerPath.FindStringSubmatch(path); {
	case path == "/images/create":
		line = "pull"
	case path == "/containers/create":
		line = "create " + g.role(r.URL.Query().Get("name"))
	case match != nil && r.Method != http.MethodGet && verbs[match[2]] != "":
		line = verbs[match[2]] + " " + g.role(match[1])
	}
	if line == "" || strings.HasSuffix(line, " data") {
		return 0
	}
	g.add(line)
	g.mu.Lock()
	status, pulled := g.fail[line], g.pulled
	delete(g.fail, line)
	g.mu.Unlock()
	if line == "pull" && pulled != nil {
		<-pulled
	}
	return status
}

// role names a container by what it is to the update.
func (g *rig) role(reference string) string {
	if reference == g.old {
		return "old"
	}
	g.mu.Lock()
	role, ok := g.roles[reference]
	g.mu.Unlock()
	if ok {
		return role
	}
	name, info := reference, g.engine.Container(reference)
	if info == nil {
		info = g.byName(reference)
	}
	if info != nil {
		name = strings.TrimPrefix(info.Name, "/")
	}
	switch {
	case strings.HasSuffix(name, "-state"):
		role = "state"
	case strings.HasSuffix(name, "-undo"):
		role = "undo"
	case strings.HasSuffix(name, "-data"):
		role = "data"
	case strings.HasPrefix(name, "ludock-self-update-"):
		role = "helper"
	case strings.Contains(name, "-ludock-update-") || name == "ludock":
		role = "new"
	default:
		return name
	}
	if info != nil {
		g.mu.Lock()
		g.roles[info.ID] = role
		g.mu.Unlock()
	}
	return role
}

// boot starts the Ludock of container, which settles an update it finds.
func (g *rig) boot(container, version string) *process {
	g.t.Helper()
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		g.t.Fatal("open:", err)
	}
	g.t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	g.t.Cleanup(cancel)
	p := &process{d: d, cancel: cancel, id: container, svc: &selfupdate.Service{DB: d, Docker: g.client, Own: self.New(g.client, container),
		Updates: &update.Service{Docker: g.client}, Locks: locks.New(), DataDir: g.dataDir, Socket: socket, SelfFound: "mounts", Version: version, Poll: 5 * time.Millisecond,
		Snapshot: func(ctx context.Context) (*db.Snapshot, error) {
			g.add("save")
			return d.Snapshot(ctx)
		}}}
	g.mu.Lock()
	g.running = p
	g.mu.Unlock()
	if err := p.svc.Recover(ctx, ctx); err != nil {
		g.t.Fatal("recover:", err)
	}
	return p
}

// settle waits for the helper to remove itself, then starts the Ludock that
// runs under its name.
func (g *rig) settle(operation string) (*process, *docker.ContainerInspect) {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for g.byName(selfupdate.HelperName(operation)) != nil || !slices.Contains(g.lines(), "remove helper") {
		if time.Now().After(deadline) {
			g.t.Fatalf("the helper is still there after %v", g.lines())
		}
		time.Sleep(5 * time.Millisecond)
	}
	running := g.byName("ludock")
	if running == nil || !running.State.Running {
		g.t.Fatalf("no Ludock runs: %+v", running)
	}
	version := "1.0.0"
	if running.Image == newImage {
		version = "1.1.0"
	}
	return g.boot(running.ID, version), running
}

func (g *rig) byName(name string) *docker.ContainerInspect {
	for _, id := range g.engine.SortedIDs() {
		if info := g.engine.Container(id); info != nil && info.Name == "/"+name {
			return info
		}
	}
	return nil
}

func (g *rig) lines() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.trace)
}

// restartHelper runs the helper again once its run ended, as Docker does
// after it is killed.
func (g *rig) restartHelper() {
	g.mu.Lock()
	run := g.helpers[len(g.helpers)-1]
	g.mu.Unlock()
	run.cancel()
	go func() {
		<-run.done
		var helper *fakedocker.Container
		for _, id := range g.engine.SortedIDs() {
			g.engine.Update(id, func(c *fakedocker.Container) {
				if c.CreateRequest.Labels["ludock.internal"] == "self-update" {
					helper = c
				}
			})
		}
		g.engine.Starts["self-update"](helper)
	}()
}

// The steps every update takes before Docker replaces anything, and those
// that end it.
var (
	begin   = []string{"pull", "save", "create helper", "start helper", "create state", "create new"}
	swap    = []string{"policy old", "stop old"}
	forward = []string{"remove old", "rename new", "policy new", "remove helper"}
	putBack = []string{"create undo", "remove new", "policy old", "stop old", "aside", "restore", "mark restored", "policy old", "start old", "remove helper"}
	// What the Ludock that comes up removes.
	forwardDone = []string{"remove state"}
	backDone    = []string{"remove state", "remove undo"}
)

func join(parts ...[]string) []string { return slices.Concat(parts...) }

// Each case updates Ludock from 1.0.0 to the image its tag now points to,
// and checks what ran in what order, which Ludock runs afterwards under its
// name, and what the Ludock that came up recorded. The game is never
// touched, and nothing of the update's is left.
func TestSelfUpdateOrder(t *testing.T) {
	newerSchema := func(g *rig) {
		g.engine.OnStart = func(c *fakedocker.Container) {
			if c.Image != newImage || c.CreateRequest.Labels["ludock.internal"] != "" {
				return
			}
			// The new version migrates the data, then fails its health check.
			conn, err := sql.Open("sqlite", "file:"+filepath.Join(g.dataDir, "ludock.db"))
			if err == nil {
				_, err = conn.Exec(fmt.Sprintf("PRAGMA user_version = %d", db.LatestVersion()+1))
				conn.Close()
			}
			if err != nil {
				t.Error(err)
			}
			g.engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: "unhealthy"} })
		}
	}
	cases := []struct {
		name    string
		arrange func(g *rig)
		// act runs once the first update started; it returns the error a
		// second request got, if it made one.
		act    func(g *rig, p *process) error
		trace  []string
		image  string
		state  string
		reason string
		// refused is the code Update Ludock was refused with.
		refused string
	}{
		{name: "success", trace: join(begin, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "the new container fails to start", arrange: func(g *rig) { g.fail["start new"] = 500 },
			trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "Docker couldn’t start the new container, for example because its port was still in use"},
		{name: "the new version never reports healthy", arrange: func(g *rig) {
			g.engine.OnStart = func(c *fakedocker.Container) {
				if c.Image == newImage && c.CreateRequest.Labels["ludock.internal"] == "" {
					g.engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: "starting"} })
				}
			}
		}, trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "the new version didn’t report healthy within 3 minutes"},
		{name: "the helper dies after stopping the old container", arrange: func(g *rig) {
			stop := g.engine.OnStop
			once := sync.Once{}
			g.engine.OnStop = func(c *fakedocker.Container) {
				stop(c)
				if c.ID == g.old {
					once.Do(g.restartHelper)
				}
			}
		}, trace: join(begin, swap, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "the helper dies after starting the new container", arrange: func(g *rig) {
			once := sync.Once{}
			g.engine.OnStart = func(c *fakedocker.Container) {
				if c.Image == newImage && c.CreateRequest.Labels["ludock.internal"] == "" {
					once.Do(g.restartHelper)
				}
			}
		}, trace: join(begin, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "a newer schema that fails is restored before the old version starts", arrange: newerSchema,
			trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "the new version reported itself unhealthy"},
		{name: "the helper never starts", arrange: func(g *rig) { g.fail["start helper"] = 500 },
			trace: []string{"pull", "save", "create helper", "start helper", "stop helper", "remove helper"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment."},
		{name: "work is running", arrange: func(g *rig) {
			d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
			if err == nil {
				_, err = d.Exec(`INSERT OR IGNORE INTO docker_hosts (id,name,created_at) VALUES ('host','local',1);
          INSERT INTO logical_servers (id,host_id,external_identity,display_name,game_type,status,binding_revision,binding_fingerprint,first_seen_at,last_seen_at)
          VALUES ('server','host','valheim','Valheim','valheim','active',1,'x',1,1);
          INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,binding_revision,created_at,updated_at)
          VALUES ('op','server','admin','backup','running','backing_up','{}',1,1,1)`)
				d.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
		}, refused: "OPERATION_CONFLICT", image: oldImage},
		{name: "a second click while it updates", arrange: func(g *rig) { g.pulled = make(chan struct{}) },
			act: func(g *rig, p *process) error {
				if !p.svc.Updating() {
					t.Error("changes aren't held back while Ludock updates")
				}
				err := p.svc.Start(context.Background(), "admin", g.session, "")
				close(g.pulled)
				return err
			}, refused: "LUDOCK_UPDATING", trace: join(begin, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			if c.arrange != nil {
				c.arrange(g)
			}
			started := g.engine.Container(g.game).State.StartedAt
			p := g.boot(g.old, "1.0.0")
			err := p.svc.Start(context.Background(), "admin", g.session, "")
			if c.act != nil {
				err = c.act(g, p)
			}
			if c.refused != "" && !apperr.Is(err, c.refused) || c.refused == "" && err != nil {
				t.Fatalf("Start: %v", err)
			}
			last := p.svc.Last(context.Background())
			if c.state == "" {
				if last != nil || len(g.lines()) != 0 {
					t.Fatalf("a refused update ran: %+v %v", last, g.lines())
				}
				return
			}
			after, running := g.settle(last.ID)
			if got := g.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace\n got %q\nwant %q", got, c.trace)
			}
			if running.Image != c.image || running.ID == g.old != (c.image == oldImage) {
				t.Errorf("Ludock runs %s in %s", running.Image, running.ID)
			}
			for _, id := range g.engine.SortedIDs() {
				if name := g.engine.Container(id).Name; strings.Contains(name, "ludock-") {
					t.Errorf("%s is left", name)
				}
			}
			if game := g.engine.Container(g.game); game.State.StartedAt != started || !game.State.Running {
				t.Error("the game was touched")
			}
			record := after.svc.Last(context.Background())
			if record.State != c.state || record.Reason != c.reason || record.From != "1.0.0" || record.To != "1.1.0" {
				t.Errorf("recorded %+v", record)
			}
			// Activity says who updated Ludock, between which versions, and
			// how it ended; Needs attention keeps a failure.
			var user, action, details string
			if err := after.d.QueryRow("SELECT user_id, action, details_json FROM audit_log WHERE target_type='ludock'").Scan(&user, &action, &details); err != nil {
				t.Fatal(err)
			}
			outcome := map[string]string{selfupdate.StateSucceeded: "succeeded"}[c.state]
			if outcome == "" {
				outcome = "failed"
			}
			wantDetails := fmt.Sprintf(`{"from":"1.0.0","outcome":%q,"reason":%q,"to":"1.1.0"}`, c.state, c.reason)
			if user != "admin" || action != "ludock.update."+outcome || details != wantDetails {
				t.Errorf("audit %s %s %s", user, action, details)
			}
			if failed := after.svc.Failed(context.Background()); (failed != nil) != (c.state != selfupdate.StateSucceeded) {
				t.Errorf("needs attention: %+v", failed)
			}
			if release, err := after.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Errorf("work is still held back: %v", err)
			} else {
				release()
			}
		})
	}
}
