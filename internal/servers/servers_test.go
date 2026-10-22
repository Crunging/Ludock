package servers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
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
		Observer: &Observer{Docker: docker.New(engine.Socket)}}
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
	if observed.Observation.GameConfiguration["env:RCON_PASSWORD"] != "private-password" || observed.Observation.Compose == nil {
		t.Fatal("observation lost identity inputs")
	}
	if secrets := Secrets(observed.Observation); len(secrets) != 1 || secrets[0] != "private-password" {
		t.Fatal(secrets)
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
		stopped.GameName != "Other game" || stopped.ConnectPort != 7777 {
		t.Fatalf("%+v", stopped)
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
	// The listing says eligible, but the fresh inspection opts out.
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Labels = map[string]string{"ludock.enable": "false"} })
	now = now.Add(31 * time.Second)
	observed, err := s.Observer.List(ctx)
	if err != nil || len(observed) != 1 {
		// Listing summaries include labels, so the list itself now excludes it.
		t.Logf("observed %d", len(observed))
	}
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
	engine.Fail = 500
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
	engine.Fail = 500
	snapshot, err := s.Snapshot(ctx, admin, views[0].ID)
	if err != nil || !snapshot.DiscoveryUnavailable || snapshot.Server.State != "unknown" || snapshot.Stats != nil {
		t.Fatalf("%+v %v", snapshot, err)
	}
	engine.Fail = 0
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
