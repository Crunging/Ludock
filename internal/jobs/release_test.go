package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
)

// testdata/1.0 holds what a 1.0 install wrote: ludock serve (7b53154) against
// fakedocker -demo, data in /var/tmp/ludock-1.0, set up by fixture-admin,
// "Friends’ survival world" shared with fixture-friend as Can run it, its
// default daily restart scheduled in UTC, one backup of it taken, then the
// next save of Ludock's own data. backup.json is the archive's id, size,
// and checksum and what the data folder held, read on that install.
//
// Never regenerate these to make this test pass. A failure means the change
// breaks 1.0 installs: it needs a migration, or a reader that still accepts
// the old form.
func TestReadsDataWrittenBy10(t *testing.T) {
	var made struct {
		ID, Checksum, DataFolder string
		Size                     int64
		// Files maps each path to its content's SHA-256; folders end in /.
		Files map[string]string
	}
	encoded, err := os.ReadFile("testdata/1.0/backup.json")
	if err == nil {
		err = json.Unmarshal(encoded, &made)
	}
	if err != nil {
		t.Fatal(err)
	}

	// The save restores as `ludock restore-data` does and opens as Ludock
	// starts.
	path := filepath.Join(t.TempDir(), "data", "ludock.db")
	if _, err := db.RestoreSnapshot("testdata/1.0/ludock-data-20261124-190205.tar", path); err != nil {
		t.Fatal("restore-data:", err)
	}
	d, err := db.Open(path)
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(func() { d.Close() })
	users, err := d.ListUsers(ctx)
	if err != nil || len(users) != 2 || users[0].Username != "fixture-admin" || users[0].Role != db.RoleAdmin ||
		users[1].Username != "fixture-friend" || users[1].Role != db.RoleMember {
		t.Fatalf("accounts %+v %v", users, err)
	}
	signIn := &auth.Service{DB: d, Hasher: auth.Argon2{}}
	for username, password := range map[string]string{"fixture-admin": "throwaway fixture password", "fixture-friend": "throwaway friend password"} {
		if user, err := signIn.Authenticate(ctx, username, password); err != nil || user == nil {
			t.Fatalf("%s can't sign in: %v", username, err)
		}
	}
	admin := &db.SessionUser{ID: users[0].ID, Username: users[0].Username, Role: users[0].Role}
	store := &identity.Store{DB: d}
	authorization := &authz.Service{DB: d, Servers: store}
	views, err := (&servers.Service{DB: d, Identity: store, Authz: authorization}).ListSaved(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var server *servers.View
	for index := range views {
		if views[index].DisplayName == "Friends’ survival world" {
			server = &views[index]
		}
	}
	if server == nil || server.GameType != "minecraft" || server.SharingPaused {
		t.Fatalf("servers %+v", views)
	}
	if shares, err := authorization.Sharing(ctx, server.ID); err != nil || len(shares) != 1 ||
		shares[0].Username != "fixture-friend" || shares[0].Level != authz.LevelRun {
		t.Fatalf("sharing %+v %v", shares, err)
	}
	saved, err := (&schedules.Service{DB: d, Authz: authorization, Identity: store}).List(ctx, admin, server.ID)
	if err != nil || len(saved) != 1 || saved[0].Kind != "recurring" || !slices.Equal(slices.Sorted(slices.Values(saved[0].Days)), []int{0, 1, 2, 3, 4, 5, 6}) || saved[0].Action != "restart" || saved[0].Time != "08:00" || saved[0].Timezone != "UTC" ||
		!saved[0].CreateBackup || !saved[0].Enabled || saved[0].OwnerName != "fixture-admin" {
		t.Fatalf("schedules %+v %v", saved, err)
	}
	records, err := (&backups.Service{DB: d}).List(ctx, server.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("backups %+v %v", records, err)
	}
	record := records[0]
	if record.ID != made.ID || record.Size != made.Size || record.Checksum != made.Checksum || record.State != "complete" ||
		record.GameType != "minecraft" || len(record.Roots) != 1 || record.Roots[0].Path != made.DataFolder {
		t.Fatalf("backup %+v", record)
	}

	// The archive, under its 1.0 record, restores onto this package's fake
	// server, whose data folder has the same path inside it but is kept
	// elsewhere on the host.
	f := setup(t)
	archive, err := os.ReadFile("testdata/1.0/" + made.ID + ".tar")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.backupTo, made.ID+".tar"), archive, 0o600); err != nil {
		t.Fatal(err)
	}
	f.engine.Update(f.game, func(c *fakedocker.Container) { c.Mounts[0].Destination = made.DataFolder })
	f.game = f.engine.Recreate(f.game, url.Values{})
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	record.ServerID = f.serverID
	if err := f.jobs.Backups.Record(ctx, record, "1.0", f.backupTo); err != nil {
		t.Fatal(err)
	}
	if job := f.run(t, "restore", map[string]any{"backupId": made.ID, "confirmation": "mc", MovedInput: true}); job.Status != ops.Succeeded {
		t.Fatalf("restore %+v", job)
	}
	restored := map[string]string{}
	err = filepath.WalkDir(f.data, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == f.data {
			return err
		}
		name, _ := filepath.Rel(f.data, path)
		if entry.IsDir() {
			restored[name+"/"] = ""
			return nil
		}
		content, err := os.ReadFile(path)
		sum := sha256.Sum256(content)
		restored[name] = hex.EncodeToString(sum[:])
		return err
	})
	if err != nil || !maps.Equal(restored, made.Files) {
		t.Fatalf("restored %v %v\nwant %v", restored, err, made.Files)
	}
}
