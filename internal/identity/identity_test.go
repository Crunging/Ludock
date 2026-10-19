package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

func observation(change func(*Observation)) Observation {
	o := Observation{ContainerID: "docker-a", Name: "minecraft-1", DisplayName: "Minecraft", GameType: "minecraft",
		Mounts:            []Mount{{Type: "bind", Source: "/srv/minecraft", Destination: "/data", Writable: true}},
		GameConfiguration: map[string]string{"ludock.console": "minecraft-rcon"}}
	if change != nil {
		change(&o)
	}
	return o
}

func reconcile(t *testing.T, s *Store, now int64, observations ...Observation) []Server {
	t.Helper()
	servers, err := s.Reconcile(ctx, observations, "", now)
	if err != nil {
		t.Fatal(err)
	}
	return servers
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apperr.Is(err, code) {
		t.Fatalf("err = %v, want %s", err, code)
	}
}

func TestBindingDigestCoversMaterialSettingsOnly(t *testing.T) {
	base := BindingDigest(observation(nil))
	same := []func(*Observation){
		func(o *Observation) { o.ContainerID, o.Name, o.DisplayName = "docker-b", "renamed", "Renamed" },
		func(o *Observation) { o.Mounts[0].Source = "/srv//minecraft/" },
		func(o *Observation) {
			o.Mounts = append(o.Mounts, Mount{Type: "bind", Source: "/srv/mods", Destination: "/mods"})
		},
	}
	for index, change := range same {
		if BindingDigest(observation(change)) != base {
			t.Errorf("change %d altered the digest", index)
		}
	}
	two := func(first, second Mount) func(*Observation) {
		return func(o *Observation) { o.Mounts = []Mount{first, second} }
	}
	a := Mount{Type: "bind", Source: "/srv/a", Destination: "/a", Writable: true}
	b := Mount{Type: "volume", Source: "/var/lib/docker/volumes/b/_data", Name: "b", Destination: "/b", Writable: true}
	if BindingDigest(observation(two(a, b))) != BindingDigest(observation(two(b, a))) {
		t.Error("mount order altered the digest")
	}
	different := []func(*Observation){
		func(o *Observation) { o.GameType = "valheim" },
		func(o *Observation) { o.Mounts[0].Source = "/srv/other" },
		func(o *Observation) { o.Mounts[0].Destination = "/world" },
		func(o *Observation) { o.GameConfiguration["ludock.console"] = "attach" },
		func(o *Observation) { o.GameConfiguration = nil },
	}
	for index, change := range different {
		if BindingDigest(observation(change)) == base {
			t.Errorf("change %d kept the digest", index)
		}
	}
}

func TestExternalIdentityEscapesSeparators(t *testing.T) {
	compose, _ := ExternalIdentity(observation(func(o *Observation) {
		o.Compose = &ComposeIdentity{Project: "a:b", Service: "c", ContainerNumber: "1"}
	}))
	other, _ := ExternalIdentity(observation(func(o *Observation) {
		o.Compose = &ComposeIdentity{Project: "a", Service: "b:c", ContainerNumber: "1"}
	}))
	standalone, _ := ExternalIdentity(observation(func(o *Observation) { o.Name = "/games stack" }))
	if compose == other || compose != "compose:a%3Ab:c:1" || standalone != "standalone:games+stack" {
		t.Fatal(compose, other, standalone)
	}
}

func TestRecreationKeepsUUID(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 100, observation(nil))[0]
	recreated := reconcile(t, s, 200, observation(func(o *Observation) { o.ContainerID = "docker-b" }))[0]
	if recreated.ID != original.ID || recreated.Status != StatusActive || recreated.BindingRevision != original.BindingRevision+1 ||
		recreated.ContainerID != "docker-b" || recreated.FirstSeenAt != 100 || recreated.LastSeenAt != 200 {
		t.Fatalf("%+v", recreated)
	}
	_, err := s.ResolveBinding(ctx, recreated.ID, original.BindingRevision)
	wantCode(t, err, "SERVER_BINDING_CHANGED")
	rows, _ := s.DB.Query("SELECT container_id FROM server_bindings ORDER BY id")
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if strings.Join(ids, ",") != "docker-a,docker-b" {
		t.Fatal(ids)
	}
}

func TestUnchangedBindingsRefreshCoarsely(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 100, observation(nil))[0]
	reconcile(t, s, 100, observation(func(o *Observation) { o.ContainerID = "docker-z"; o.Name = "gone" }))
	reconcile(t, s, 100, observation(nil))
	reconcile(t, s, 59_000, observation(nil))
	server, _ := s.Get(ctx, original.ID)
	if server.LastSeenAt != 100 {
		t.Fatal("rewrote an unchanged binding")
	}
	reconcile(t, s, 60_100, observation(nil))
	server, _ = s.Get(ctx, original.ID)
	if server.LastSeenAt != 60_100 || server.Status != StatusActive {
		t.Fatalf("%+v", server)
	}
}

func TestStandaloneRenameIsNewServer(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 0, observation(nil))[0]
	result := reconcile(t, s, 0, observation(func(o *Observation) { o.Name = "different-name" }))
	if len(result) != 2 {
		t.Fatal(len(result))
	}
	old, _ := s.Get(ctx, original.ID)
	if old.Status != StatusMissing || old.ContainerID != "" {
		t.Fatalf("%+v", old)
	}
	_, err := s.ResolveBinding(ctx, original.ID, -1)
	wantCode(t, err, "SERVER_BINDING_UNAVAILABLE")
}

func TestDuplicateComposeIdentityIsAmbiguous(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	compose := &ComposeIdentity{"p", "s", "1"}
	original := reconcile(t, s, 0, observation(func(o *Observation) { o.Compose = compose }))[0]
	duplicate := reconcile(t, s, 0, observation(func(o *Observation) { o.Compose = compose }),
		observation(func(o *Observation) { o.Compose = compose; o.ContainerID = "docker-b"; o.Name = "duplicate" }))[0]
	if duplicate.ID != original.ID || duplicate.Status != StatusAmbiguous || duplicate.ContainerID != "" {
		t.Fatalf("%+v", duplicate)
	}
	var count int
	s.DB.QueryRow("SELECT COUNT(*) FROM server_bindings").Scan(&count)
	if count != 1 {
		t.Fatal(count)
	}
	recovered := reconcile(t, s, 0, observation(func(o *Observation) { o.Compose = compose }))[0]
	if recovered.ID != original.ID || recovered.Status != StatusActive {
		t.Fatalf("%+v", recovered)
	}
}

func TestMaterialChangesNeedReview(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 0, observation(nil))[0]
	changed := observation(func(o *Observation) {
		o.Mounts = []Mount{{Type: "bind", Source: "/srv/unrelated-world", Destination: "/data", Writable: true}}
	})
	pending := reconcile(t, s, 0, changed)[0]
	if pending.ID != original.ID || pending.Status != StatusReviewRequired || pending.BindingFingerprint != original.BindingFingerprint ||
		pending.PendingFingerprint == original.BindingFingerprint {
		t.Fatalf("%+v", pending)
	}
	_, err := s.ResolveBinding(ctx, pending.ID, -1)
	wantCode(t, err, "SERVER_BINDING_UNAVAILABLE")
	_, err = s.ReviewBinding(ctx, pending.ID, "stale-fingerprint", -1)
	wantCode(t, err, "SERVER_REVIEW_CHANGED")
	reviewed, err := s.ReviewBinding(ctx, pending.ID, pending.PendingFingerprint, -1)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := s.BindingFingerprint(changed)
	if reviewed.Status != StatusActive || reviewed.BindingFingerprint != want || reviewed.BindingRevision <= pending.BindingRevision {
		t.Fatalf("%+v", reviewed)
	}
	for _, candidate := range []Observation{
		observation(func(o *Observation) { o.Mounts = changed.Mounts; o.GameType = "factorio" }),
		observation(func(o *Observation) {
			o.Mounts = changed.Mounts
			o.GameConfiguration = map[string]string{"ludock.console": "different"}
		}),
	} {
		if fingerprint, _ := s.BindingFingerprint(candidate); fingerprint == reviewed.BindingFingerprint {
			t.Fatal("material change kept fingerprint")
		}
	}
	// Read-only mounts are not material.
	readOnly := observation(func(o *Observation) {
		o.Mounts = append(o.Mounts, Mount{Type: "bind", Source: "/etc", Destination: "/etc", Writable: false})
	})
	if BindingDigest(readOnly) != BindingDigest(observation(nil)) {
		t.Fatal("read-only mount changed digest")
	}
}

func TestReviewSurvivesDisappearanceAndReversion(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 0, observation(nil))[0]
	reconcile(t, s, 0, observation(func(o *Observation) { o.GameType = "factorio" }))
	reconcile(t, s, 0)
	reappeared := reconcile(t, s, 0, observation(nil))[0]
	if reappeared.ID != original.ID || reappeared.Status != StatusReviewRequired || !reappeared.ReviewRequired {
		t.Fatalf("%+v", reappeared)
	}
}

func TestObservedBindingRejectsReplacement(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	server := reconcile(t, s, 0, observation(nil))[0]
	if _, err := s.AssertObservedBinding(ctx, server.ID, observation(nil), server.BindingRevision); err != nil {
		t.Fatal(err)
	}
	for _, o := range []Observation{
		observation(func(o *Observation) { o.ContainerID = "external-new" }),
		observation(func(o *Observation) { o.GameType = "factorio" }),
		observation(func(o *Observation) { o.Name = "other" }),
	} {
		_, err := s.AssertObservedBinding(ctx, server.ID, o, -1)
		wantCode(t, err, "SERVER_BINDING_CHANGED")
	}
}

func TestRejectsInvalidSnapshots(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	original := reconcile(t, s, 0, observation(nil))[0]
	_, err := s.Reconcile(ctx, []Observation{observation(nil), observation(nil)}, "", 0)
	wantCode(t, err, "INVALID_SERVER_SNAPSHOT")
	_, err = s.Reconcile(ctx, []Observation{observation(func(o *Observation) { o.Compose = &ComposeIdentity{"games", "", "1"} })}, "", 0)
	if err == nil || !strings.Contains(err.Error(), "Incomplete Compose") {
		t.Fatal(err)
	}
	server, _ := s.Get(ctx, original.ID)
	if server.Status != StatusActive {
		t.Fatal(server.Status)
	}
	a, _ := ExternalIdentity(observation(func(o *Observation) { o.Compose = &ComposeIdentity{"a:b", "c", "1"} }))
	b, _ := ExternalIdentity(observation(func(o *Observation) { o.Compose = &ComposeIdentity{"a", "b:c", "1"} }))
	if a == b {
		t.Fatal("ambiguous encoding")
	}
}
