package selfupdate_test

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"io"
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
	// alerts are the types of the alerts sent.
	alerts []notify.Alert
	// restarts counts restartOld's restarts.
	restarts int
	// hidden, set by unreadable, is what Docker won't show, for hiddenFor
	// more looks if that isn't 0.
	hidden    string
	hiddenFor int
	// stale is a copy an earlier update left, as it was.
	stale []byte
	// retries counts the restarts Docker gave a Ludock that refused to start.
	retries int
	// held is each container's Ludock's lock on the data folder.
	held map[string]io.Closer
	// done: the case has ended, and boots are the Ludocks still starting.
	done  bool
	boots sync.WaitGroup
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
	g := &rig{t: t, engine: engine, client: docker.New(engine.Socket), dataDir: filepath.Join(t.TempDir(), "data?#1"), fail: map[string]int{},
		at: map[string]func(){}, processes: map[string]*process{}, held: map[string]io.Closer{}, killed: make(chan struct{})}
	// Named with ? and #, which a file URI must escape.
	if err := os.Mkdir(g.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Before the data folder goes, every Ludock still starting has stopped.
	t.Cleanup(func() {
		g.mu.Lock()
		g.done = true
		g.mu.Unlock()
		g.boots.Wait()
	})
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
		// Ludock's own health check answers once it serves: until its
		// start-up check is done, the new version isn't healthy yet. A case
		// may set its own health instead.
		if isNew(c) {
			health(g, c, "booting")
		}
		if g.onStart != nil {
			g.onStart(c)
		}
		if c.CreateRequest.Labels["ludock.internal"] == "" && c.Config.Labels["com.docker.compose.service"] == "ludock" {
			// None starts once the case ends; those starting are waited for.
			g.mu.Lock()
			if !g.done {
				g.boots.Add(1)
				go func() {
					defer g.boots.Done()
					g.boot(c.ID, c.Image, c.State.StartedAt)
				}()
			}
			g.mu.Unlock()
		}
	}
	engine.OnStop = func(c *fakedocker.Container) {
		g.mu.Lock()
		running := g.processes[c.ID]
		delete(g.processes, c.ID)
		held := g.held[c.ID]
		delete(g.held, c.ID)
		g.mu.Unlock()
		// Its lock goes as the process dies, whether or not it had
		// finished starting.
		if held != nil {
			held.Close()
		}
		if running != nil {
			running.cancel()
		}
	}
	fake.Run(engine, fake.Options{Poll: 5 * time.Millisecond, HealthWait: 5 * time.Second,
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

// unreadable makes Docker answer 500 to inspecting any container whose
// reference contains part, until it is called again with "".
func (g *rig) unreadable(part string) { g.unreadableFor(part, 0) }

// unreadableFor is unreadable for the next n looks only, or for all with 0.
func (g *rig) unreadableFor(part string, n int) {
	g.mu.Lock()
	g.hidden, g.hiddenFor = part, n
	g.mu.Unlock()
}

func (g *rig) intercept(r *http.Request) int {
	g.mu.Lock()
	hidden := g.hidden != "" && r.Method == http.MethodGet && strings.Contains(r.URL.Path, g.hidden)
	if hidden && g.hiddenFor > 0 {
		if g.hiddenFor--; g.hiddenFor == 0 {
			g.hidden = ""
		}
	}
	g.mu.Unlock()
	if hidden {
		return 500
	}
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
	ended := func() bool {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.done
	}
	// The helper exits just after it starts the original, which under -race
	// on a loaded machine can take seconds.
	putBack, held, err := selfupdate.BeforeOpen(ctx, g.client, container, g.dataDir, 10*time.Second)
	if err != nil {
		cancel()
		g.refused(container)
		return nil
	}
	g.t.Cleanup(func() { held.Close() })
	g.mu.Lock()
	g.held[container] = held
	g.mu.Unlock()
	if info := g.engine.Container(container); info == nil || !info.State.Running {
		held.Close()
	}
	if ended() {
		held.Close()
		cancel()
		return nil
	}
	d, err := db.Open(filepath.Join(g.dataDir, "ludock.db"))
	if err != nil {
		held.Close()
		cancel()
		return nil
	}
	g.t.Cleanup(func() { d.Close() })
	version := map[string]string{oldImage: "1.0.0", newImage: "1.1.0"}[image]
	p := &process{d: d, cancel: cancel, svc: &selfupdate.Service{DB: d, Docker: g.client, Own: self.New(g.client, container),
		Updates: &update.Service{Docker: g.client}, Locks: locks.New(), DataDir: g.dataDir, Socket: socket, MountID: container,
		Version: version, Poll: 5 * time.Millisecond, PutBack: putBack, Notify: func(alert notify.Alert, _, _ string) {
			g.mu.Lock()
			g.alerts = append(g.alerts, alert)
			g.mu.Unlock()
		}}}
	g.mu.Lock()
	g.processes[container] = p
	g.mu.Unlock()
	// Ludock's own health check answers once it serves, so the helper sees
	// the new version healthy only after its start-up check is done.
	if image == newImage {
		g.engine.Update(container, func(c *fakedocker.Container) {
			if c.State.Health != nil && c.State.Health.Status == "booting" {
				c.State.Health = &docker.HealthState{Status: "healthy"}
			}
		})
	}
	// A run that already ended, before its process was there to die, has none.
	if info := g.engine.Container(container); len(run) > 0 && (info == nil || !info.State.Running || info.State.StartedAt != run[0]) {
		held.Close()
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
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		for _, id := range g.engine.SortedIDs() {
			info := g.engine.Container(id)
			// A name ending in "-" is the start of a temporary one.
			matches := info != nil && (info.Name == "/"+name || strings.HasSuffix(name, "-") && strings.HasPrefix(info.Name, "/"+name))
			if !matches || !info.State.Running {
				continue
			}
			g.mu.Lock()
			p := g.processes[info.ID]
			g.mu.Unlock()
			// Settled, and no take-over still holding work back.
			if p != nil && !p.svc.Last(context.Background()).InProgress() && !p.svc.Updating() {
				if release, err := p.svc.Locks.Acquire([]string{"server:settled"}, nil); err == nil {
					release()
					return p, info
				}
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

func isNewInfo(info *docker.ContainerInspect) bool {
	return info.Image == newImage && info.Config.Labels["ludock.internal"] == ""
}

func health(g *rig, c *fakedocker.Container, status string) {
	g.engine.Update(c.ID, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: status} })
}

var (
	swap = []string{"pull", "create helper", "start helper", "create new", "policy old", "stop old", "start new"}
	// The original, started again, restores its own restart policy.
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
		// settler is the name the Ludock that records it runs under, when
		// not Ludock's.
		settler              string
		noPolicy, onLeftover bool
		// policy is the owner's restart policy, unless-stopped if empty.
		policy string
		// duringCopy runs once the copy is written, in the Ludock that
		// wrote it.
		duringCopy func(t *testing.T, g *rig, p *process)
		// refusesData: the Ludock that comes up can't open the data.
		refusesData bool
		// restarts is how often restartOld restarts the original first.
		restarts int
		// reboot: the case reboots, whose Docker calls depend on timing, so
		// its trace isn't compared.
		reboot bool
		// then runs once it is recorded, as the owner looks at Settings or
		// finishes it.
		then func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect)
	}{
		{name: "success", trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "remove helper"}),
			image: newImage, state: selfupdate.StateSucceeded, probe: "after"},
		{name: "a second Ludock started on the same data folder refuses", trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "remove helper"}),
			then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
				other := g.engine.AddContainer(fakedocker.GameOptions{Name: "ludock-copy", Image: tag, Env: []string{"LUDOCK_DATA_DIR=" + g.dataDir},
					Labels: map[string]string{"com.docker.compose.project": "other", "com.docker.compose.service": "ludock"}})
				g.client.Start(ctx, other)
				time.Sleep(200 * time.Millisecond)
				g.mu.Lock()
				beside := g.processes[other]
				g.mu.Unlock()
				if beside != nil {
					t.Error("a second Ludock started on the same data")
				}
			}, image: newImage, state: selfupdate.StateSucceeded, probe: "after"},
		{name: "an update after one that stopped partway gives the new version the owner's policy, not a leftover no", before: func(g *rig, p *process) {
			// One whose take-over couldn't yet put the policy back.
			earlier := selfupdate.Record{ID: identity.NewUUID(), State: selfupdate.StateInterrupted, Container: g.old, Name: "ludock",
				Policy: json.RawMessage(`{"Name":"unless-stopped","MaximumRetryCount":0}`), NoPolicy: true}
			if err := p.d.SetSetting(ctx, nil, "ludock.update", earlier); err != nil {
				t.Fatal(err)
			}
			g.client.SetRestartPolicy(ctx, g.old, json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`))
		}, trace: join([]string{"policy old"}, swap, []string{"policy old", "policy new", "remove old", "rename new", "remove helper"}),
			image: newImage, state: selfupdate.StateSucceeded, probe: "after"},
		{name: "the new version reports itself unhealthy, as one that can't use the data does", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the new version reported itself unhealthy", probe: "before"},
		{name: "an earlier update's helper left behind doesn't hide this one's put-back", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
			earlier := g.engine.AddContainer(fakedocker.GameOptions{Name: "ludock-self-update-earlier", Image: oldImage, Labels: launch.HelperLabels("self-update")})
			g.engine.Update(earlier, func(c *fakedocker.Container) {
				c.ExtraConfig = map[string]any{"Cmd": []string{"self-update", g.old, newImage, "earlier"}}
				c.Created, c.State.ExitCode = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), selfupdate.ExitUnchanged
			})
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the new version reported itself unhealthy", probe: "before"},
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
		{name: "the original's restart policy fails after it started again, and its data is still put back", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "unhealthy")
				}
			}
			g.at["start old"] = func() { g.fail["policy old"] = 500 }
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, noPolicy: true,
			reason: "the new version reported itself unhealthy", probe: "before"},
		{name: "the original can't be removed, so the new version runs under its temporary name and says how to finish", arrange: func(g *rig) {
			g.fail["remove old"] = 500
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			// No second update starts over what this one left.
			if offer := p.svc.Offer(ctx); offer.Available || !offer.NoCommands || !strings.HasPrefix(offer.Reason, "An earlier update of Ludock didn’t finish") {
				t.Errorf("offer %+v", offer)
			}
			// Once the owner removes the original, removing this container
			// isn't advised any more.
			// While Docker can't say whether the original is there, it gives
			// no command at all.
			g.unreadable(g.old)
			if failed := p.svc.Failed(ctx); failed == nil || !failed.Unchecked || failed.OriginalGone {
				t.Errorf("while Docker can't show the original, Settings shows %+v", failed)
			}
			g.unreadable("")
			g.engine.RemoveContainer(g.old)
			if failed := p.svc.Failed(ctx); failed == nil || !failed.OnLeftover || !failed.OriginalGone {
				t.Errorf("after the original went, Settings shows %+v", failed)
			}
		}, trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "policy old", "policy new", "remove helper"}), image: newImage,
			settler: "ludock-ludock-update-", state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper killed while the new version waits on its health check, which then keeps Ludock's restart policy", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			// A helper of this update, settled now, that Docker kept, doesn't
			// make the original, started by hand, put it back.
			record := p.svc.Last(ctx)
			stale := g.engine.AddContainer(fakedocker.GameOptions{Name: "ludock-self-update-stale", Image: oldImage, Labels: launch.HelperLabels("self-update")})
			g.engine.Update(stale, func(c *fakedocker.Container) {
				c.ExtraConfig = map[string]any{"Cmd": []string{"self-update", g.old, newImage, record.ID}}
				c.State.ExitCode = 137
			})
			beside := func() bool {
				g.mu.Lock()
				defer g.mu.Unlock()
				return g.processes[g.old] != nil
			}
			g.client.Start(ctx, g.old)
			time.Sleep(200 * time.Millisecond)
			if beside() {
				t.Error("the original's Ludock started beside the new one")
			}
			g.client.Stop(ctx, g.old, nil)
			// Nor when Docker can't say whether the new one runs.
			g.unreadable("/ludock-ludock-update-")
			g.client.Start(ctx, g.old)
			time.Sleep(200 * time.Millisecond)
			g.unreadable("")
			if !g.engine.Container(running.ID).State.Running || p.svc.Last(ctx).State != selfupdate.StateInterrupted {
				t.Error("a start of the original after the update settled put it back")
			}
			if beside() {
				t.Error("the original's Ludock started while Docker couldn't show the new one")
			}
		}, trace: join(swap, []string{"policy old", "policy new", "remove helper"}), image: newImage, settler: "ludock-ludock-update-",
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper killed before it gave the new version Ludock's name, which it then takes itself", arrange: func(g *rig) {
			g.fail["rename new"] = 500
			g.at["rename new"] = g.killHelper
		}, trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "rename new", "policy new", "remove helper"}), image: newImage,
			state: selfupdate.StateSucceeded, probe: "after"},
		{name: "a reboot during the health check brings the original back with its data", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.reboot()
				}
			}
		}, reboot: true, image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "a reboot after the new version changed the data brings the original back with its own", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "starting")
					g.reboot()
				}
			}
		}, reboot: true, image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "a reboot with the owner's policy no brings the original back, with no again", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.HostConfig["RestartPolicy"] = map[string]any{"Name": "no", "MaximumRetryCount": 0}
			})
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.reboot()
				}
			}
		}, reboot: true, policy: "no", image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "Docker restarting with the new version still up starts the original, which stops it and puts the update back", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.client.Start(context.Background(), g.old)
					g.killHelper()
				}
			}
		}, reboot: true, image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "the helper killed during the health check, then a reboot, which starts only the new version", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			g.reboot()
			if g.engine.Container(g.old).State.Running || !g.engine.Container(running.ID).State.Running {
				t.Error("the reboot didn't start the new version alone")
			}
		}, image: newImage, settler: "ludock-ludock-update-", reboot: true,
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper killed during the health check while Docker can't show the original, so the new version takes no policy", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					// Once the new version runs, as it does in a real update.
					go func() {
						g.process(c.ID)
						g.unreadable(g.old)
						g.killHelper()
					}()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			var policy struct{ Name string }
			if spec, err := g.client.InspectSpec(ctx, running.ID); err == nil {
				json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
			}
			if failed := p.svc.Failed(ctx); policy.Name != "no" || failed == nil || !failed.OnLeftover || !failed.Unchecked {
				t.Errorf("policy %q, Settings shows %+v", policy.Name, failed)
			}
		}, trace: join(swap, []string{"remove helper"}), image: newImage, settler: "ludock-ludock-update-", noPolicy: true,
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper killed and removed, then the original started, which stops the new version and puts it back", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
					g.engine.RemoveContainer(g.helper())
					g.client.Start(context.Background(), g.old)
				}
			}
		}, reboot: true, image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "a reboot with the owner's policy no, where Docker won't remove the new version or change its policy, still gives no back", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.HostConfig["RestartPolicy"] = map[string]any{"Name": "no", "MaximumRetryCount": 0}
			})
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.reboot()
					g.mu.Lock()
					g.fail["remove new"] = 500
					g.fail["policy new"] = 500
					g.mu.Unlock()
				}
			}
		}, reboot: true, policy: "no", noPolicy: true, image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before",
			then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
				var policy struct{ Name string }
				if spec, err := g.client.InspectSpec(ctx, g.old); err == nil {
					json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
				}
				if policy.Name != "no" {
					t.Errorf("the original's policy is %q", policy.Name)
				}
			}},
		{name: "a reboot after the original went, before the rename, waits for the new version's health check and then takes the name", arrange: func(g *rig) {
			g.fail["rename new"] = 500
			g.at["rename new"] = func() {
				g.killHelper()
				for _, id := range g.engine.SortedIDs() {
					if info := g.engine.Container(id); info != nil && info.Image == newImage && info.Config.Labels["ludock.internal"] == "" {
						set := func(status string) {
							g.engine.Update(id, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: status} })
						}
						set("starting")
						go func() {
							time.Sleep(300 * time.Millisecond)
							set("healthy")
						}()
					}
				}
			}
		}, trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "rename new", "policy new", "remove helper"}), image: newImage,
			state: selfupdate.StateSucceeded, probe: "after"},
		{name: "the owner's policy no, the helper killed during the health check, then a reboot, which starts nothing", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.HostConfig["RestartPolicy"] = map[string]any{"Name": "no", "MaximumRetryCount": 0}
			})
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			g.reboot()
			if g.engine.Container(g.old).State.Running || g.engine.Container(running.ID).State.Running {
				t.Fatal("a reboot started a Ludock whose owner gave it no restart policy")
			}
			// The owner starts the new one, which comes up on its own data.
			g.client.Start(ctx, running.ID)
			var probe string
			if started := g.process(running.ID); started == nil {
				t.Fatal("the new version didn't start")
			} else if started.d.Setting(ctx, "probe", &probe); probe != "after" {
				t.Errorf("the new version runs on %q", probe)
			}
			// Started by hand once that one stopped, the old one runs on the
			// data as it is, and Settings says this Ludock is the old one.
			g.client.Stop(ctx, running.ID, nil)
			g.client.Start(ctx, g.old)
			if old := g.process(g.old); old == nil {
				t.Error("the old version didn't start once the new one stopped")
			} else if failed := old.svc.Failed(ctx); failed == nil || failed.OnLeftover || failed.Leftover != strings.TrimPrefix(running.Name, "/") {
				t.Errorf("the old version's Settings shows %+v", failed)
			}
		}, image: newImage, settler: "ludock-ludock-update-", policy: "no", reboot: true,
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper said updated but couldn't remove the original, which, started by hand beside the new version, refuses", arrange: func(g *rig) {
			g.fail["remove old"] = 500
			g.at["rename new"] = func() { g.client.Start(context.Background(), g.old) }
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			g.mu.Lock()
			beside := g.processes[g.old]
			g.mu.Unlock()
			if beside != nil {
				t.Error("the original's Ludock started beside the new version")
			}
		}, reboot: true, image: newImage, settler: "ludock-ludock-update-",
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the original started by hand as the helper renames, after it said updated, doesn't put back what the new version settled", arrange: func(g *rig) {
			g.fail["remove old"] = 500
			// The original looks at the helper while it still runs, and next
			// once the new version settled the update and removed it.
			g.at["rename new"] = func() {
				g.client.Start(context.Background(), g.old)
				time.Sleep(300 * time.Millisecond)
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			time.Sleep(2 * time.Second)
			g.mu.Lock()
			beside := g.processes[g.old]
			g.mu.Unlock()
			if beside != nil || !g.engine.Container(running.ID).State.Running {
				t.Error("the original stopped the new version or started beside it")
			}
			if record := p.svc.Last(ctx); record.State != selfupdate.StateInterrupted || !record.OnLeftover {
				t.Errorf("the record became %+v", record)
			}
			if _, err := os.Stat(filepath.Join(g.dataDir, selfupdate.CopyName)); err != nil {
				t.Error("the copy went", err)
			}
		}, reboot: true, image: newImage, settler: "ludock-ludock-update-",
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "a new version settling just before the original stops it keeps its data", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					// Once the new version's Ludock is up, Docker starts the
					// original and the helper dies.
					go func() {
						g.process(c.ID)
						g.client.Start(context.Background(), g.old)
						g.killHelper()
					}()
				}
			}
			// As the original stops it, the new version has just settled
			// the update, and may have accepted changes since.
			g.at["stop new"] = func() {
				for _, id := range g.engine.SortedIDs() {
					if info := g.engine.Container(id); info != nil && isNewInfo(info) {
						// As judge settles it: the policy taken by rule 1,
						// the record saved, and its alert sent.
						if settled := g.process(id); settled != nil {
							g.client.SetRestartPolicy(context.Background(), g.old, json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`))
							g.client.SetRestartPolicy(context.Background(), id, json.RawMessage(`{"Name":"unless-stopped","MaximumRetryCount":0}`))
							record := settled.svc.Last(context.Background())
							record.State, record.Reason, record.FinishedAt = selfupdate.StateInterrupted, "the update stopped partway", time.Now().UnixMilli()
							record.Leftover, record.OnLeftover = strings.TrimPrefix(info.Name, "/"), true
							settled.d.SetSetting(context.Background(), nil, "ludock.update", record)
							g.mu.Lock()
							g.alerts = append(g.alerts, notify.Results)
							g.mu.Unlock()
						}
					}
				}
			}
		}, reboot: true, image: oldImage, state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true},
		{name: "a restart during the pull, with an earlier update's copy still there, restores nothing and leaves that copy", before: func(g *rig, p *process) {
			// The earlier copy holds older data than the database.
			p.d.SetSetting(ctx, nil, "probe", "stale")
			if err := p.d.CopyBeside(ctx, selfupdate.CopyName); err != nil {
				t.Fatal(err)
			}
			p.d.SetSetting(ctx, nil, "probe", "live")
			g.stale, _ = os.ReadFile(filepath.Join(g.dataDir, selfupdate.CopyName))
			g.at["pull"] = g.restartOld
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			if left, err := os.ReadFile(filepath.Join(g.dataDir, selfupdate.CopyName)); err != nil || !bytes.Equal(left, g.stale) {
				t.Error("the earlier update's copy was changed", err)
			}
		}, trace: []string{"pull", "stop old", "start old", "policy old"}, image: oldImage, restarts: 1, copy: true,
			state: selfupdate.StateFailed, reason: "the update stopped before Ludock was replaced, so nothing was changed", probe: "live"},
		{name: "a restart during the pull, where Docker first can't show the helper and then shows it missing, leaves an earlier copy", before: func(g *rig, p *process) {
			p.d.SetSetting(ctx, nil, "probe", "stale")
			if err := p.d.CopyBeside(ctx, selfupdate.CopyName); err != nil {
				t.Fatal(err)
			}
			p.d.SetSetting(ctx, nil, "probe", "live")
			g.stale, _ = os.ReadFile(filepath.Join(g.dataDir, selfupdate.CopyName))
			g.at["pull"] = func() {
				g.unreadableFor("ludock-self-update-", 1)
				g.restartOld()
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			if left, err := os.ReadFile(filepath.Join(g.dataDir, selfupdate.CopyName)); err != nil || !bytes.Equal(left, g.stale) {
				t.Error("the earlier update's copy was changed", err)
			}
		}, trace: []string{"pull", "stop old", "start old", "policy old"}, image: oldImage, restarts: 1, copy: true,
			state: selfupdate.StateFailed, reason: "the update stopped before Ludock was replaced, so nothing was changed", probe: "live"},
		{name: "a restart just after the copy, before the helper, with an earlier update's copy there, restores this update's own", arrange: func(g *rig) {
			g.fail["create helper"] = 500
		}, before: func(g *rig, p *process) {
			p.d.SetSetting(ctx, nil, "probe", "stale")
			if err := p.d.CopyBeside(ctx, selfupdate.CopyName); err != nil {
				t.Fatal(err)
			}
			p.d.SetSetting(ctx, nil, "probe", "live")
		}, duringCopy: func(t *testing.T, g *rig, p *process) {
			g.restartOld()
		}, trace: []string{"pull", "create helper", "stop old", "start old", "stop new", "policy old"}, image: oldImage, restarts: 1,
			state: selfupdate.StateFailed, reason: "the update stopped before Ludock was replaced, so nothing was changed", probe: "live"},
		{name: "the helper killed during the health check, then the old one started by hand, which takes the owner's policy, and leaves a later change to it", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			policyOf := func(id string) string {
				var policy struct{ Name string }
				if spec, err := g.client.InspectSpec(ctx, id); err == nil {
					json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
				}
				return policy.Name
			}
			g.client.Stop(ctx, running.ID, nil)
			g.client.Start(ctx, g.old)
			old := g.process(g.old)
			for deadline := time.Now().Add(3 * time.Second); policyOf(g.old) != "unless-stopped" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			if old == nil || policyOf(g.old) != "unless-stopped" || policyOf(running.ID) != "no" {
				t.Fatalf("the old one has %q and the new one %q", policyOf(g.old), policyOf(running.ID))
			}
			// The owner then changes it by hand, as docker update does, and
			// Ludock, started again, leaves it.
			g.client.SetRestartPolicy(ctx, g.old, json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`))
			g.client.Stop(ctx, g.old, nil)
			g.client.Start(ctx, g.old)
			if old = g.process(g.old); old == nil {
				t.Fatal("the old one didn't start again")
			}
			time.Sleep(200 * time.Millisecond)
			if policyOf(g.old) != "no" {
				t.Errorf("a start changed the owner's policy to %q", policyOf(g.old))
			}
			// And the next update keeps it as the owner's.
			g.engine.RemoveContainer(running.ID)
			g.mu.Lock()
			g.fail["pull"] = 500
			g.mu.Unlock()
			if err := old.svc.Start(ctx, "admin", g.session, ""); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); old.svc.Last(ctx).InProgress() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			var policy struct{ Name string }
			json.Unmarshal(old.svc.Last(ctx).Policy, &policy)
			if policy.Name != "no" {
				t.Errorf("the next update recorded the owner's policy as %q", policy.Name)
			}
		}, reboot: true, image: newImage, settler: "ludock-ludock-update-",
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "the helper killed, then the old one started by hand where Docker won't give it the policy, whose leftover no a new update doesn't take", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
			g.client.Stop(ctx, running.ID, nil)
			g.mu.Lock()
			g.fail["policy old"] = 500
			g.mu.Unlock()
			g.client.Start(ctx, g.old)
			old := g.process(g.old)
			for deadline := time.Now().Add(3 * time.Second); old != nil && !old.svc.Last(ctx).NoPolicy && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			if old == nil || !old.svc.Last(ctx).NoPolicy {
				t.Fatal("the refused policy wasn't recorded")
			}
			g.engine.RemoveContainer(running.ID)
			g.mu.Lock()
			g.fail["pull"] = 500
			g.mu.Unlock()
			if err := old.svc.Start(ctx, "admin", g.session, ""); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); old.svc.Last(ctx).InProgress() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			var policy struct{ Name string }
			json.Unmarshal(old.svc.Last(ctx).Policy, &policy)
			if policy.Name != "unless-stopped" {
				t.Errorf("the next update recorded the owner's policy as %q", policy.Name)
			}
		}, reboot: true, image: newImage, settler: "ludock-ludock-update-",
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, onLeftover: true},
		{name: "a put-back without the data copy keeps the newer data, and says so", arrange: func(g *rig) {
			g.fail["start new"] = 500
			g.at["stop new"] = func() { os.Remove(filepath.Join(g.dataDir, selfupdate.CopyName)) }
		}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataKept,
			reason: "Docker couldn’t start the new container, for example because its port was still in use", probe: "after"},
		{name: "the original restarted after it put its data back, before it saved how the update ended, still says it was put back with its data", arrange: func(g *rig) {
			g.fail["start new"] = 500
			// As the original sets its own restart policy back, before it
			// saves how the update ended.
			g.at["policy old"] = func() { g.at["policy old"] = g.restartOld }
		}, trace: join(swap, []string{"stop new", "remove new", "start old", "policy old", "stop old", "start old", "policy old", "remove helper"}),
			image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before", restarts: 1,
			reason: "Docker couldn’t start the new container, for example because its port was still in use"},
		{name: "a helper Docker won't remove once the outcome is saved goes at the next start", arrange: func(g *rig) {
			g.fail["start new"] = 500
			g.fail["remove helper"] = 500
			g.at["remove helper"] = g.restartOld
		}, trace: join(swap, []string{"stop new", "remove new", "start old", "policy old", "remove helper", "stop old", "start old", "remove helper"}),
			image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before", restarts: 1,
			reason: "Docker couldn’t start the new container, for example because its port was still in use"},
		{name: "Docker refuses to create the new container", arrange: func(g *rig) { g.fail["create new"] = 400 },
			trace: []string{"pull", "create helper", "start helper", "create new", "policy old", "remove helper"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t swap in Ludock’s new container, so nothing was changed", probe: "after"},
		{name: "a pull that fails keeps the copy an earlier update left", arrange: func(g *rig) {
			os.WriteFile(filepath.Join(g.dataDir, selfupdate.CopyName), []byte("kept"), 0o600)
			g.fail["pull"] = 500
		}, trace: []string{"pull"}, image: oldImage, copy: true, state: selfupdate.StateFailed, probe: "before",
			reason: "Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG."},
		{name: "the pull fails", arrange: func(g *rig) { g.fail["pull"] = 500 }, trace: []string{"pull"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG.",
			probe: "before"},
		{name: "the helper killed mid-swap, then docker compose up -d, which starts the original and puts the update back", arrange: func(g *rig) {
			g.at["start new"] = g.killHelper
			g.fail["start new"] = 500
		}, after: func(g *rig) string {
			g.client.Start(context.Background(), g.old)
			return "ludock"
		}, trace: join(swap, []string{"start old", "stop new", "remove new", "policy old", "remove helper"}), image: oldImage,
			state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
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
		{name: "work and access changes asked for after the copy are refused, so a put-back can't forget them", arrange: func(g *rig) { g.fail["start new"] = 500 },
			duringCopy: func(t *testing.T, g *rig, p *process) {
				store := identity.Store{DB: p.d}
				games, err := store.Reconcile(context.Background(), []identity.Observation{{ContainerID: g.game, Name: "game", DisplayName: "game", GameType: "minecraft"}}, "", 0)
				if err == nil {
					_, err = (&ops.Runner{DB: p.d, Locks: p.svc.Locks}).Enqueue(context.Background(), nil,
						ops.EnqueueOptions{ServerID: games[0].ID, ActorID: ops.RecoveryActor, Kind: ops.KindRestart})
				}
				if !errors.Is(err, ops.ErrLudockUpdating) {
					t.Errorf("work was accepted: %v", err)
				}
				// Nor is a change to access, such as a revocation, that a
				// put-back would undo.
				ctx, now := context.Background(), db.Now()
				link := p.d.Tx(ctx, func(tx *sql.Tx) error {
					_, err := db.PutAccountLink(ctx, tx, "admin", "link", now, now+time.Hour.Milliseconds())
					return err
				})
				_, used := p.d.UseAccountLink(ctx, db.LinkUse{TokenHash: "link", UserID: "admin", PasswordHash: "y", Session: db.SessionInput{CreatedAt: now}})
				for change, err := range map[string]error{
					"reset link":                 cmp.Or(link, used),
					"recovery":                   p.d.UpdateUserPassword(ctx, "admin", "y", db.AuditEntry{Action: "user.password.recovered"}),
					"two-factor setup":           p.d.SetPendingTwoFactor(ctx, "admin", g.session, "pending", db.Now),
					"two-factor setup cancelled": p.d.ClearPendingTwoFactor(ctx, "admin", g.session, db.Now),
					"two-factor":                 p.d.ChangeTwoFactor(ctx, db.TwoFactorChange{UserID: "admin", TokenHash: g.session, Session: db.SessionInput{CreatedAt: now}}),
					"admin change":               p.d.AdminMutation(ctx, "admin", g.session, now, func(*sql.Tx) error { return nil }),
					"sign-out":                   p.d.Logout(ctx, g.session, db.AuditEntry{Action: "auth.logout"}),
					"password":                   p.d.ChangePassword(ctx, "admin", g.session, "x", "y", "", db.SessionInput{CreatedAt: now}, db.AuditEntry{Action: "auth.password.changed"}),
				} {
					if !errors.Is(err, db.ErrLudockUpdating) {
						t.Errorf("%s was accepted: %v", change, err)
					}
				}
				if _, err := p.d.DeleteUserSessionByID(ctx, "admin", g.session, "x", db.Now, db.AuditEntry{}); !errors.Is(err, db.ErrLudockUpdating) {
					t.Errorf("session revocation was accepted: %v", err)
				}
			}, trace: join(swap, putBack), image: oldImage, state: selfupdate.StatePutBack, data: selfupdate.DataRestored,
			reason: "Docker couldn’t start the new container, for example because its port was still in use", probe: "before"},
		{name: "the helper killed after the new version changed the data, then the original started, which puts its data back", arrange: func(g *rig) {
			g.onStart = func(c *fakedocker.Container) {
				if isNew(c) {
					g.migrate()
					health(g, c, "starting")
					g.killHelper()
				}
			}
		}, after: func(g *rig) string {
			for _, id := range g.engine.SortedIDs() {
				if info := g.engine.Container(id); info != nil && info.Image == newImage {
					g.engine.RemoveContainer(id)
				}
			}
			g.client.Start(context.Background(), g.old)
			return "ludock"
		}, trace: join(swap, []string{"start old", "stop new", "policy old", "remove helper"}), image: oldImage,
			state: selfupdate.StatePutBack, data: selfupdate.DataRestored, reason: "the update was cut off, for example by a restart, before the new version was accepted", probe: "before"},
		{name: "Docker stops the original but its answer is lost", arrange: func(g *rig) {
			g.at["stop old"] = func() { g.engine.Exit(g.old, 0) }
			g.fail["stop old"] = 500
		}, trace: []string{"pull", "create helper", "start helper", "create new", "policy old", "stop old", "remove new", "start old", "policy old", "remove helper"},
			image: oldImage, state: selfupdate.StateFailed, reason: "Docker couldn’t swap in Ludock’s new container, so nothing was changed", probe: "after"},
		{name: "Docker starts the original again but its answer is lost", arrange: func(g *rig) {
			g.fail["start new"] = 500
			g.at["start old"] = func() { g.client.Start(context.Background(), g.old) }
			g.fail["start old"] = 500
		}, trace: join(swap, []string{"stop new", "remove new", "start old", "start old", "policy old", "remove helper"}), image: oldImage,
			state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before",
			reason: "Docker couldn’t start the new container, for example because its port was still in use"},
		{name: "Ludock restarted before it made the helper", arrange: func(g *rig) { g.fail["create helper"] = 500 },
			duringCopy: func(t *testing.T, g *rig, p *process) {
				g.client.Stop(context.Background(), g.old, nil)
				g.client.Start(context.Background(), g.old)
			}, trace: []string{"pull", "create helper", "stop old", "start old", "stop new", "policy old"}, image: oldImage,
			state: selfupdate.StateFailed, reason: "the update stopped before Ludock was replaced, so nothing was changed", probe: "before"},
		{name: "the helper killed after it took the original's restart policy, before it stopped it", arrange: func(g *rig) {
			g.at["stop old"] = g.killHelper
			g.fail["stop old"] = 500
		}, trace: []string{"pull", "create helper", "start helper", "create new", "policy old", "stop old", "remove new", "policy old", "remove helper"},
			image: oldImage, state: selfupdate.StateFailed, reason: "the update stopped before Ludock was replaced, so nothing was changed", probe: "after"},
		{name: "a new container nothing ran in, which Docker won't remove, isn't called nothing changed", arrange: func(g *rig) {
			g.fail["policy old"] = 500
			g.fail["remove new"] = 500
			g.at["remove new"] = func() { g.fail["remove new"] = 500 }
		}, trace: []string{"pull", "create helper", "start helper", "create new", "policy old", "remove new", "remove new", "policy new", "policy old", "remove helper"}, image: oldImage, state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true},
		{name: "a new container left over that Docker can't show isn't called nothing changed", arrange: func(g *rig) {
			g.fail["policy old"] = 500
			g.fail["remove new"] = 500
			g.unreadable("/ludock-ludock-update-")
		}, trace: []string{"pull", "create helper", "start helper", "create new", "policy old", "remove new", "remove helper"}, image: oldImage,
			state: selfupdate.StateInterrupted, reason: "the update stopped partway", probe: "after", copy: true, noPolicy: true,
			then: func(t *testing.T, g *rig, p *process, running *docker.ContainerInspect) {
				if failed := p.svc.Failed(ctx); failed == nil || failed.Finished {
					t.Errorf("while Docker can't show the new container, Settings shows %+v", failed)
				}
			}},
		{name: "a health check of the container's own", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.ExtraConfig = map[string]any{"Healthcheck": map[string]any{"Test": []string{"CMD-SHELL", "nc -z localhost 3000"}}}
			})
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE"},
		{name: "work holds its locks", before: func(g *rig, p *process) {
			if _, err := p.svc.Locks.Acquire([]string{"server:x"}, nil); err != nil {
				t.Fatal(err)
			}
		}, refused: "OPERATION_CONFLICT"},
		{name: "no health check", arrange: func(g *rig) {
			g.engine.SetImageConfig(oldImage, map[string]any{})
			g.engine.Update(g.old, func(c *fakedocker.Container) { c.ExtraConfig = map[string]any{} })
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE"},
		{name: "data kept inside the container, which is said before any other reason", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.Mounts = []docker.MountPoint{{Type: "bind", Source: g.engine.Socket, Destination: socket}}
				c.ExtraConfig = map[string]any{"Healthcheck": map[string]any{"Test": []string{"NONE"}}}
			})
		}, refused: "LUDOCK_UPDATE_UNAVAILABLE", reason: "Ludock keeps its data inside its own container"},
		{name: "the next update replaces a data copy an earlier one left, and its confirmation says so", arrange: func(g *rig) {
			os.WriteFile(filepath.Join(g.dataDir, selfupdate.CopyName), []byte("kept"), 0o600)
		}, before: func(g *rig, p *process) {
			if offer := p.svc.Offer(ctx); !offer.Available || !offer.CopyLeft {
				t.Errorf("offer %+v", offer)
			}
		}, trace: join(swap, []string{"policy old", "policy new", "remove old", "rename new", "remove helper"}), image: newImage,
			state: selfupdate.StateSucceeded, probe: "after"},
		{name: "a host port Docker picks while work runs, which Settings gives as the reason, without the commands", arrange: func(g *rig) {
			g.engine.Update(g.old, func(c *fakedocker.Container) {
				c.HostConfig["PortBindings"] = map[string]any{"3000/tcp": []any{map[string]any{"HostIp": "", "HostPort": "3000-3005"}}}
			})
		}, before: func(g *rig, p *process) {
			games, err := (&identity.Store{DB: p.d}).Reconcile(ctx, []identity.Observation{{ContainerID: g.game, Name: "game", DisplayName: "game", GameType: "minecraft"}}, "", 0)
			if err == nil {
				_, err = (&ops.Runner{DB: p.d, Locks: p.svc.Locks}).Enqueue(ctx, nil, ops.EnqueueOptions{ServerID: games[0].ID, ActorID: ops.RecoveryActor, Kind: ops.KindRestart})
			}
			if offer := p.svc.Offer(ctx); err != nil || !offer.NoCommands || !strings.HasPrefix(offer.Reason, "Wait for the work in progress") {
				t.Errorf("offer %+v, %v", offer, err)
			}
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
			g.at["create helper"] = func() {
				p.d.SetSetting(ctx, nil, "probe", "after")
				if c.duringCopy != nil {
					c.duringCopy(t, g, p)
				}
			}
			if c.before != nil {
				c.before(g, p)
			}
			err := p.svc.Start(ctx, "admin", g.session, "")
			if c.refused != "" {
				if message, _ := apperr.Public(err); !apperr.Is(err, c.refused) || len(g.lines()) != 0 || !strings.HasPrefix(message, c.reason) {
					t.Fatalf("Start: %v, trace %q", err, g.lines())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			name := cmp.Or(c.settler, "ludock")
			if c.after != nil {
				select {
				case <-g.killed:
				case <-time.After(5 * time.Second):
					t.Fatal("the helper was never killed")
				}
				name = c.after(g)
			}
			if c.refusesData {
				time.Sleep(50 * time.Millisecond)
				if d, err := db.Open(filepath.Join(g.dataDir, "ludock.db")); err == nil {
					d.Close()
					t.Error("the data opened")
				}
				if _, err := os.Stat(filepath.Join(g.dataDir, selfupdate.CopyName)); err != nil || selfupdate.Unfinished(g.dataDir) == "" {
					t.Error("the refusal doesn't say the update didn't finish", err)
				}
				return
			}
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				g.mu.Lock()
				restarted := g.restarts >= c.restarts
				g.mu.Unlock()
				if restarted || time.Now().After(deadline) {
					break
				}
			}
			after, running := g.settle(name)
			// A start removes a helper an earlier one couldn't, meanwhile.
			for deadline := time.Now().Add(2 * time.Second); g.helper() != "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			if got := g.lines(); !c.reboot && !slices.Equal(got, c.trace) {
				t.Errorf("trace\n got %q\nwant %q", got, c.trace)
			}
			if running.Image != c.image {
				t.Errorf("Ludock runs %s", running.Image)
			}
			// Whichever of Ludock's containers runs has its restart policy
			// back, so a reboot starts it.
			var policy struct{ Name string }
			if spec, err := g.client.InspectSpec(ctx, running.ID); err == nil {
				json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
			}
			if want := cmp.Or(c.policy, "unless-stopped"); (running.ID == g.old || running.Config.Labels[update.OperationLabel] != "") && !c.noPolicy && policy.Name != want {
				t.Errorf("Ludock's restart policy is %q, want %q", policy.Name, want)
			}
			// One that pulled and ends on the old version leaves Ludock's tag on
			// it, so docker compose up doesn't start the rejected image.
			if record := after.svc.Last(ctx); running.Image == oldImage && (record.State == selfupdate.StatePutBack || record.State == selfupdate.StateFailed) {
				if id, _ := g.client.InspectImage(ctx, tag); id != oldImage {
					t.Errorf("Ludock's tag points at %s", id)
				}
			}
			if game := g.engine.Container(g.game); game.State.StartedAt != started || !game.State.Running {
				t.Error("the game was touched")
			}
			record := after.svc.Last(ctx)
			if record.State != c.state || record.Reason != c.reason || record.Data != c.data || record.NoPolicy != c.noPolicy ||
				record.OnLeftover != c.onLeftover || c.onLeftover && record.Leftover != strings.TrimPrefix(running.Name, "/") {
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
			// A put-back leaves no new container, running or not.
			for _, id := range g.engine.SortedIDs() {
				if info := g.engine.Container(id); c.state == selfupdate.StatePutBack && record.Leftover == "" && info != nil && info.Config.Labels[update.OperationLabel] != "" {
					t.Errorf("the new container %s is left", info.Name)
				}
			}
			if c.then != nil {
				c.then(t, g, after, running)
			}
			// One that didn't succeed alerts as Restores and updates.
			g.mu.Lock()
			alerts := slices.Clone(g.alerts)
			g.mu.Unlock()
			if unsuccessful := c.state != selfupdate.StateSucceeded && c.state != selfupdate.StateCurrent; unsuccessful != (len(alerts) > 0) ||
				slices.ContainsFunc(alerts, func(alert notify.Alert) bool { return alert != notify.Results }) {
				t.Errorf("alerts %v", alerts)
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
		if info := g.engine.Container(id); info != nil && info.Config.Labels["ludock.internal"] == "self-update" && info.Name != "/ludock-self-update-earlier" {
			return id
		}
	}
	return ""
}

// reboot stops Ludock's containers and its helper, as a host restart does,
// then starts those Docker starts after one: any with a restart policy. The
// game is left out, as Ludock never touches it.
func (g *rig) reboot() {
	ctx := context.Background()
	for _, id := range g.engine.SortedIDs() {
		switch info := g.engine.Container(id); {
		case info == nil || id == g.game || !info.State.Running:
		case info.Config.Labels["ludock.internal"] == "self-update":
			g.killHelper()
		default:
			g.client.Stop(ctx, id, nil)
		}
	}
	for _, id := range g.engine.SortedIDs() {
		var policy struct{ Name string }
		if spec, err := g.client.InspectSpec(ctx, id); err == nil && id != g.game {
			json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
		}
		if policy.Name != "" && policy.Name != "no" {
			g.client.Start(ctx, id)
		}
	}
}

// refused ends a Ludock that wouldn't start, as cmd/ludock exits with 1, so
// its container stops; Docker then starts it again if its restart policy
// says so, after a delay that grows, as it does.
func (g *rig) refused(container string) {
	g.engine.Exit(container, 1)
	var policy struct{ Name string }
	if spec, err := g.client.InspectSpec(context.Background(), container); err == nil {
		json.Unmarshal(spec.HostConfig["RestartPolicy"], &policy)
	}
	g.mu.Lock()
	g.retries++
	tries := g.retries
	g.mu.Unlock()
	if policy.Name != "" && policy.Name != "no" && tries <= 8 {
		time.AfterFunc(time.Duration(10<<tries)*time.Millisecond, func() {
			g.mu.Lock()
			done := g.done
			g.mu.Unlock()
			if !done {
				g.client.Start(context.Background(), container)
			}
		})
	}
}

// process waits for the Ludock of container to start, or nil.
func (g *rig) process(container string) *process {
	// As long as a case waits for one to settle, since under -race on one
	// loaded core a start can take seconds.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		g.mu.Lock()
		p := g.processes[container]
		g.mu.Unlock()
		if p != nil {
			return p
		}
	}
	return nil
}

// restartOld ends the original's Ludock as a crash would, so nothing it does
// next is saved, and starts the original again.
func (g *rig) restartOld() {
	g.mu.Lock()
	running := g.processes[g.old]
	g.mu.Unlock()
	running.d.Close()
	g.client.Stop(context.Background(), g.old, nil)
	g.client.Start(context.Background(), g.old)
	g.mu.Lock()
	g.restarts++
	g.mu.Unlock()
}

// killHelper ends the helper's run as its host's end would.
func (g *rig) killHelper() {
	select {
	case <-g.killed:
		return
	default:
	}
	g.mu.Lock()
	kill := g.kill
	g.mu.Unlock()
	kill()
	g.engine.Exit(g.helper(), 137)
	close(g.killed)
}

// A helper Docker can't say anything about, long after any swap would have
// ended, is given up on: work is let go and Settings says the update didn't
// finish, with the commands.
func TestSelfUpdateGivesUpOnAnUnreadableHelper(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	p := g.boot(g.old, oldImage)
	record := selfupdate.Record{ID: identity.NewUUID(), State: selfupdate.StateUpdating, Container: g.old, Name: "ludock",
		StartedAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := p.d.SetSetting(ctx, nil, "ludock.update", record); err != nil {
		t.Fatal(err)
	}
	// Docker can't say anything about the helper, nor this Ludock's own
	// container, so the copy stays.
	os.WriteFile(filepath.Join(g.dataDir, selfupdate.CopyName), []byte("copy"), 0o600)
	g.engine.Intercept = func(r *http.Request) int {
		if strings.Contains(r.URL.Path, selfupdate.HelperName(record.ID)) || strings.Contains(r.URL.Path, g.old) {
			return 500
		}
		return 0
	}
	p.svc.Recover(ctx, ctx)
	for deadline := time.Now().Add(5 * time.Second); p.svc.Updating(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("work is still held back")
		}
	}
	if last := p.svc.Last(ctx); last.State != selfupdate.StateInterrupted {
		t.Errorf("recorded %+v", last)
	}
	if _, err := os.Stat(filepath.Join(g.dataDir, selfupdate.CopyName)); err != nil {
		t.Error("the copy went", err)
	}

	// Nor when the owner recreated Ludock with Compose long after an update
	// cut off before its copy, and only the helper can't be read: the copy
	// there may be an earlier update's.
	g = newRig(t)
	p = g.boot(g.old, oldImage)
	record = selfupdate.Record{ID: identity.NewUUID(), State: selfupdate.StateUpdating, Container: g.old, Name: "ludock",
		StartedAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := p.d.SetSetting(ctx, nil, "ludock.update", record); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(g.dataDir, selfupdate.CopyName), []byte("copy"), 0o600)
	// Removing it, as Compose does, ends the Ludock in it first.
	g.client.Stop(ctx, g.old, nil)
	g.engine.RemoveContainer(g.old)
	recreated := g.engine.AddContainer(fakedocker.GameOptions{Name: "ludock", Image: tag, Env: []string{"LUDOCK_DATA_DIR=" + g.dataDir},
		Labels: map[string]string{"com.docker.compose.project": "ludock", "com.docker.compose.service": "ludock"}})
	g.unreadable(selfupdate.HelperName(record.ID))
	g.client.Start(ctx, recreated)
	started := g.process(recreated)
	for deadline := time.Now().Add(5 * time.Second); started != nil && started.svc.Last(ctx).InProgress(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the update didn't end")
		}
	}
	if _, err := os.Stat(filepath.Join(g.dataDir, selfupdate.CopyName)); started == nil || err != nil {
		t.Error("the copy went", started == nil, err)
	}
}

// A database db.Open would start afresh or refuse never stops the start
// before it: an empty file holds no update, and one db.Open refuses gets
// db.Open's own message, not a failure to look for an update.
func TestBeforeOpenLeavesNewAndRefusedDataToOpen(t *testing.T) {
	ctx := context.Background()
	for name, content := range map[string][]byte{"empty": nil, "refused": []byte(strings.Repeat("not a database ", 20))} {
		dataDir := t.TempDir()
		path := filepath.Join(dataDir, "ludock.db")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		client := docker.New(filepath.Join(dataDir, "no-docker.sock"))
		putBack, held, err := selfupdate.BeforeOpen(ctx, client, "ludock", dataDir, time.Second)
		if err != nil || putBack != "" {
			t.Errorf("%s: BeforeOpen %q %v", name, putBack, err)
		}
		if held != nil {
			held.Close()
		}
		d, err := db.Open(path)
		if (err == nil) != (name == "empty") {
			t.Errorf("%s: db.Open %v", name, err)
		}
		if d != nil {
			d.Close()
		}
	}
}
