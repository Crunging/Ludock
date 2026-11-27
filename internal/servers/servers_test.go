package servers

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/self"
)

var ctx = context.Background()

func setup(t *testing.T) (*fakedocker.Engine, *Service) {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	return engine, &Service{DB: d, Identity: store, Authz: &authz.Service{DB: d, Servers: store},
		Observer: &Observer{Docker: docker.New(engine.Socket), Own: self.New(nil, "")}}
}

func inspectTest(t *testing.T, s *Service, id string) *Observed {
	t.Helper()
	observed, _, err := s.Observer.Inspect(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return observed
}

func ports(entries ...string) map[string][]docker.PortBinding {
	result := map[string][]docker.PortBinding{}
	for _, entry := range entries {
		key, binding, _ := strings.Cut(entry, "=")
		ip, port, _ := strings.Cut(binding, "|")
		result[key] = append(result[key], docker.PortBinding{HostIP: ip, HostPort: port})
	}
	return result
}

func TestPublicMetadataExcludesSecrets(t *testing.T) {
	engine, s := setup(t)
	id := engine.AddContainer(fakedocker.GameOptions{Name: "rust", Image: "didstopia/rust-server", Running: true,
		Labels: map[string]string{"ludock.console": "rust-webrcon", "ludock.private-token": "hidden-token",
			"com.docker.compose.project": "games", "com.docker.compose.service": "rust", "com.docker.compose.container-number": "1"},
		Env:    []string{"RCON_PASSWORD=private-password", "HOME=/root"},
		Mounts: []docker.MountPoint{{Type: "volume", Source: "/var/lib/docker/volumes/rust/_data", Name: "rust", Destination: "/steamcmd/rust", RW: true}}})
	observed := inspectTest(t, s, id)
	text := fmt.Sprintf("%+v", observed.Container)
	for _, secret := range []string{"private-password", "hidden-token", "/var/lib/docker", "com.docker.compose"} {
		if strings.Contains(text, secret) {
			t.Errorf("container exposes %s", secret)
		}
	}
	if len(observed.Observation.DataFolders) != 1 || observed.Observation.DataFolders[0].Source != "rust" || observed.Observation.Compose == nil {
		t.Fatalf("observation lost identity inputs: %+v", observed.Observation)
	}
	if secrets := observed.Observation.Secrets; len(secrets) != 1 || secrets[0] != "private-password" {
		t.Fatal(secrets)
	}
}

// Values of credential-like variables, variables named by ludock.secret-env,
// and URL passwords, decoded and as written, are redacted; flags, working
// directories, and the console password's variable name are ordinary output.
// The longer list of redacted names leaves the identity unchanged.
func TestSecretsFromEnvironment(t *testing.T) {
	engine, s := setup(t)
	env := []string{"RCON_PASSWORD=ab", "DB_PASS=db-secret", "ACCESS_CODE=4321", "PRIVATE_KEY=-----BEGIN KEY", "ENABLE_AUTH_KEY=true",
		"SERVER_PASS=1", "PWD=/data", "MOTD=Welcome", "STATS=postgres://stats:url-secret@db/stats", "INVITE=hunter22",
		"CACHE=redis://:ab@cache", "QUEUE=amqp://queue:p%40ss@mq/", "MIRROR=//mirror:k%2Fey@cdn/maps"}
	labels := map[string]string{"ludock.console.password-env": "RCON_PASSWORD"}
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true, Labels: labels, Env: env})
	before := inspectTest(t, s, id).Observation
	got := strings.Join(before.Secrets, ",")
	if got != "-----BEGIN KEY,url-secret,db-secret,k%2Fey,p%40ss,4321,k/ey,p@ss,ab" {
		t.Fatal(got)
	}
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Labels["ludock.secret-env"] = "INVITE, MOTD" })
	after := inspectTest(t, s, id).Observation
	if got := strings.Join(after.Secrets, ","); got != "-----BEGIN KEY,url-secret,db-secret,hunter22,Welcome,k%2Fey,p%40ss,4321,k/ey,p@ss,ab" {
		t.Fatal(got)
	}
	if identity.BindingDigest(before) != identity.BindingDigest(after) {
		t.Fatal("listing more secrets changed the identity")
	}
}

func TestAddressLabelAndConnectPorts(t *testing.T) {
	engine, s := setup(t)
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server",
		Labels: map[string]string{"ludock.address": "mc.example.com"}, Ports: ports("25565/tcp=0.0.0.0|25565")})
	observed := inspectTest(t, s, id)
	if observed.Container.AddressLabel == nil || observed.Container.AddressLabel.Host != "mc.example.com" || observed.Container.ConnectPort != 25565 {
		t.Fatalf("%+v", observed.Container)
	}
	if strings.Contains(fmt.Sprint(observed.Observation), "ludock.address") {
		t.Fatal("address label affects identity")
	}
	for _, c := range []struct {
		image  string
		labels map[string]string
		ports  []string
		want   int
	}{
		{"factoriotools/factorio", nil, []string{"34197/tcp=0.0.0.0|30000", "34197/udp=0.0.0.0|34197"}, 34197},
		{"factoriotools/factorio", nil, []string{"34197/udp=127.0.0.1|34197", "34197/tcp=0.0.0.0|34197"}, 0},
		{"itzg/minecraft-server", nil, []string{"25575/tcp=0.0.0.0|25575"}, 0},
		{"itzg/minecraft-server", nil, []string{"25565/tcp=127.0.0.1|25565", "25565/tcp=::1|25565", "25575/tcp=0.0.0.0|25575"}, 0},
		{"example/custom-game", map[string]string{"ludock.enable": "true"}, []string{"8080/tcp=127.0.0.1|8080", "7777/udp=|7777"}, 7777},
	} {
		id := engine.AddContainer(fakedocker.GameOptions{Name: "game", Image: c.image, Labels: c.labels, Ports: ports(c.ports...)})
		if got := inspectTest(t, s, id).Container.ConnectPort; got != c.want {
			t.Errorf("%s %v: %d, want %d", c.image, c.ports, got, c.want)
		}
	}
}

// A container's health, run and exit are read as Docker reports them. Its
// game is down since its exit, or since the first exit seen of a crash loop
// it is still in, through the loop's restarts and brief runs; a loop long
// after the last starts again. A brief run a page still holds from a moment
// ago reads as the loop's once Ludock has seen it end in another restart.
func TestHealthUptimeAndExit(t *testing.T) {
	engine, s := setup(t)
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Ports: ports("25575/tcp=0.0.0.0|25575", "25565/tcp=0.0.0.0|25566")})
	engine.Update(id, func(c *fakedocker.Container) {
		c.State.StartedAt = "2026-09-15T10:00:00.000Z"
		c.State.Health = &docker.HealthState{Status: "starting"}
	})
	container := inspectTest(t, s, id).Container
	started, _ := time.Parse(time.RFC3339, "2026-09-15T10:00:00Z")
	if container.Health != "starting" || container.StateSince != started.UnixMilli() || container.Exit != nil ||
		container.GameName != "Minecraft" || container.ConnectPort != 25566 || container.Console == nil ||
		container.Console.Commands[0].Command != "list" || container.Status != "running (starting)" {
		t.Fatalf("%+v", container)
	}
	crashed := engine.AddContainer(fakedocker.GameOptions{Name: "custom", Image: "example/custom-game",
		Labels: map[string]string{"ludock.enable": "true"}, Ports: ports("7777/udp=0.0.0.0|7777")})
	engine.Update(crashed, func(c *fakedocker.Container) {
		c.State.FinishedAt, c.State.ExitCode = "2026-09-15T12:00:00.000Z", 1
		c.State.Health = &docker.HealthState{Status: "unhealthy"}
	})
	stopped := inspectTest(t, s, crashed).Container
	finished, _ := time.Parse(time.RFC3339, "2026-09-15T12:00:00Z")
	if stopped.Health != "" || stopped.StateSince != finished.UnixMilli() || stopped.Exit == nil || stopped.Exit.Code != 1 ||
		stopped.GameName != "Other game" || stopped.ConnectPort != 7777 || stopped.DownSince != finished.UnixMilli() {
		t.Fatalf("%+v", stopped)
	}
	s.Looping = func(context.Context, string, Container) bool { return true }
	var run Container
	for _, step := range []struct {
		state, at string
		want      string
	}{
		{"restarting", "2026-09-15T13:00:00Z", "2026-09-15T13:00:00Z"},
		{"restarting", "2026-09-15T13:00:30Z", "2026-09-15T13:00:00Z"},
		{"running", "2026-09-15T13:00:31Z", "2026-09-15T13:00:00Z"},
		{"restarting", "2026-09-15T13:00:40Z", "2026-09-15T13:00:00Z"},
		{"restarting", "2026-09-15T15:00:00Z", "2026-09-15T15:00:00Z"},
	} {
		engine.Update(id, func(c *fakedocker.Container) {
			c.State.Status, c.State.Running, c.State.Restarting = step.state, true, step.state == "restarting"
			c.RestartCount++
			if step.state == "running" {
				c.State.StartedAt = step.at
			} else {
				c.State.FinishedAt = step.at
			}
		})
		want, _ := time.Parse(time.RFC3339, step.want)
		looping := inspectTest(t, s, id).Container
		if looping.DownSince != want.UnixMilli() {
			t.Fatalf("%s at %s: down since %d", step.state, step.at, looping.DownSince)
		}
		if step.state == "running" {
			run = looping
		}
		if restarting := s.Restarting(ctx, "", run); run.ID != "" && step.at <= "2026-09-15T13:00:40Z" && !restarting {
			t.Fatalf("the run seen at 13:00:31 reads as running once Ludock saw %s at %s", step.state, step.at)
		}
	}
	if s.Restarting(ctx, "", run) {
		t.Fatal("a run two hours before the latest restart Ludock saw reads as the loop's")
	}
}

// A supported game without a status query answers through its image's
// health check, from the last check that passed in this run. Valheim answers
// its query only while it's public, so with SERVER_PUBLIC off the query is
// off and the page says why; a label naming the query keeps it.
func TestHealthCheckStandsInForQuery(t *testing.T) {
	engine, s := setup(t)
	factorio := engine.AddContainer(fakedocker.GameOptions{Name: "factorio", Image: "factoriotools/factorio", Running: true})
	engine.Update(factorio, func(c *fakedocker.Container) {
		c.State.StartedAt = "2026-09-15T10:00:00.000Z"
		c.State.Health = &docker.HealthState{Status: "unhealthy", Log: []docker.HealthResult{
			{End: "2026-09-15T10:01:00.000Z"}, {End: "2026-09-15T10:01:30.000Z"}, {End: "2026-09-15T10:02:00.000Z", ExitCode: 1}}}
	})
	passed, _ := time.Parse(time.RFC3339, "2026-09-15T10:01:30Z")
	if got := inspectTest(t, s, factorio).Container; !got.HealthAnswers || got.HealthyAt != passed.UnixMilli() || got.Query != nil {
		t.Fatalf("%+v", got)
	}
	none := engine.AddContainer(fakedocker.GameOptions{Name: "factorio-plain", Image: "factoriotools/factorio", Running: true})
	off := engine.AddContainer(fakedocker.GameOptions{Name: "factorio-off", Image: "factoriotools/factorio", Running: true,
		Labels: map[string]string{"ludock.query": "none"}})
	engine.Update(off, func(c *fakedocker.Container) { c.State.Health = &docker.HealthState{Status: "healthy"} })
	for _, id := range []string{none, off} {
		if got := inspectTest(t, s, id).Container; got.HealthAnswers {
			t.Fatalf("%+v", got)
		}
	}
	private := engine.AddContainer(fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Running: true,
		Env: []string{"SERVER_PUBLIC=false"}})
	if got := inspectTest(t, s, private).Container; got.Query != nil || !strings.Contains(got.QueryOff, "SERVER_PUBLIC") {
		t.Fatalf("%+v", got)
	}
	labeled := engine.AddContainer(fakedocker.GameOptions{Name: "valheim-labeled", Image: "lloesche/valheim-server", Running: true,
		Env: []string{"SERVER_PUBLIC=0"}, Labels: map[string]string{"ludock.query": "a2s"}})
	public := engine.AddContainer(fakedocker.GameOptions{Name: "valheim-public", Image: "lloesche/valheim-server", Running: true,
		Env: []string{"SERVER_PUBLIC=true"}})
	for _, id := range []string{labeled, public} {
		if got := inspectTest(t, s, id).Container; got.Query == nil || got.QueryOff != "" {
			t.Fatalf("%+v", got)
		}
	}
}

func TestInspectionCacheAndEligibilityRecheck(t *testing.T) {
	engine, s := setup(t)
	now := time.Unix(1000, 0)
	s.Observer.Now = func() time.Time { return now }
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	count := func() int {
		n := 0
		for _, request := range engine.Requests {
			if strings.HasSuffix(request, id+"/json") {
				n++
			}
		}
		return n
	}
	s.Observer.List(ctx)
	s.Observer.List(ctx)
	if count() != 1 {
		t.Fatalf("inspected %d times", count())
	}
	engine.Update(id, func(c *fakedocker.Container) { c.State.Status = "exited"; c.State.Running = false })
	s.Observer.List(ctx)
	if count() != 2 {
		t.Fatal("state change did not re-inspect")
	}
	now = now.Add(31 * time.Second)
	s.Observer.List(ctx)
	if count() != 3 {
		t.Fatal("reuse window not enforced")
	}
	// A server that opts out is refused on inspection, not just left out of lists.
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Labels = map[string]string{"ludock.enable": "false"} })
	now = now.Add(31 * time.Second)
	if _, _, err := s.Observer.Inspect(ctx, id); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if err := s.Observer.ChangeState(ctx, id, Start, nil); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	if _, _, err := s.Observer.Inspect(ctx, "../../etc/passwd"); !apperr.Is(err, "INVALID_CONTAINER_ID") {
		t.Fatal(err)
	}
}

func TestListFailsWholeSnapshot(t *testing.T) {
	engine, s := setup(t)
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	if _, err := s.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	engine.AddFault(".", 500, false, 0)
	if _, err := s.RefreshNow(ctx); err == nil {
		t.Fatal("partial snapshot accepted")
	}
	servers, _ := s.Identity.List(ctx, "")
	if len(servers) != 1 || servers[0].Status != identity.StatusActive {
		t.Fatal("failed discovery marked servers missing")
	}
}

func adminUser(t *testing.T, s *Service) *db.SessionUser {
	t.Helper()
	user := db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"}
	if err := s.DB.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	return &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
}

// Verify authorized rejects replaced container.
func TestVerifyRejectsReplacedContainer(t *testing.T) {
	engine, s := setup(t)
	admin := adminUser(t, s)
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: "/srv/mc", Destination: "/data", RW: true}}})
	views, err := s.List(ctx, admin)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	server := views[0]
	if !server.Can(authz.ServerStop) || len(server.FileRoots) != 1 || server.FileRoots[0].Name != "Minecraft data" {
		t.Fatalf("%+v", server)
	}
	context, err := s.VerifyAuthorized(ctx, admin, server.ID, authz.ServerStop, server.BindingRevision)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(context.LockKeys, ",") != "server:"+server.ID+",path:/srv/mc" {
		t.Fatal(context.LockKeys)
	}
	// An external manager swaps the data mount between authorization and
	// the mutation; the saved binding no longer matches.
	engine.Update(id, func(c *fakedocker.Container) { c.Mounts[0].Source = "/srv/other" })
	if _, err := s.VerifyAuthorized(ctx, admin, server.ID, authz.ServerStop, server.BindingRevision); !apperr.Is(err, "SERVER_BINDING_CHANGED") {
		t.Fatal(err)
	}
	// Stale revisions are rejected too.
	if _, err := s.VerifyAuthorized(ctx, admin, server.ID, authz.ServerStop, server.BindingRevision+5); !apperr.Is(err, "SERVER_BINDING_CHANGED") {
		t.Fatal(err)
	}
}

func TestSnapshotDuringDockerOutage(t *testing.T) {
	engine, s := setup(t)
	admin := adminUser(t, s)
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	views, _ := s.List(ctx, admin)
	engine.AddFault(".", 500, false, 0)
	snapshot, err := s.Snapshot(ctx, admin, views[0].ID)
	if err != nil || !snapshot.DiscoveryUnavailable || snapshot.Server.State != "unknown" || snapshot.Stats != nil {
		t.Fatalf("%+v %v", snapshot, err)
	}
	engine.ClearFaults()
	snapshot, err = s.Snapshot(ctx, admin, views[0].ID)
	if err != nil || snapshot.DiscoveryUnavailable || snapshot.Server.State != "running" || snapshot.Stats == nil {
		t.Fatalf("%+v %v", snapshot, err)
	}
}

func TestStopTimeouts(t *testing.T) {
	info := &docker.ContainerInspect{Config: docker.ContainerConfig{Image: "lloesche/valheim-server"}}
	if StopTimeoutSeconds(info) != 180 {
		t.Fatal(StopTimeoutSeconds(info))
	}
	long, forever := 600, -1
	info.Config.StopTimeout = &long
	if StopTimeoutSeconds(info) != 600 {
		t.Fatal("container timeout ignored")
	}
	info.Config.StopTimeout = &forever
	if StopTimeoutSeconds(info) != -1 {
		t.Fatal("indefinite stop not preserved")
	}
	now := time.Unix(0, 0)
	info.State.Running = true
	state := StopRecoveryState(info, now)
	if !state.StopStarted || state.StopRecoveryDeadlineAt != 5*60*1000 {
		t.Fatalf("%+v", state)
	}
}

// Reads reuse a discovery from the last moments; a Docker write by Ludock,
// a forced refresh, or the window passing discovers again. Viewers of one
// container share a stats sample.
func TestRefreshReusesRecentDiscovery(t *testing.T) {
	engine, s := setup(t)
	now := time.Unix(1000, 0)
	s.Now = func() time.Time { return now }
	s.Observer.Now = s.Now
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	count := func(suffix string) int {
		n := 0
		for _, request := range engine.Requests {
			if strings.Contains(request, suffix) {
				n++
			}
		}
		return n
	}
	listings := func() int { return count("/containers/json") }
	refresh := func(read func(context.Context) (map[string]Container, error)) {
		t.Helper()
		if _, err := read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refresh(s.Refresh)
	refresh(s.Refresh)
	if listings() != 1 {
		t.Fatal("listed", listings(), "times for two reads")
	}
	refresh(s.RefreshNow)
	if listings() != 2 {
		t.Fatal("forced refresh reused a discovery")
	}
	if err := s.Observer.ChangeState(ctx, id, Stop, nil); err != nil {
		t.Fatal(err)
	}
	current, err := s.Refresh(ctx)
	if err != nil || listings() != 3 || current[id].State != "exited" {
		t.Fatal("reused a discovery from before Ludock stopped the server", listings(), current[id].State, err)
	}
	now = now.Add(discoveryReuse)
	refresh(s.Refresh)
	if listings() != 4 {
		t.Fatal("reused an expired discovery")
	}
	engine.Update(id, func(c *fakedocker.Container) { c.State.Status = "running"; c.State.Running = true })
	allow := func(identity.Observation) error { return nil }
	for range 2 {
		if _, err := s.Observer.Stats(ctx, id, allow); err != nil {
			t.Fatal(err)
		}
	}
	if count("/stats") != 1 {
		t.Fatal("sampled", count("/stats"), "times for two viewers")
	}
}

// An update's temporary name fits Docker's 128-character limit for any
// container name, and discovery still hides it.
func TestReplacementNameFitsAndIsHidden(t *testing.T) {
	for _, name := range []string{"mc", "/" + strings.Repeat("a", 127)} {
		temporary := ReplacementName(name, "operation")
		if len(temporary) > 128 || !isReplacement([]string{"/" + temporary}) || isReplacement([]string{name}) {
			t.Error(name, temporary)
		}
	}
}

// Docker's usage counts file cache the kernel can reclaim; the figure shown
// leaves it out as docker stats does, on either cgroup version.
func TestMemoryInUseExcludesReclaimableCache(t *testing.T) {
	const usage = 2000
	for name, cache := range map[string]map[string]float64{
		"cgroup v1":         {"total_inactive_file": 500, "inactive_file": 100},
		"cgroup v2":         {"inactive_file": 500},
		"cache over usage":  {"inactive_file": usage + 1},
		"no cache reported": nil,
	} {
		var stats docker.ContainerStats
		stats.MemoryStats.Usage, stats.MemoryStats.Stats = usage, cache
		want := 1500.0
		if name == "cache over usage" || name == "no cache reported" {
			want = usage
		}
		if got := memoryInUse(&stats); got != want {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
}

// A server's identity follows the data Files shows, nested mounts included,
// and nothing else: a rotated password or another label leaves it as it was.
func TestIdentityFollowsDataFolders(t *testing.T) {
	engine, s := setup(t)
	mounts := []docker.MountPoint{{Type: "volume", Source: "/var/lib/docker/volumes/mc/_data", Name: "mc", Destination: "/data", RW: true},
		{Type: "bind", Source: "/srv/worlds/one", Destination: "/data/world", RW: true},
		{Type: "bind", Source: "/srv/unrelated", Destination: "/backups", RW: true}}
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.files": "/data"}, Env: []string{"RCON_PASSWORD=one", "DB_PASS=one", "RCON_PORT=25575"}, Mounts: mounts})
	base := identity.BindingDigest(inspectTest(t, s, id).Observation)
	digest := func(change func(*fakedocker.Container)) string {
		t.Helper()
		engine.Update(id, change)
		return identity.BindingDigest(inspectTest(t, s, id).Observation)
	}
	if digest(func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=two", "DB_PASS=two", "RCON_PORT=25576", "ENABLE_RCON=false"}
		c.Config.Labels["ludock.console"] = "attach"
		c.Config.Labels["ludock.console.password-env"] = "DB_PASS"
		c.Mounts[2].Source = "/srv/elsewhere"
	}) != base {
		t.Fatal("a setting outside the game and its data folders changed the identity")
	}
	// Another spelling of the same game, or no label where the image names it.
	for _, game := range []string{"Minecraft", " MINECRAFT ", "minecraft", ""} {
		if digest(func(c *fakedocker.Container) { c.Config.Labels["ludock.game"] = game }) != base ||
			inspectTest(t, s, id).Container.GameType != "minecraft" {
			t.Fatalf("game label %q changed the game or identity", game)
		}
	}
	other := digest(func(c *fakedocker.Container) { c.Config.Labels["ludock.game"] = "My Game" })
	if other == base || digest(func(c *fakedocker.Container) { c.Config.Labels["ludock.game"] = "my game" }) != other {
		t.Fatal("an unknown game label's case changed the identity, or the label didn't")
	}
	engine.Update(id, func(c *fakedocker.Container) { delete(c.Config.Labels, "ludock.game") })
	if digest(func(c *fakedocker.Container) { c.Mounts[1].Source = "/srv/worlds/two" }) == base {
		t.Fatal("a different nested world kept the identity")
	}
	if digest(func(c *fakedocker.Container) {
		c.Mounts[1].Source = "/srv/worlds/one"
		c.Config.Labels["ludock.files"] = "/data/world"
	}) == base {
		t.Fatal("other data folders kept the identity")
	}
	// Only --mount settings show a volume's subpath; changing it changes
	// the folder. A volume from --volumes-from may carry one nothing shows,
	// so it is not offered at all.
	subpath := func(folder string) func(*fakedocker.Container) {
		return func(c *fakedocker.Container) {
			c.Config.Labels["ludock.files"] = "/data"
			c.HostConfig = map[string]any{"Mounts": []map[string]any{{"Type": "volume", "Source": "mc", "Target": "/data",
				"VolumeOptions": map[string]any{"Subpath": folder}}}}
		}
	}
	if digest(subpath("worlds/a")) == base || digest(subpath("worlds/a")) == digest(subpath("worlds/b")) {
		t.Fatal("a volume subpath kept the identity")
	}
	engine.Update(id, func(c *fakedocker.Container) { c.HostConfig = map[string]any{"VolumesFrom": []string{"data"}} })
	if roots := inspectTest(t, s, id).Container.FileRoots; len(roots) != 0 {
		t.Fatal(roots)
	}
}

// A game folder that holds Ludock's own backup folder, or a volume Ludock
// keeps its data in (by name or by its folder on the host), is never offered in Files or backups: a member with file
// access could otherwise download Ludock's password hashes and keys.
func TestFileRootsLeaveOutLudocksOwnFolders(t *testing.T) {
	engine, s := setup(t)
	engine.AddContainer(fakedocker.GameOptions{ID: "ludock-self", Name: "ludock-self", Image: "ghcr.io/crunging/ludock:1", Running: true,
		Mounts: []docker.MountPoint{
			{Type: "bind", Source: "/srv/games/ludock-backups", Destination: "/backups", RW: true},
			{Type: "volume", Name: "ludock-data", Source: "/srv/docker/volumes/ludock-data/_data", Destination: "/data", RW: true},
		}})
	s.Observer.Own = self.New(s.Observer.Docker, "ludock-self")
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Mounts: []docker.MountPoint{
			{Type: "bind", Source: "/srv/games", Destination: "/data", RW: true},
			{Type: "volume", Name: "ludock-data", Source: "/srv/docker/volumes/ludock-data/_data", Destination: "/shared", RW: true},
			{Type: "bind", Source: "/srv/docker/volumes", Destination: "/volumes", RW: true},
			{Type: "bind", Source: "/srv/mods", Destination: "/mods", RW: true},
		}})
	want := []files.Root{{ID: files.RootID("/mods"), Name: "mods", Path: "/mods"}}
	listed, err := s.Observer.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, observed := range listed {
		if observed.Container.ID == id {
			found = true
			if !reflect.DeepEqual(observed.Container.FileRoots, want) {
				t.Fatal("listed", observed.Container.FileRoots)
			}
		}
	}
	if !found {
		t.Fatal("game not listed")
	}
	if got := inspectTest(t, s, id).Container.FileRoots; !reflect.DeepEqual(got, want) {
		t.Fatal("inspected", got)
	}
}

// A server is called by its ludock.name label, else by the Compose service
// that made it, numbered for a replica after the first, else by its
// container name. Where a Compose server's name would be another listed
// server's too, its project tells them apart, whichever was found first, and
// its container name where even that is taken. A container discovery turns
// away tells nothing apart. Work on a server calls it by the name saved for
// it, and a server Ludock knows keeps its ID when its name changes.
func TestServerNames(t *testing.T) {
	compose := func(project, service, number string) map[string]string {
		return map[string]string{"com.docker.compose.project": project, "com.docker.compose.service": service,
			"com.docker.compose.container-number": number}
	}
	for _, order := range []bool{false, true} {
		engine, s := setup(t)
		containers := []struct {
			name   string
			labels map[string]string
			want   string
		}{
			{"survival-minecraft-1", compose("survival", "minecraft", "1"), "minecraft (survival)"},
			{"creative-minecraft-1", compose("creative", "minecraft", "1"), "minecraft (creative)"},
			{"valheim-valheim-1", compose("valheim", "valheim", "1"), "valheim"},
			{"valheim-valheim-2", compose("valheim", "valheim", "2"), "valheim 2"},
			{"lobby", nil, "lobby"},
			{"hub-server-1", compose("hub", "server", "1"), "server (hub)"},
			{"server", nil, "server"},
			{"lab-arena-1", compose("lab", "arena", "1"), "lab-arena-1"},
			{"arena", nil, "arena"},
			{"labelled", map[string]string{"ludock.name": "arena (lab)"}, "arena (lab)"},
			{"named-minecraft-1", map[string]string{"com.docker.compose.project": "named", "com.docker.compose.service": "minecraft",
				"com.docker.compose.container-number": "1", "ludock.name": "Friends"}, "Friends"},
		}
		if order {
			slices.Reverse(containers)
		}
		ids := map[string]string{}
		for _, c := range containers {
			ids[c.name] = engine.AddContainer(fakedocker.GameOptions{Name: c.name, Image: "itzg/minecraft-server", Labels: c.labels})
		}
		turnedAway := engine.AddContainer(fakedocker.GameOptions{Name: "spare-valheim-1", Image: "itzg/minecraft-server",
			Labels: compose("spare", "valheim", "1")})
		engine.Update(turnedAway, func(c *fakedocker.Container) { c.Host.Privileged = true })
		if _, err := s.RefreshNow(ctx); err != nil {
			t.Fatal(err)
		}
		saved, _ := s.Identity.List(ctx, "")
		for _, c := range containers {
			server := saved[slices.IndexFunc(saved, func(server identity.Server) bool { return server.ContainerID == ids[c.name] })]
			resolved, err := s.ResolveChecked(ctx, server.ID, -1, func(context.Context) error { return nil })
			if err != nil || server.DisplayName != c.want || resolved.Container.DisplayName != c.want {
				t.Errorf("%s: saved %q, resolved %+v %v, want %q", c.name, server.DisplayName, resolved, err, c.want)
			}
		}
		if len(saved) != len(containers) {
			t.Fatal(saved)
		}
		engine.Update(ids["named-minecraft-1"], func(c *fakedocker.Container) { delete(c.Config.Labels, "ludock.name") })
		if _, err := s.RefreshNow(ctx); err != nil {
			t.Fatal(err)
		}
		after, _ := s.Identity.List(ctx, "")
		renamed := slices.IndexFunc(after, func(server identity.Server) bool { return server.DisplayName == "minecraft (named)" })
		if len(after) != len(saved) || renamed < 0 || !slices.ContainsFunc(saved, func(server identity.Server) bool {
			return server.ID == after[renamed].ID && server.DisplayName == "Friends"
		}) {
			t.Fatal(saved, after)
		}
	}
}
