package selfupdate_test

import (
	"context"
	"database/sql"
	"errors"
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
	tag        = "ghcr.io/crunging/ludock:1"
	oldImage   = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	newImage   = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	movedImage = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	socket     = "/var/run/docker.sock"
)

// rig is one Docker host on the fake daemon, with Ludock's container and a
// game, and the trace of what was asked of Docker in order: mutations by
// the container's role (old, new, helper, its markers), and the data steps
// of putting the old version back. Inspections and the readiness markers
// each Ludock writes on its own schedule aren't traced. A Ludock process
// starts with each Ludock container and dies when it stops, as in its
// container.
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
	// fail fails the first request traced as each line with its status;
	// at runs a function when a line is first traced, before it is served.
	fail map[string]int
	at   map[string]func(r *http.Request)
	// step, when set, answers a data step's exit code in its place.
	step func(cmd []string) (int, bool)
	// pulled, when set, holds the pull until it is closed.
	pulled chan struct{}
	// processes are the Ludock processes by container.
	processes map[string]*process
	// onStart and onStop are a case's own hooks.
	onStart, onStop func(c *fakedocker.Container)
	helpers         []helperRun
	foreign         map[string]bool
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
}

func newRig(t *testing.T) *rig {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	g := &rig{t: t, engine: engine, client: docker.New(engine.Socket), dataDir: t.TempDir(), roles: map[string]string{}, fail: map[string]int{},
		at: map[string]func(*http.Request){}, processes: map[string]*process{}, foreign: map[string]bool{}}
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
			"GroupAdd": []string{"999"}, "PortBindings": map[string]any{"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000"}}},
			"RestartPolicy": map[string]any{"Name": "unless-stopped", "MaximumRetryCount": 0}}
	})
	g.game = engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: true})
	engine.SetImageConfig(newImage, map[string]any{"Healthcheck": health, "Env": []string{"LUDOCK_DATA_DIR=/data"},
		"Labels": map[string]any{"org.opencontainers.image.version": "1.1.0"}})
	engine.PublishImage(tag, newImage)

	engine.Intercept = g.intercept
	engine.OnStart = func(c *fakedocker.Container) {
		if g.onStart != nil {
			g.onStart(c)
		}
		if c.CreateRequest.Labels["ludock.internal"] == "" && c.Config.Labels["com.docker.compose.service"] == "ludock" {
			go g.boot(c.ID, c.Image, "")
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
		if g.onStop != nil {
			g.onStop(c)
		}
	}
	fake.Run(engine, fake.Options{Poll: 5 * time.Millisecond, HealthWait: 200 * time.Millisecond,
		Step: func(cmd []string) (int, bool) {
			step := strings.Join(slices.DeleteFunc(slices.Clone(cmd), func(arg string) bool { return strings.Contains(arg, "-") && len(arg) == 36 }), " ")
			if cmd[0] == "restore-data" {
				step = "restore"
			}
			g.add(strings.TrimPrefix(step, "self-update-data "))
			g.mu.Lock()
			answer := g.step
			g.mu.Unlock()
			if answer != nil {
				return answer(cmd)
			}
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
	match := containerPath.FindStringSubmatch(path)
	switch {
	case path == "/images/create":
		line = "pull"
	case path == "/containers/create":
		line = "create " + g.role(r.URL.Query().Get("name"))
	case match != nil && r.Method != http.MethodGet && verbs[match[2]] != "":
		line = verbs[match[2]] + " " + g.role(match[1])
	}
	if r.Method == http.MethodDelete && match != nil {
		// The Ludock in a removed container dies with it.
		if info := g.engine.Container(match[1]); info != nil {
			g.mu.Lock()
			running := g.processes[info.ID]
			delete(g.processes, info.ID)
			g.mu.Unlock()
			if running != nil {
				running.cancel()
			}
		}
	}
	if line == "" || strings.HasSuffix(line, " data") || strings.HasSuffix(line, " ready") {
		return 0
	}
	g.add(line)
	g.mu.Lock()
	status, pulled, at := g.fail[line], g.pulled, g.at[line]
	delete(g.fail, line)
	delete(g.at, line)
	g.mu.Unlock()
	if at != nil {
		at(r)
	}
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
	case g.foreign[name]:
		return "foreign"
	case strings.Contains(name, "-ready-"):
		role = "ready"
	case strings.HasSuffix(name, "-state"), strings.HasSuffix(name, "-undo"), strings.HasSuffix(name, "-data"), strings.HasSuffix(name, "-healthy"):
		role = name[strings.LastIndex(name, "-")+1:]
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
// name, when set, is how that Ludock finds itself by name.
func (g *rig) boot(container, image, name string) *process {
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		// As Ludock does, it waits with data it can't open, settling nothing.
		return nil
	}
	g.t.Cleanup(func() { d.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	g.t.Cleanup(cancel)
	version := map[string]string{oldImage: "1.0.0", newImage: "1.1.0"}[image]
	found, own, mountID := "mounts", container, container
	if name != "" {
		found, own, mountID = "env", name, ""
	}
	p := &process{d: d, cancel: cancel, svc: &selfupdate.Service{DB: d, Docker: g.client, Own: self.New(g.client, own),
		Updates: &update.Service{Docker: g.client}, Locks: locks.New(), DataDir: g.dataDir, Socket: socket, SelfFound: found, MountID: mountID,
		Version: version, Poll: 5 * time.Millisecond,
		Snapshot: func(ctx context.Context) (*db.Snapshot, error) {
			g.add("save")
			return d.Snapshot(ctx)
		}}}
	g.mu.Lock()
	g.processes[container] = p
	g.mu.Unlock()
	p.svc.Prepare(ctx)
	for p.svc.Recover(ctx, ctx) != nil && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	return p
}

// settle waits for the update to end, as the Ludock under its name records
// it, and returns that Ludock and its container.
func (g *rig) settle() (*process, *docker.ContainerInspect) {
	g.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if running := g.byName("ludock"); running != nil && running.State.Running {
			g.mu.Lock()
			p := g.processes[running.ID]
			g.mu.Unlock()
			if p != nil && !p.svc.Last(context.Background()).InProgress() && !p.svc.Updating() && g.helper() == nil {
				return p, running
			}
		}
		if time.Now().After(deadline) {
			g.t.Fatalf("the update didn't end: %q", g.lines())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// helper is the update's helper container, if one is left.
func (g *rig) helper() *docker.ContainerInspect {
	for _, id := range g.engine.SortedIDs() {
		if info := g.engine.Container(id); info != nil && info.Config.Labels["ludock.internal"] == "self-update" {
			return info
		}
	}
	return nil
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

// The steps every update takes before Docker replaces anything, and those
// that end it.
var (
	begin   = []string{"pull", "save", "create helper", "start helper", "create state", "create new"}
	swap    = []string{"policy old", "stop old"}
	forward = []string{"create healthy", "remove old", "policy new", "rename new", "remove helper"}
	putBack = []string{"create undo", "remove new", "settled", "policy old", "stop old", "aside", "restore", "mark restored", "policy old", "start old", "remove helper"}
	// What the Ludock that comes up removes.
	forwardDone = []string{"remove healthy", "remove state"}
	backDone    = []string{"remove state", "remove undo"}
)

func join(parts ...[]string) []string { return slices.Concat(parts...) }

// Each case updates Ludock from 1.0.0 to the image its tag now points to,
// and checks what ran in what order, which Ludock runs afterwards under its
// name, and what the Ludock that came up recorded. The game is never
// touched, and nothing of the update's is left. A case that stays expects
// the helper to wait with Ludock unable to say it opened its data.
func TestSelfUpdateOrder(t *testing.T) {
	ctx := context.Background()
	var probe error
	cases := []struct {
		name    string
		arrange func(g *rig)
		// before runs before the update is asked for; act once it started,
		// returning the error a second request got, if it made one.
		before func(g *rig, p *process)
		act    func(g *rig, p *process) error
		// self is how Ludock finds itself by name instead of its mounts.
		self   func(g *rig) string
		trace  []string
		image  string
		state  string
		reason string
		to     string
		// refused is the code Update Ludock was refused with.
		refused string
		stays   bool
		check   func(t *testing.T, g *rig, after *process)
	}{
		{name: "success", trace: join(begin, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "the new container fails to start", arrange: func(g *rig) { g.fail["start new"] = 500 },
			trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "Docker couldn’t start the new container, for example because its port was still in use",
			check: func(t *testing.T, g *rig, _ *process) {
				// The data steps reach the data exactly as Ludock does, with its groups.
				for _, create := range g.engine.Creates {
					if create.Labels["ludock.internal"] == "self-update-data" {
						mounts := create.HostConfig.Mounts
						if len(mounts) != 1 || mounts[0].Type != "bind" || mounts[0].Source != g.dataDir || mounts[0].Target != g.dataDir || mounts[0].ReadOnly {
							t.Errorf("data step mounts %+v", mounts)
						}
					}
				}
			}},
		{name: "the new version never reports healthy", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
				}
			}
		}, trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "the new version didn’t report healthy within 3 minutes"},
		{name: "the helper dies after stopping the old container", arrange: func(g *rig) {
			once := sync.Once{}
			g.onStop = func(c *fakedocker.Container) {
				if c.ID == g.old {
					once.Do(g.restartHelper)
				}
			}
		}, trace: join(begin, swap, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "the helper dies after starting the new container", arrange: func(g *rig) {
			once := sync.Once{}
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					once.Do(g.restartHelper)
				}
			}
		}, trace: join(begin, swap, []string{"start new"}, forward, forwardDone), image: newImage, state: selfupdate.StateSucceeded},
		{name: "a newer schema that fails is restored before the old version starts", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
		}, trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "the new version reported itself unhealthy"},
		{name: "a write after the data save is refused, so putting it back loses nothing", arrange: func(g *rig) {
			g.fail["start new"] = 500
			g.at["create helper"] = func(*http.Request) {
				g.mu.Lock()
				old := g.processes[g.old]
				g.mu.Unlock()
				probe = old.d.SetSetting(ctx, nil, "probe", "after")
			}
		}, before: func(g *rig, p *process) {
			if err := p.d.SetSetting(ctx, nil, "probe", "before"); err != nil {
				t.Fatal(err)
			}
		}, trace: join(begin, swap, []string{"start new"}, putBack, backDone), image: oldImage, state: selfupdate.StatePutBack,
			reason: "Docker couldn’t start the new container, for example because its port was still in use",
			check: func(t *testing.T, g *rig, after *process) {
				var value string
				if _, err := after.d.Setting(ctx, "probe", &value); err != nil || value != "before" || !errors.Is(probe, db.ErrUpdating) {
					t.Errorf("probe %q %v, write %v", value, err, probe)
				}
			}},
		{name: "a failed restore is tried again", arrange: func(g *rig) {
			g.fail["start new"] = 500
			tried := false
			g.step = func(cmd []string) (int, bool) {
				if cmd[0] == "restore-data" && !tried {
					tried = true
					return 1, true
				}
				return 0, false
			}
		}, trace: join(begin, swap, []string{"start new", "create undo", "remove new", "settled", "policy old", "stop old", "aside", "restore"},
			[]string{"settled", "policy old", "stop old", "aside", "restore", "mark restored", "policy old", "start old", "remove helper"}, backDone),
			image: oldImage, state: selfupdate.StatePutBack, reason: "Docker couldn’t start the new container, for example because its port was still in use"},
		{name: "a save the old version can't restore keeps the newer data, and the helper waits", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
			g.step = func(cmd []string) (int, bool) { return selfupdate.RestoreFinal, cmd[0] == "restore-data" }
		}, trace: join(begin, swap, []string{"start new", "create undo", "remove new", "settled", "policy old", "stop old", "aside", "restore", "back", "mark kept",
			"policy old", "start old"}), stays: true},
		{name: "the original removed outside before the new one passed is never taken for a cutover", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "unhealthy")
					go g.engine.RemoveContainer(g.old)
				}
			}
		}, trace: join(begin, swap, []string{"start new", "create undo"}), stays: true},
		{name: "the tag moves after the pull", arrange: func(g *rig) {
			g.at["create helper"] = func(*http.Request) {
				g.engine.SetImageConfig(movedImage, map[string]any{})
				g.engine.PublishImage(tag, movedImage)
				g.client.Pull(ctx, tag, "", "")
				g.mu.Lock()
				g.trace = slices.DeleteFunc(g.trace, func(line string) bool { return line == "pull" })
				g.trace = append([]string{"pull"}, g.trace...)
				g.mu.Unlock()
			}
		}, trace: []string{"pull", "save", "create helper", "start helper", "create state", "create undo", "policy old", "start old", "remove helper",
			"remove state", "remove undo"}, image: oldImage, state: selfupdate.StatePutBack, reason: "the image Ludock’s tag points to changed during the update"},
		{name: "a container that took the helper's name is never touched", arrange: func(g *rig) {
			g.at["create helper"] = func(r *http.Request) {
				name := r.URL.Query().Get("name")
				g.mu.Lock()
				g.foreign[name] = true
				g.mu.Unlock()
				g.engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "busybox"})
			}
		}, trace: []string{"pull", "save", "create helper"}, image: oldImage, state: selfupdate.StateFailed,
			reason: "Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment."},
		{name: "the helper never starts", arrange: func(g *rig) { g.fail["start helper"] = 500 },
			trace: []string{"pull", "save", "create helper", "start helper", "stop helper", "remove helper"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment."},
		{name: "a tag moved back to an older version is refused", arrange: func(g *rig) {
			g.engine.SetImageConfig(newImage, map[string]any{"Labels": map[string]any{"org.opencontainers.image.version": "0.9.0"}})
		}, trace: []string{"pull"}, image: oldImage, state: selfupdate.StateFailed, to: "0.9.0",
			reason: "The image Ludock’s tag points to is version 0.9.0, older than this one, which can’t use the data this version keeps. Change the image in its compose.yaml to update."},
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
		}, refused: "OPERATION_CONFLICT"},
		{name: "work holds its locks", before: func(g *rig, p *process) {
			if _, err := p.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Fatal(err)
			}
		}, refused: "OPERATION_CONFLICT"},
		{name: "startup recovery hasn't finished", before: func(g *rig, p *process) { p.svc.Locks.Fence() }, refused: "RECOVERY_PENDING"},
		{name: "a name in the environment that isn't Ludock's own container", self: func(g *rig) string {
			g.engine.AddContainer(fakedocker.GameOptions{Name: "another-panel", Image: tag, Running: true,
				Mounts: []docker.MountPoint{{Type: "bind", Source: t.TempDir(), Destination: g.dataDir, RW: true}}})
			return "another-panel"
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE"},
		{name: "a second click while it updates", arrange: func(g *rig) { g.pulled = make(chan struct{}) },
			act: func(g *rig, p *process) error {
				if !p.svc.Updating() {
					t.Error("work isn't held back while Ludock updates")
				}
				err := p.svc.Start(ctx, "admin", g.session, "")
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
			name := ""
			if c.self != nil {
				name = c.self(g)
			}
			p := g.boot(g.old, oldImage, name)
			if c.before != nil {
				c.before(g, p)
			}
			err := p.svc.Start(ctx, "admin", g.session, "")
			if c.act != nil {
				err = c.act(g, p)
			}
			if c.refused != "" && !apperr.Is(err, c.refused) || c.refused == "" && err != nil {
				t.Fatalf("Start: %v", err)
			}
			if c.state == "" && !c.stays {
				if last := p.svc.Last(ctx); last != nil || len(g.lines()) != 0 {
					t.Fatalf("a refused update ran: %+v %v", last, g.lines())
				}
				return
			}
			if c.stays {
				deadline := time.Now().Add(5 * time.Second)
				for !slices.Equal(g.lines(), c.trace) && time.Now().Before(deadline) {
					time.Sleep(5 * time.Millisecond)
				}
				time.Sleep(300 * time.Millisecond)
				if got := g.lines(); !slices.Equal(got, c.trace) || g.helper() == nil {
					t.Errorf("trace\n got %q\nwant %q, helper %v", got, c.trace, g.helper() != nil)
				}
				return
			}
			after, running := g.settle()
			if got := g.lines(); !slices.Equal(got, c.trace) {
				t.Errorf("trace\n got %q\nwant %q", got, c.trace)
			}
			if running.Image != c.image || running.ID == g.old != (c.image == oldImage) {
				t.Errorf("Ludock runs %s in %s", running.Image, running.ID)
			}
			for _, id := range g.engine.SortedIDs() {
				if name := g.engine.Container(id).Name; strings.Contains(name, "ludock-") && !g.foreign[name[1:]] {
					t.Errorf("%s is left", name)
				}
			}
			if game := g.engine.Container(g.game); game.State.StartedAt != started || !game.State.Running {
				t.Error("the game was touched")
			}
			to := c.to
			if to == "" {
				to = "1.1.0"
			}
			record := after.svc.Last(ctx)
			if record.State != c.state || record.Reason != c.reason || record.From != "1.0.0" || record.To != to {
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
			wantDetails := fmt.Sprintf(`{"from":"1.0.0","outcome":%q,"reason":%q,"to":%q}`, c.state, c.reason, to)
			if user != "admin" || action != "ludock.update."+outcome || details != wantDetails {
				t.Errorf("audit %s %s %s", user, action, details)
			}
			if failed := after.svc.Failed(ctx); (failed != nil) != (c.state != selfupdate.StateSucceeded) {
				t.Errorf("needs attention: %+v", failed)
			}
			if release, err := after.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Errorf("work is still held back: %v", err)
			} else {
				release()
			}
			if err := after.d.SetSetting(ctx, nil, "probe2", "writes"); err != nil {
				t.Errorf("writes are still held back: %v", err)
			}
			if c.check != nil {
				c.check(t, g, after)
			}
		})
	}
}
