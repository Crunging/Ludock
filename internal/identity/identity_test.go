package identity

import (
	"context"
	"errors"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
)

var ctx = context.Background()

func observation(change func(*Observation)) Observation {
	o := Observation{ContainerID: "docker-a", Name: "minecraft-1", DisplayName: "Minecraft", GameType: "minecraft",
		Mounts:      []Mount{{Type: "bind", Source: "/srv/minecraft", Destination: "/data", Writable: true}},
		DataFolders: []DataFolder{{Path: "/data", Type: "bind", Source: "/srv/minecraft", Destination: "/data"}}}
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

// Only the game and the data folders Ludock reads, backs up, and restores
// make a server different in substance; mounts outside them and every other
// setting change freely.
func TestBindingDigestScope(t *testing.T) {
	base := BindingDigest(observation(nil))
	same := []func(*Observation){
		func(o *Observation) { o.ContainerID, o.Name, o.DisplayName = "docker-b", "renamed", "Renamed" },
		func(o *Observation) { o.DataFolders[0].Source = "/srv//minecraft/" },
		func(o *Observation) {
			o.Mounts = append(o.Mounts, Mount{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Name: "x", Destination: "/cache", Writable: true})
		},
		func(o *Observation) { o.Compose = &ComposeIdentity{"p", "s", "1"} },
		func(o *Observation) { o.Secrets = []string{"rotated-password"} },
	}
	for index, change := range same {
		if BindingDigest(observation(change)) != base {
			t.Errorf("change %d altered the digest", index)
		}
	}
	two := func(first, second DataFolder) func(*Observation) {
		return func(o *Observation) { o.DataFolders = []DataFolder{first, second} }
	}
	a := DataFolder{Path: "/a", Type: "bind", Source: "/srv/a", Destination: "/a"}
	b := DataFolder{Path: "/b/world", Type: "volume", Source: "b", Destination: "/b"}
	if BindingDigest(observation(two(a, b))) != BindingDigest(observation(two(b, a))) {
		t.Error("folder order altered the digest")
	}
	different := []func(*Observation){
		func(o *Observation) { o.GameType = "valheim" },
		func(o *Observation) { o.DataFolders[0].Source = "/srv/other" },
		func(o *Observation) { o.DataFolders[0].Path = "/data/world" },
		func(o *Observation) { o.DataFolders[0].Destination = "/" },
		func(o *Observation) { o.DataFolders[0].Type = "volume" },
		func(o *Observation) { o.DataFolders = append(o.DataFolders, b) },
		func(o *Observation) { o.DataFolders = nil },
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

func addUser(t *testing.T, s *Store, id string, role db.Role) {
	t.Helper()
	if err := s.DB.CreateUser(ctx, db.UserRecord{User: db.User{ID: id, Username: id, Role: role, CreatedAt: 1}, PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}
}

func share(t *testing.T, s *Store, userID, serverID string) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO server_grants(user_id, server_id, capabilities_json, updated_at) VALUES (?, ?, '["server.view"]', 1)`,
		userID, serverID); err != nil {
		t.Fatal(err)
	}
}

// A new game or other data folders take effect at once. They pause sharing
// only when someone other than an administrator has access, and an
// administrator confirms the server as it is now to resume it.
func TestMaterialChangePausesSharing(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	addUser(t, s, "admin", db.RoleAdmin)
	addUser(t, s, "other-admin", db.RoleAdmin)
	addUser(t, s, "friend", db.RoleOperator)
	session := dbtest.Session(t, s.DB, "admin")
	original := reconcile(t, s, 0, observation(nil))[0]
	share(t, s, "other-admin", original.ID)
	moved := func(o *Observation) {
		o.DataFolders = []DataFolder{{Path: "/data", Type: "bind", Source: "/srv/unrelated-world", Destination: "/data"}}
	}
	unshared := reconcile(t, s, 0, observation(moved))[0]
	want, _ := s.BindingFingerprint(observation(moved))
	if unshared.SharingPaused || unshared.BindingFingerprint != want || unshared.BindingRevision != original.BindingRevision+1 {
		t.Fatalf("%+v", unshared)
	}
	share(t, s, "friend", original.ID)
	paused := reconcile(t, s, 0, observation(func(o *Observation) { moved(o); o.GameType = "factorio" }))[0]
	if !paused.SharingPaused || paused.Status != StatusActive || paused.GameType != "factorio" || paused.BindingRevision != unshared.BindingRevision+1 {
		t.Fatalf("%+v", paused)
	}
	if _, err := s.ResolveBinding(ctx, paused.ID, paused.BindingRevision); err != nil {
		t.Fatal("administrators lost the server:", err)
	}
	// Recreating the container without another material change keeps it paused.
	recreated := reconcile(t, s, 0, observation(func(o *Observation) { moved(o); o.GameType = "factorio"; o.ContainerID = "docker-b" }))[0]
	if !recreated.SharingPaused {
		t.Fatalf("%+v", recreated)
	}
	wantCode(t, s.ResumeSharing(ctx, "admin", session, paused.ID, paused.BindingRevision), "SERVER_CHANGED_AGAIN")
	if err := s.ResumeSharing(ctx, "admin", "revoked-session", paused.ID, recreated.BindingRevision); !errors.Is(err, db.ErrSessionGone) {
		t.Fatal(err)
	}
	if err := s.ResumeSharing(ctx, "admin", session, paused.ID, recreated.BindingRevision); err != nil {
		t.Fatal(err)
	}
	resumed, _ := s.Get(ctx, paused.ID)
	if resumed.SharingPaused || resumed.BindingRevision != recreated.BindingRevision {
		t.Fatalf("%+v", resumed)
	}
}

// A pause survives the container disappearing and coming back as it was
// before the change: nobody confirmed either version since.
func TestPauseSurvivesDisappearance(t *testing.T) {
	s := &Store{DB: dbtest.Open(t)}
	addUser(t, s, "friend", db.RoleViewer)
	original := reconcile(t, s, 0, observation(nil))[0]
	share(t, s, "friend", original.ID)
	reconcile(t, s, 0, observation(func(o *Observation) { o.GameType = "factorio" }))
	reconcile(t, s, 0)
	reappeared := reconcile(t, s, 0, observation(nil))[0]
	if reappeared.ID != original.ID || reappeared.Status != StatusActive || !reappeared.SharingPaused {
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

// The ID checks agree with the patterns they replaced, at the boundaries
// and on random strings over the characters that matter.
func TestIDChecksMatchPatterns(t *testing.T) {
	logical := regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	container := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	cases := []string{"", "a", "-", ".a", "_a", "a-", "a.b_c-d", strings.Repeat("a", 128), strings.Repeat("a", 129),
		"a b", "a\n", "é", "aé", NewUUID(), strings.ToUpper(NewUUID()), NewUUID() + "\n", NewUUID()[1:], NewUUID() + "0",
		"0123456789abcdef0123456789abcdef0123", "01234567-89ab-cdef-0123-456789abcdeg", "01234567_89ab-cdef-0123-456789abcdef",
		"0123456789-ab-cdef-0123-456789abcdef", "01234567-89ab-cdef-0123-456789abcdef"}
	random := rand.New(rand.NewPCG(1, 2))
	alphabet := "09afAFgzGZ-_.\n é"
	for range 20000 {
		length := random.IntN(40)
		if random.IntN(4) == 0 {
			length = 120 + random.IntN(12)
		}
		var value strings.Builder
		for range length {
			value.WriteByte(alphabet[random.IntN(len(alphabet))])
		}
		cases = append(cases, value.String())
	}
	for range 2000 {
		value := []byte(NewUUID())
		value[random.IntN(len(value))] = alphabet[random.IntN(len(alphabet))]
		cases = append(cases, string(value))
	}
	for _, value := range cases {
		if ValidID(value) != logical.MatchString(value) {
			t.Errorf("ValidID(%q) = %v", value, ValidID(value))
		}
		if ValidContainerID(value) != container.MatchString(value) {
			t.Errorf("ValidContainerID(%q) = %v", value, ValidContainerID(value))
		}
	}
}
