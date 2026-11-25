package jobs

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/notify"
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

// testdata/1.0/interrupted holds what 1.0 installs left when killed partway
// through work, one folder each; its README says how they were made. Each
// is started again as Ludock starts, and the owner sees where the data is,
// whether the server runs, and what Activity says. In each, the game's data
// was "as backed up" until its backup, and "as it is now" since.
//
// Never regenerate these to make this test pass. A failure means the change
// breaks recovery of a 1.0 install that was upgraded partway through work:
// it needs a reader that still accepts what 1.0 saved.
func TestRecoversWorkInterruptedOn10(t *testing.T) {
	backedUp := map[string]string{"server.properties": "motd=as backed up\n", "world/": "", "world/level.dat": "the world as backed up"}
	now := map[string]string{"server.properties": "motd=as it is now\n", "world/": "", "world/level.dat": "the world as it is now",
		"world/since.dat": "made since the backup"}
	// After an undo, what the game wrote while started elsewhere is set aside.
	setAside := map[string]string{"{aside}/": "", "{aside}/server.properties": "motd=as backed up\n", "{aside}/world/": "",
		"{aside}/world/level.dat": "the world as the game saved it later"}
	restoreAlert := "mc: Ludock restarted during a restore. Its Activity says what the data was left as."
	backupAlert := "mc: Ludock restarted during a backup, so there’s no new backup. Its Activity says more."
	undone := "The restore was undone: the server’s data is as it was. Ludock started the server again."
	finishedUndo := "Ludock finished undoing a restore that was stopped partway: the server’s previous data is back in place. " +
		"Anything Ludock moved out of its way went to the folder {aside} beside it."
	interrupted := "Ludock restarted while this ran. Check the server before trying again."
	for _, c := range []struct {
		name string
		// status, err, and guidance are the interrupted operation's outcome,
		// or, with none, the restore's; err defaults to interrupted.
		status, err, guidance string
		// pending, for an undo, is what the restore it finished says.
		pending string
		data    map[string]string
		// containers are each one's name, state, restart policy, and image.
		containers []string
		// sameRun: the server runs the run it had, and no start was sent.
		sameRun bool
		// stopConfirmed: recovery saw the stop it had sent end.
		stopConfirmed bool
		alerts        []string
	}{
		// Staging: only the stage folder changed, which is removed.
		{name: "restore-staging", status: ops.Interrupted, guidance: "Nothing was restored: the server’s data is as it was. Ludock started the server again.",
			data: now, containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// Moving the old data out, which had all moved: it is put back.
		{name: "restore-moving-old", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// Old data moved, nothing in its place yet.
		{name: "restore-old-moved", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// The backup's data in place, not committed: it is removed and the
		// old data put back.
		{name: "restore-replaced", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// A failed swap rolling back, its new data removed and the old data
		// still in the stage.
		{name: "restore-rolling-back", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// Rolled back, its stage folder not yet removed.
		{name: "restore-rolled-back", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// Committed, its stage folder with the replaced data not yet removed:
		// the restore stands.
		{name: "restore-committed", status: ops.Interrupted, guidance: "The restore had finished: what it restored is in place. Ludock started the server again.",
			data: backedUp, containers: []string{"/mc running unless-stopped 0000"}},
		// One folder's restore, the folder moved into the stage.
		{name: "item-old-moved", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// One folder's restore, the backup's copy in its place.
		{name: "item-replaced", status: ops.Interrupted, guidance: undone, data: now,
			containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{restoreAlert}},
		// An undo of a restore that failed because the server was started
		// elsewhere, killed once it set aside what the game wrote meanwhile
		// and before it journaled that it kept anything.
		{name: "undo-set-aside", status: ops.Interrupted, guidance: "The server was stopped before this began, so Ludock didn’t start it.",
			pending: finishedUndo, data: merged(now, setAside), containers: []string{"/mc exited unless-stopped 0000"}},
		// The same undo killed later, as it put the previous data back, with
		// kept journaled.
		{name: "undo-set-aside-kept", status: ops.Interrupted, guidance: "The server was stopped before this began, so Ludock didn’t start it.",
			pending: finishedUndo, data: merged(now, setAside), containers: []string{"/mc exited unless-stopped 0000"}},
		// A backup written but not published: the partial archive goes.
		{name: "backup-copying", status: ops.Interrupted,
			guidance: "Ludock restarted before the backup finished, so there’s no new backup. Ludock started the server again.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{backupAlert}},
		// The stop sent and done, but not yet confirmed.
		{name: "backup-stop-unconfirmed", status: ops.Interrupted,
			guidance: "Ludock restarted before the backup finished, so there’s no new backup. Ludock started the server again.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 0000"}, stopConfirmed: true, alerts: []string{backupAlert}},
		// The backup recorded and the start sent: it isn't sent again.
		{name: "backup-start-sent", status: ops.Interrupted, guidance: "The backup had finished. Ludock started the server again.",
			data: backedUp, containers: []string{"/mc running unless-stopped 0000"}, sameRun: true},
		// Players warned, the backup waiting: it resumes, long past the time
		// players were given, so it doesn't run.
		{name: "backup-warning", status: ops.Failed,
			err:  "Ludock wasn’t running when the server was due to restart for a backup, so it didn’t. Players had been told it would.",
			data: backedUp, containers: []string{"/mc running unless-stopped 0000"}},
		// A restart whose stop was sent and done but not confirmed: recovery
		// only confirms the stop.
		{name: "restart-stop-unconfirmed", status: ops.Interrupted, data: backedUp, containers: []string{"/mc exited unless-stopped 0000"},
			stopConfirmed: true},
		// A restart with a backup, its archive written but not published:
		// the partial archive goes and the server runs again.
		{name: "restart-backup-copying", status: ops.Interrupted,
			guidance: "Ludock restarted before the backup finished, so there’s no new backup. The server was started again.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 0000"}, alerts: []string{backupAlert}},
		// A restore that failed because the server was started elsewhere,
		// with its new data in place, and the server stopped since: startup
		// undoes it, setting aside what the game wrote.
		{name: "restore-failed-pending", status: ops.Failed,
			err:      "Something outside Ludock started or changed the server meanwhile. Check it before trying again.",
			guidance: finishedUndo, data: merged(now, setAside), containers: []string{"/mc exited unless-stopped 0000"}},
		// The replacement created under its temporary name, its ID not yet
		// saved: found by its name and label, removed.
		{name: "update-created", status: ops.Interrupted,
			guidance: "Ludock restarted during the update, so the update didn’t happen. The server was started again as it was.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 0000"}},
		// The replacement started but not yet verified: removed, and the
		// original kept stopped.
		{name: "update-started", status: ops.Interrupted,
			guidance: "Ludock restarted while it checked the updated container, so the update was undone and the previous container kept. " +
				"The server was left stopped, since the new version may already have run and changed the game data. Check the server, then start it.",
			data: backedUp, containers: []string{"/mc exited unless-stopped 0000"},
			alerts: []string{"mc: the update did not finish after its new container started, which may have changed the game data, " +
				"so the previous container was restored and left stopped. Check its Activity, then start it."}},
		// The original removed, the verified replacement still under its
		// temporary name: the update is finished.
		{name: "update-original-removed", status: ops.Interrupted,
			guidance: "Ludock restarted while the updated container replaced the previous one, so it finished the update.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 1111"}, sameRun: true},
		// The replacement renamed and given the original's policy, the swap
		// saved as replaced, the server's state not yet confirmed.
		{name: "update-replaced", status: ops.Interrupted,
			guidance: "Ludock restarted while the updated container replaced the previous one, so it finished the update.",
			data:     backedUp, containers: []string{"/mc running unless-stopped 1111"}, sameRun: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			i := interrupted10(t, c.name)
			var restore string
			if err := i.jobs.DB.QueryRow("SELECT id FROM operations WHERE kind='restore'").Scan(&restore); err != nil && !errors.Is(err, sql.ErrNoRows) {
				t.Fatal(err)
			}
			id := i.interrupted
			if id == "" {
				id = restore
			}
			job := finished(t, i.runner, id)
			named := strings.NewReplacer("{aside}", helper.SetAsideName(restore))
			want := c.err
			if want == "" && c.status == ops.Interrupted {
				want = interrupted
			}
			if job.Status != c.status || job.Error != want || job.Result.Guidance != named.Replace(c.guidance) || job.Result.UndoNote != named.Replace(c.pending) {
				t.Fatalf("%s %q %q, undo %q\nwant %s %q %q, undo %q", job.Status, job.Error, job.Result.Guidance, job.Result.UndoNote,
					c.status, want, named.Replace(c.guidance), named.Replace(c.pending))
			}
			if c.stopConfirmed && (!job.Recovery.StopSettled || job.Recovery.StopRecoveryTimedOut) {
				t.Fatalf("stop not confirmed: %+v", job.Recovery)
			}
			if c.pending != "" {
				if pending, err := i.runner.Get(ctx, restore); err != nil || pending.Result.Guidance != named.Replace(c.pending) {
					t.Fatalf("restore %+v %v", pending, err)
				}
			}
			data := map[string]string{}
			for name, content := range c.data {
				data[named.Replace(name)] = content
			}
			if found := tree(t, i.host+i.install+"/data/mc"); !maps.Equal(found, data) {
				t.Fatalf("data %v\nwant %v", found, data)
			}
			if containers := i.containers(); !slices.Equal(containers, c.containers) {
				t.Fatalf("containers %v, want %v", containers, c.containers)
			}
			if c.sameRun && (i.started() != i.savedStart(t) || i.startSent()) {
				t.Fatal("the server was started again")
			}
			// The backups 1.0 had stay as they were, recorded; an unpublished
			// one is gone, and none is new.
			archives := map[string]string{}
			for name, content := range tree(t, i.host+i.install+"/backups") {
				sum := sha256.Sum256([]byte(content))
				archives[name] = hex.EncodeToString(sum[:])
			}
			var records []string
			rows, err := i.jobs.DB.Query("SELECT id FROM backups WHERE state='complete' ORDER BY id")
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var id string
				rows.Scan(&id)
				records = append(records, id+".tar")
			}
			rows.Close()
			if !maps.Equal(archives, i.archives) || !slices.Equal(records, slices.Sorted(maps.Keys(i.archives))) ||
				len(tree(t, i.host+i.install+"/backups-now")) != 0 {
				t.Fatalf("backups %v, recorded %v\nwant %v", archives, records, i.archives)
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if !slices.Equal(i.alerts, c.alerts) {
				t.Fatalf("alerts %q, want %q", i.alerts, c.alerts)
			}
		})
	}
}

func merged(a, b map[string]string) map[string]string {
	out := maps.Clone(a)
	maps.Copy(out, b)
	return out
}

// installed10 is a 1.0 install that was killed partway through some work:
// its data folder, its host's files, and Docker's containers, as
// testdata/1.0/interrupted/<name> holds them.
type installed10 struct {
	engine *fakedocker.Engine
	jobs   *Jobs
	runner *ops.Runner
	// host is the host's root: what the install kept at a path p is at host+p.
	host, install string
	// interrupted is the operation that was running when it was killed, if
	// any.
	interrupted string
	// archives are the backup folder's archives as it was killed, by name,
	// with their content's SHA-256.
	archives map[string]string
	mu       sync.Mutex
	alerts   []string
}

// interrupted10 is the install as it starts again, once recovery ran.
func interrupted10(t *testing.T, name string) *installed10 {
	t.Helper()
	i := &installed10{host: t.TempDir(), install: "/var/tmp/ludock-1.0-interrupted/" + name}
	fixture := filepath.Join("testdata/1.0/interrupted", name)
	unpack(t, filepath.Join(fixture, "host.tar.gz"), i.host+i.install)
	data := filepath.Join(i.host, "ludock")
	unpack(t, filepath.Join(fixture, "data.tar.gz"), data)
	d, err := db.Open(filepath.Join(data, "ludock.db"))
	if err != nil {
		t.Fatal("open:", err)
	}
	t.Cleanup(func() { d.Close() })
	i.archives = map[string]string{}
	for name, content := range tree(t, i.host+i.install+"/backups") {
		if strings.HasSuffix(name, ".tar") {
			sum := sha256.Sum256([]byte(content))
			i.archives[name] = hex.EncodeToString(sum[:])
		}
	}
	// Ludock opens its own backup folder itself, so it moves with the host.
	// Its owner has since chosen another, so recovery must use the folder
	// each operation saved.
	backupTo, chosen := i.host+i.install+"/backups", i.host+i.install+"/backups-now"
	if err := os.Mkdir(chosen, 0o755); err != nil {
		t.Fatal(err)
	}
	for statement, to := range map[string]string{
		`UPDATE settings SET value_json=replace(value_json,'"'||?1||'"','"'||?2||'"')`:         chosen,
		`UPDATE backups SET destination=?2 WHERE destination=?1`:                               backupTo,
		`UPDATE operations SET recovery_json=replace(recovery_json,'"'||?1||'"','"'||?2||'"')`: backupTo,
	} {
		if _, err := d.Exec(statement, i.install+"/backups", to); err != nil {
			t.Fatal(err)
		}
	}

	if i.engine, err = fakedocker.Start(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { i.engine.Close() })
	helper.HostRoot = "/"
	t.Cleanup(func() { helper.HostRoot = "/host" })
	i.engine.AddImage(image)
	// Helpers see the host's paths, which are below host here.
	onHost := func(argument string) string { return strings.ReplaceAll(argument, `"`+i.install, `"`+i.host+i.install) }
	i.engine.RunHelpers(func(_ *fakedocker.Container, _, args []string, stdin *bufio.Reader, stdout, stderr func([]byte)) (int, bool) {
		if args == nil {
			return 0, true
		}
		for index := range args {
			args[index] = onHost(args[index])
		}
		return helper.Main(args, stdin, writerFunc(stdout), writerFunc(stderr)), true
	})
	i.engine.OnStart = func(c *fakedocker.Container) {
		if c.CreateRequest.Labels["ludock.internal"] == "mount-validator" {
			var out bytes.Buffer
			helper.RunMountProof(onHost(c.CreateRequest.Cmd[2]), "1", &out)
			i.engine.Update(c.ID, func(c *fakedocker.Container) { c.Logs = []string{strings.TrimSpace(out.String())} })
		}
	}
	encoded, err := os.ReadFile(filepath.Join(fixture, "docker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var containers []fakedocker.Container
	if err := json.Unmarshal(encoded, &containers); err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		i.engine.AddContainer(fakedocker.GameOptions{ID: container.ID})
		i.engine.Update(container.ID, func(c *fakedocker.Container) { *c = container })
	}
	i.engine.AddContainer(fakedocker.GameOptions{ID: "ludock", Name: "ludock", Image: image, Running: true,
		Mounts: []docker.MountPoint{{Type: "bind", Source: backupTo, Destination: backupTo, RW: true},
			{Type: "bind", Source: chosen, Destination: chosen, RW: true}}})

	i.jobs = wire(i.engine, d, backupTo, chosen)
	i.jobs.Notify = func(_ notify.Alert, _, message string) {
		i.mu.Lock()
		i.alerts = append(i.alerts, message)
		i.mu.Unlock()
	}
	if err := d.QueryRow("SELECT id FROM operations WHERE status='running'").Scan(&i.interrupted); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	i.runner = &ops.Runner{DB: d, Locks: i.jobs.Locks}
	i.jobs.Register(i.runner)
	if err := i.runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.runner.Stop)
	return i
}

type writerFunc func([]byte)

func (w writerFunc) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w(append([]byte(nil), p...))
	}
	return len(p), nil
}

// unpack extracts a gzipped tar into folder with each entry's mode.
func unpack(t *testing.T, archive, folder string) {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(unzipped)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(folder, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o700)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
				var content []byte
				if content, err = io.ReadAll(reader); err == nil {
					err = os.WriteFile(path, content, 0o600)
				}
			}
		default:
			t.Fatal("unexpected entry", header.Name)
		}
		if err == nil {
			err = os.Chmod(path, fs.FileMode(header.Mode).Perm())
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

// tree maps each path in folder to its content; folders end in /.
func tree(t *testing.T, folder string) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(folder, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == folder {
			return err
		}
		name, _ := filepath.Rel(folder, path)
		if entry.IsDir() {
			found[name+"/"] = ""
			return nil
		}
		content, err := os.ReadFile(path)
		found[name] = string(content)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// containers describes each container but Ludock's.
func (i *installed10) containers() []string {
	var described []string
	for _, id := range i.engine.SortedIDs() {
		if id == "ludock" {
			continue
		}
		i.engine.Update(id, func(c *fakedocker.Container) {
			policy, _ := c.HostConfig["RestartPolicy"].(map[string]any)
			described = append(described, fmt.Sprint(c.Name, " ", c.State.Status, " ", policy["Name"], " ", c.Image[len("sha256:"):][:4]))
		})
	}
	return described
}

func (i *installed10) started() string {
	for _, id := range i.engine.SortedIDs() {
		if c := i.engine.Container(id); c.Name == "/mc" {
			return c.State.StartedAt
		}
	}
	return ""
}

// startSent reports whether a start was sent to the container now named mc.
func (i *installed10) startSent() bool {
	for _, id := range i.engine.SortedIDs() {
		if c := i.engine.Container(id); c.Name == "/mc" {
			return slices.ContainsFunc(i.engine.Requests, func(request string) bool { return strings.HasSuffix(request, "/containers/"+id+"/start") })
		}
	}
	return false
}

// savedStart is when the container now named mc started, as 1.0 saw it.
func (i *installed10) savedStart(t *testing.T) string {
	encoded, err := os.ReadFile(filepath.Join("testdata/1.0/interrupted", filepath.Base(i.install), "docker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []fakedocker.Container
	json.Unmarshal(encoded, &saved)
	for _, c := range saved {
		if current := i.engine.Container(c.ID); current != nil && current.Name == "/mc" {
			return c.State.StartedAt
		}
	}
	return ""
}
