package update

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/servers"
)

// Docker's classic store records short repository names and the containerd
// store full ones; either matches the registry's answer for the same tag,
// and another repository's digest says nothing about this one.
func TestCompareDigestsSpellings(t *testing.T) {
	remote, old := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	for _, c := range []struct {
		reference string
		digests   []string
		want      string
	}{
		{"itzg/minecraft-server:java21", []string{"itzg/minecraft-server@" + remote}, CheckCurrent},
		{"itzg/minecraft-server:java21", []string{"docker.io/itzg/minecraft-server@" + remote}, CheckCurrent},
		{"docker.io/itzg/minecraft-server", []string{"index.docker.io/itzg/minecraft-server@" + remote}, CheckCurrent},
		{"alpine:3", []string{"docker.io/library/alpine@" + remote}, CheckCurrent},
		{"library/alpine", []string{"alpine@" + remote}, CheckCurrent},
		{"itzg/minecraft-server:java21", []string{"itzg/minecraft-server@" + old}, CheckAvailable},
		// A re-pull that found the same image adds its new digest beside the old.
		{"itzg/minecraft-server:java21", []string{"itzg/minecraft-server@" + old, "itzg/minecraft-server@" + remote}, CheckCurrent},
		{"itzg/minecraft-server:java21", []string{"ghcr.io/itzg/minecraft-server@" + remote, "mirror.example/itzg/minecraft-server@" + old}, CheckUnknown},
		{"itzg/minecraft-server:java21", nil, CheckUnknown},
		{"itzg/minecraft-server:java21", []string{"not-a-digest"}, CheckUnknown},
		{"localhost:5000/game:1.2", []string{"localhost:5000/game@" + old}, CheckAvailable},
		{"localhost:5000/game:1.2", []string{"localhost:5000/game@" + remote}, CheckCurrent},
		{"registry.example:5000/team/game", []string{"registry.example:5000/team/game@" + remote}, CheckCurrent},
	} {
		if got := compareDigests(imageKey(c.reference), c.digests, remote); got != c.want {
			t.Errorf("%s with %v: got %s, want %s", c.reference, c.digests, got, c.want)
		}
	}
}

// Spellings of one tag share a check; different tags or registries don't.
func TestImageKeySpellings(t *testing.T) {
	for reference, want := range map[string]string{
		"alpine":                                "docker.io/library/alpine:latest",
		"library/alpine:latest":                 "docker.io/library/alpine:latest",
		"index.docker.io/library/alpine":        "docker.io/library/alpine:latest",
		"itzg/minecraft-server:java21":          "docker.io/itzg/minecraft-server:java21",
		"ghcr.io/owner/game":                    "ghcr.io/owner/game:latest",
		"localhost:5000/game":                   "localhost:5000/game:latest",
		"localhost/game:1":                      "localhost/game:1",
		"REGISTRY/team/game":                    "registry/team/game:latest",
		"registry.example:5000/team/game:2.0.1": "registry.example:5000/team/game:2.0.1",
	} {
		if got := imageKey(reference); got != want {
			t.Errorf("%s: got %s, want %s", reference, got, want)
		}
	}
}

// A tag that dropped the server's platform has nothing pulling could give it;
// variants are left to Docker, which can pull a compatible one.
func TestOffersPlatform(t *testing.T) {
	both := []docker.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm", Variant: "v7"}}
	for platform, want := range map[string]bool{
		"linux/amd64": true, "linux/arm/v7": true, "linux/arm": true, "linux/arm/v6": true, "linux/arm64": false,
		"windows/amd64": false, "": true,
	} {
		if got := offers(both, platform); got != want {
			t.Errorf("%s: got %v", platform, got)
		}
	}
	if !offers(nil, "linux/arm64") {
		t.Error("an empty platform list ruled a platform out")
	}
}

type checksFixture struct {
	engine  *fakedocker.Engine
	servers *servers.Service
	checks  *Checks
	sent    []string
}

func newChecks(t *testing.T, config string) *checksFixture {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	client := docker.New(engine.Socket)
	f := &checksFixture{engine: engine, servers: &servers.Service{DB: d, Identity: store, Authz: &authz.Service{DB: d, Servers: store},
		Observer: &servers.Observer{Docker: client}}}
	f.checks = f.restart(config)
	return f
}

// restart is a new Checks on the same database, as after Ludock restarts.
func (f *checksFixture) restart(config string) *Checks {
	f.checks = &Checks{Updates: &Service{Docker: f.servers.Observer.Docker, DockerConfig: config}, DB: f.servers.DB,
		Identity: f.servers.Identity, Notify: func(key, message string) { f.sent = append(f.sent, key+" "+message) }}
	return f.checks
}

func (f *checksFixture) run(t *testing.T, now int64) {
	t.Helper()
	current, err := f.servers.RefreshNow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.checks.Run(context.Background(), current, now)
}

func (f *checksFixture) status(t *testing.T, containerID string) *CheckStatus {
	t.Helper()
	logical, err := f.servers.Identity.List(context.Background(), "")
	if err != nil || len(logical) != 1 {
		t.Fatal(logical, err)
	}
	return f.checks.Status(context.Background(), logical[0].ID, containerID)
}

// A registry that fails is not an alert: the server shows the failure, and
// the check is retried later with the saved credentials. A newer version
// found then is announced once, even across a restart, and the next one is
// announced again.
func TestChecksRegistryErrorThenNotifyOnce(t *testing.T) {
	folder := t.TempDir()
	basic := base64.StdEncoding.EncodeToString([]byte("player:registry-secret"))
	os.WriteFile(filepath.Join(folder, "config.json"), []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"`+basic+`"}}}`), 0o600)
	f := newChecks(t, folder)
	const reference = "itzg/minecraft-server:java21"
	id := f.engine.AddContainer(fakedocker.GameOptions{Name: "minecraft", Image: reference, Running: true})
	f.engine.FailRegistry(reference, 500)
	start := time.Now().UnixMilli()

	f.run(t, start)
	if status := f.status(t, id); status == nil || status.State != CheckPending || status.Failed {
		t.Fatalf("first pass should only schedule the check: %+v", status)
	}
	// Images first seen are checked within a quarter hour.
	f.run(t, start+(16*time.Minute).Milliseconds())
	status := f.status(t, id)
	if status == nil || status.State != CheckPending || !status.Failed || status.ConfigProblem || len(f.sent) != 0 {
		t.Fatalf("a failed check should show quietly: %+v %v", status, f.sent)
	}
	if len(f.engine.RegistryAuth) != 1 || f.engine.RegistryAuth[0] == "" {
		t.Fatal("the registry was not asked with the saved credentials:", f.engine.RegistryAuth)
	}

	f.engine.FailRegistry(reference, 0)
	f.engine.PublishImage(reference, "sha256:"+strings.Repeat("3", 64))
	later := start + (3 * time.Hour).Milliseconds()
	f.run(t, later)
	status = f.status(t, id)
	if status == nil || status.State != CheckAvailable || status.Failed || status.CheckedAt != later {
		t.Fatalf("the retry should find the newer image: %+v", status)
	}
	if len(f.sent) != 1 || !strings.HasPrefix(f.sent[0], "image-update:") || !strings.Contains(f.sent[0], "minecraft: a newer version") {
		t.Fatal(f.sent)
	}
	if strings.Contains(f.sent[0], "itzg") || strings.Contains(f.sent[0], "registry-secret") {
		t.Fatal("the notification names the image or its credentials:", f.sent[0])
	}

	f.restart(folder)
	f.run(t, later+(7*time.Hour).Milliseconds())
	if status := f.status(t, id); status == nil || status.State != CheckAvailable || len(f.sent) != 1 {
		t.Fatalf("the same version was announced again: %+v %v", status, f.sent)
	}

	f.engine.PublishImage(reference, "sha256:"+strings.Repeat("4", 64))
	f.run(t, later+(14*time.Hour).Milliseconds())
	if len(f.sent) != 2 || f.sent[0] == f.sent[1] {
		t.Fatal("a newer version was not announced:", f.sent)
	}
}

// Images pinned by digest get no check at all.
func TestChecksSkipPinnedImages(t *testing.T) {
	f := newChecks(t, "")
	id := f.engine.AddContainer(fakedocker.GameOptions{Name: "pinned", Image: "itzg/minecraft-server@sha256:" + strings.Repeat("5", 64)})
	f.run(t, 0)
	f.run(t, (time.Hour).Milliseconds())
	if status := f.status(t, id); status != nil || len(f.engine.RegistryAuth) != 0 {
		t.Fatalf("a pinned image was checked: %+v %v", status, f.engine.RegistryAuth)
	}
}

// After Update server pulls a version newer than the one last seen, the
// server isn't reported out of date against the stale answer: the tag is
// asked about again on the next pass.
func TestChecksForgetAsksAgain(t *testing.T) {
	f := newChecks(t, "")
	const reference = "itzg/minecraft-server:java21"
	id := f.engine.AddContainer(fakedocker.GameOptions{Name: "minecraft", Image: reference, Running: true})
	f.engine.PublishImage(reference, "sha256:"+strings.Repeat("3", 64))
	f.run(t, 0)
	f.run(t, (16 * time.Minute).Milliseconds())
	if status := f.status(t, id); status == nil || status.State != CheckAvailable || len(f.sent) != 1 {
		t.Fatalf("%+v %v", status, f.sent)
	}
	// The tag moves on, and the server is updated to the newest version.
	newest := "sha256:" + strings.Repeat("4", 64)
	f.engine.PublishImage(reference, newest)
	f.engine.Update(id, func(c *fakedocker.Container) { c.Image = newest })
	logical, _ := f.servers.Identity.List(context.Background(), "")
	f.checks.Forget(logical[0].ID)
	f.run(t, (20 * time.Minute).Milliseconds())
	if status := f.status(t, id); status == nil || status.State != CheckCurrent || len(f.sent) != 1 {
		t.Fatalf("compared with the stale answer: %+v %v", status, f.sent)
	}
}
