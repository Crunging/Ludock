package servers

import (
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
)

// The owner's ludock.enable label decides alone; otherwise a container that
// controls the host is left out whatever its image, a one-off is never
// added, the panel's choice comes next, and a recognized image last.
func TestChoicePrecedence(t *testing.T) {
	add, remove := identity.Choice{Managed: true}, identity.Choice{}
	socket := []docker.MountPoint{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}}
	run := []docker.MountPoint{{Type: "bind", Source: "/run/", Destination: "/host/run"}}
	data := []docker.MountPoint{{Type: "bind", Source: "/srv/games/run", Destination: "/data"}}
	for _, test := range []struct {
		name      string
		candidate candidate
		choice    *identity.Choice
		managed   bool
		unaddable string
		fromPanel bool
	}{
		{"unknown image", candidate{image: "ich777/steamcmd:valheim"}, nil, false, "", false},
		{"added", candidate{image: "ich777/steamcmd:valheim"}, &add, true, "", true},
		{"recognized", candidate{image: "lloesche/valheim-server"}, nil, true, "", false},
		{"recognized removed", candidate{image: "lloesche/valheim-server"}, &remove, false, "", false},
		{"label false beats add", candidate{image: "ich777/steamcmd:valheim", labels: map[string]string{"ludock.enable": "False "}}, &add, false, "label", false},
		{"invalid label beats add", candidate{image: "ich777/steamcmd:valheim", labels: map[string]string{"ludock.enable": "yes"}}, &add, false, "label", false},
		{"label true beats remove", candidate{image: "ich777/steamcmd:valheim", labels: map[string]string{"ludock.enable": "true"}}, &remove, true, "label", false},
		{"one-off", candidate{image: "ich777/steamcmd:valheim", labels: map[string]string{"com.docker.compose.oneoff": "True"}}, &add, false, "one-off", false},
		{"docker socket", candidate{image: "portainer/portainer-ce", mounts: socket}, &add, false, "host", false},
		{"host run folder", candidate{image: "ich777/steamcmd:valheim", mounts: run}, &add, false, "host", false},
		{"parent of Docker's data", candidate{image: "ich777/steamcmd:valheim", mounts: []docker.MountPoint{{Type: "bind", Source: "/var/lib", Destination: "/host"}}}, &add, false, "host", false},
		{"containerd sockets", candidate{image: "ich777/steamcmd:valheim", mounts: []docker.MountPoint{{Type: "bind", Source: "/run/containerd", Destination: "/c"}}}, &add, false, "host", false},
		{"ordinary data folder", candidate{image: "ich777/steamcmd:valheim", mounts: data}, &add, true, "", true},
		{"privileged", candidate{image: "ich777/steamcmd:valheim", privileged: true}, &add, false, "host", false},
		{"Ludock itself", candidate{image: "ghcr.io/crunging/ludock:0.7.0"}, &add, false, "ludock", false},
		{"update replacement", candidate{name: "valheim-ludock-update-0a1b2c3d", image: "ich777/steamcmd:valheim"}, &add, false, "update", false},
		{"recognized with a Docker socket", candidate{image: "lloesche/valheim-server", mounts: socket}, nil, false, "host", false},
		{"recognized and privileged", candidate{image: "lloesche/valheim-server", privileged: true}, nil, false, "host", false},
		{"recognized binding the host's root", candidate{image: "lloesche/valheim-server", mounts: []docker.MountPoint{{Type: "bind", Source: "/", Destination: "/host"}}}, nil, false, "host", false},
		{"label true with a Docker socket", candidate{image: "lloesche/valheim-server", mounts: socket, labels: map[string]string{"ludock.enable": "true"}}, nil, true, "label", false},
		{"label true and privileged beats remove", candidate{image: "ich777/steamcmd:valheim", privileged: true, labels: map[string]string{"ludock.enable": "true"}}, &remove, true, "label", false},
		{"incomplete Compose identity", candidate{image: "ich777/steamcmd:valheim", labels: map[string]string{"com.docker.compose.project": "games"}}, &add, false, "identity", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := test.candidate
			c.id = "0123456789ab"
			if c.name == "" {
				c.name = "game"
			}
			choices := map[string]identity.Choice{}
			if test.choice != nil {
				choices[c.external()] = *test.choice
			}
			labels, panel := c.withChoice(choices)
			if managed := games.Evaluate(c.image, labels).Eligible; managed != test.managed || panel != test.fromPanel {
				t.Fatal(managed, panel)
			}
			if reason := c.unaddable(); reason != test.unaddable {
				t.Fatal(reason)
			}
		})
	}
}

// A choice follows the Compose service, not the container: a recreated
// container keeps the server, its game, and its logical ID.
func TestChoiceSurvivesRecreation(t *testing.T) {
	engine, s := setup(t)
	s.Observer.Choices = s.Identity
	labels := map[string]string{"com.docker.compose.project": "games", "com.docker.compose.service": "valheim",
		"com.docker.compose.container-number": "1"}
	options := fakedocker.GameOptions{Name: "games-valheim-1", Image: "ich777/steamcmd:latest", Labels: labels,
		Ports: ports("2456/udp=0.0.0.0|2456")}
	first := engine.AddContainer(options)
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 0 {
		t.Fatal(current, err)
	}
	if err := s.DB.SetSetting(ctx, nil, "server.choice:compose:games:valheim:1", identity.Choice{Managed: true, Game: "valheim"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.RefreshNow(ctx)
	if err != nil || before[first].GameType != "valheim" || before[first].Source != SourcePanel || before[first].ConnectPort != 2456 {
		t.Fatal(before, err)
	}
	servers, _ := s.Identity.List(ctx, "")
	engine.RemoveContainer(first)
	second := engine.AddContainer(options)
	after, err := s.RefreshNow(ctx)
	if err != nil || after[second].GameType != "valheim" || after[second].Source != SourcePanel {
		t.Fatal(after, err)
	}
	again, _ := s.Identity.List(ctx, "")
	if len(servers) != 1 || len(again) != 1 || again[0].ID != servers[0].ID || again[0].ContainerID != second ||
		again[0].Status != identity.StatusActive {
		t.Fatal(servers, again)
	}
	// Taking it out leaves the server's record, hidden, for adding it again.
	if err := s.DB.SetSetting(ctx, nil, "server.choice:compose:games:valheim:1", identity.Choice{}); err != nil {
		t.Fatal(err)
	}
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 0 {
		t.Fatal(current, err)
	}
	if listed, _ := s.Identity.List(ctx, ""); len(listed) != 0 {
		t.Fatal(listed)
	}
	if kept, _ := s.Identity.Get(ctx, servers[0].ID); kept == nil || kept.Status != identity.StatusMissing {
		t.Fatal(kept)
	}
}

func TestSuggestGame(t *testing.T) {
	for _, test := range []struct {
		image, name string
		ports       []games.Port
		want        string
	}{
		{"ich777/steamcmd:valheim", "server", nil, "valheim"},
		{"registry.example:5000/ich777/steamcmd:satisfactory@sha256:00", "x", nil, "satisfactory"},
		{"gameservermanagers/gameserver:latest", "pz-dedicated", nil, "unknown"},
		{"custom/server", "project-zomboid", nil, "project-zomboid"},
		{"custom/server", "vrising-eu", nil, "v-rising"},
		{"custom/ark-survival-ascended", "x", nil, "ark-survival-ascended"},
		{"custom/server", "box", []games.Port{{Port: 7777, Protocol: "udp"}}, "unknown"},
		{"custom/server", "box", []games.Port{{Port: 25565, Protocol: "tcp"}}, "minecraft"},
		{"lloesche/valheim-server", "x", nil, "valheim"},
	} {
		if got := games.Suggest(test.image, test.name, test.ports); got != test.want {
			t.Errorf("%s %s: %s", test.image, test.name, got)
		}
	}
}

// A game on a recognized image that mounts the Docker socket is not
// discovered, is listed as controlling the host, and can't be added from
// the panel; its owner's label manages it.
func TestHostControlNeedsOwnerLabel(t *testing.T) {
	engine, s := setup(t)
	s.Observer.Choices = s.Identity
	socket := []docker.MountPoint{{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}}
	options := fakedocker.GameOptions{Name: "valheim", Image: "lloesche/valheim-server", Mounts: socket}
	id := engine.AddContainer(options)
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 0 {
		t.Fatal(current, err)
	}
	list, err := s.Observer.Unmanaged(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || !list[0].ControlsHost || list[0].Game != "valheim" {
		t.Fatal(list, err)
	}
	if _, err := s.Observer.Addable(ctx, id); !apperr.Is(err, "NOT_ADDABLE") {
		t.Fatal(err)
	}
	if _, _, err := s.Observer.Inspect(ctx, id); !apperr.Is(err, "FORBIDDEN") {
		t.Fatal(err)
	}
	engine.RemoveContainer(id)
	options.Labels = map[string]string{"ludock.enable": "true"}
	id = engine.AddContainer(options)
	current, err := s.RefreshNow(ctx)
	if err != nil || current[id].Source != SourceLabel || !current[id].ControlsHost {
		t.Fatal(current, err)
	}
	if list, err := s.Observer.Unmanaged(ctx); err != nil || len(list) != 0 {
		t.Fatal(list, err)
	}
	// Docker merges an image's labels into its containers'; an image that
	// labels itself in isn't its owner saying so.
	info, _ := s.Observer.Docker.Inspect(ctx, id)
	engine.SetImageConfig(info.Image, map[string]any{"Labels": map[string]string{"ludock.enable": "true"}})
	engine.RemoveContainer(id)
	engine.AddContainer(options)
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 0 {
		t.Fatal(current, err)
	}
}

// A managed server recreated as privileged leaves discovery as a relabeled
// one does: it is missing, nobody it is shared with can use it, and its
// sharing returns with it once it no longer controls the host.
func TestHostControlLaterLeavesDiscovery(t *testing.T) {
	engine, s := setup(t)
	s.Observer.Choices = s.Identity
	admin := &db.SessionUser{ID: "11111111-1111-4111-8111-111111111111", Username: "owner", Role: db.RoleAdmin}
	friend := &db.SessionUser{ID: "22222222-2222-4222-8222-222222222222", Username: "friend", Role: db.RoleMember}
	for _, user := range []*db.SessionUser{admin, friend} {
		if err := s.DB.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1},
			PasswordHash: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	labels := map[string]string{"com.docker.compose.project": "games", "com.docker.compose.service": "valheim",
		"com.docker.compose.container-number": "1"}
	options := fakedocker.GameOptions{Name: "games-valheim-1", Image: "lloesche/valheim-server", Labels: labels}
	first := engine.AddContainer(options)
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 1 || current[first].ControlsHost {
		t.Fatal(current, err)
	}
	servers, _ := s.Identity.List(ctx, "")
	serverID := servers[0].ID
	if err := s.Authz.Share(ctx, admin, dbtest.Session(t, s.DB, admin.ID), friend.ID, serverID, authz.LevelRun.Capabilities()); err != nil {
		t.Fatal(err)
	}
	engine.RemoveContainer(first)
	privileged := engine.AddContainer(options)
	engine.Update(privileged, func(c *fakedocker.Container) { c.Host.Privileged = true })
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 0 {
		t.Fatal(current, err)
	}
	if kept, _ := s.Identity.Get(ctx, serverID); kept == nil || kept.Status != identity.StatusMissing || kept.ContainerID != "" {
		t.Fatal(kept)
	}
	if allowed, _ := s.Authz.Has(ctx, friend, serverID, authz.ServerStart); allowed {
		t.Fatal("a member can use a server that controls the host")
	}
	if list, err := s.Observer.Unmanaged(ctx); err != nil || len(list) != 1 || !list[0].ControlsHost {
		t.Fatal(list, err)
	}
	engine.RemoveContainer(privileged)
	second := engine.AddContainer(options)
	if current, err := s.RefreshNow(ctx); err != nil || len(current) != 1 || current[second].ControlsHost {
		t.Fatal(current, err)
	}
	shares, err := s.Authz.Sharing(ctx, serverID)
	if err != nil || len(shares) != 1 || !slices.Contains(shares[0].Capabilities, authz.ServerStart) {
		t.Fatal(shares, err)
	}
}
