package jobs

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var skipBackup = map[string]any{"request": map[string]any{"createBackup": false, "skipBackupConfirmation": "mc"}}

// containerNamed returns the container with name, or nil.
func (f *fixture) containerNamed(name string) *docker.ContainerInspect {
	for _, id := range f.engine.SortedIDs() {
		if info := f.engine.Container(id); info != nil && info.Name == "/"+name {
			return info
		}
	}
	return nil
}

// A new version that crashes as it starts may already have migrated the
// world, so the original container comes back, stopped, and the crashed
// replacement is removed.
func TestUpdateKeepsOriginalStoppedWhenReplacementFails(t *testing.T) {
	f := setup(t)
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if strings.Contains(c.Name, "-ludock-update-") {
			f.engine.Update(c.ID, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, 1
			})
		}
	}
	job := f.run(t, "update", skipBackup)
	if job.Status != ops.Failed || !strings.Contains(job.Error, "stopped or restarted right after starting") {
		t.Fatalf("%+v", job)
	}
	original := f.containerNamed("mc")
	if original == nil || original.ID != f.game || original.State.Running {
		t.Fatalf("original not restored and stopped: %+v", original)
	}
	if ids := f.engine.SortedIDs(); len(ids) != 2 {
		t.Fatal("replacement left behind:", ids)
	}
	if logical, _ := f.jobs.Identity.Get(ctx, f.serverID); logical.ContainerID != f.game || logical.Status != identity.StatusActive {
		t.Fatalf("binding moved: %+v", logical)
	}
}

// interrupt saves an update that was running when Ludock stopped, while it
// swapped containers, then starts Ludock's runner again. prepare sets up
// Docker, the saved swap, and the rest of what was saved; a swap it gives no
// stage is before the swap.
func (f *fixture) interrupt(t *testing.T, prepare func(id string, swap *swapState, recovery *ops.Recovery)) *ops.Job {
	t.Helper()
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	id := identity.NewUUID()
	swap := swapState{Stage: swapCreating, Name: "mc", Temporary: servers.ReplacementName("mc", id),
		RestartPolicy: json.RawMessage(`{"Name":"unless-stopped"}`)}
	running := !f.wasStopped
	recovery := ops.Recovery{ContainerID: f.game, Fingerprint: logical.BindingFingerprint,
		LockKeys: []string{locks.ServerKey(f.serverID)}, InitialRunning: &running, StopStarted: true, StopSettled: true}
	prepare(id, &swap, &recovery)
	if swap.Stage != "" {
		recovery.Swap, _ = json.Marshal(swap)
	}
	encoded, _ := json.Marshal(recovery)
	input, _ := json.Marshal(skipBackup)
	if _, err := f.jobs.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
		VALUES (?,?,'admin','update','running','starting',?,?,?,?,?)`, id, f.serverID, string(input), string(encoded),
		logical.BindingRevision, db.Now(), db.Now()); err != nil {
		t.Fatal(err)
	}
	runner := &ops.Runner{DB: f.jobs.DB, Locks: f.jobs.Locks}
	f.jobs.Register(runner)
	if err := runner.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runner.Stop)
	job, err := runner.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// replacement creates and optionally starts a container under an update's
// temporary name, labeled as that update's own unless operation is "".
func (f *fixture) replacement(t *testing.T, operation string, start bool) string {
	t.Helper()
	f.engine.AddImage("itzg/minecraft-server")
	labels := map[string]string{}
	if operation != "" {
		labels[update.OperationLabel] = operation
	}
	name := servers.ReplacementName("mc", operation)
	id, err := f.jobs.Docker.Create(ctx, name, "", map[string]any{"Image": "itzg/minecraft-server", "Labels": labels})
	if err == nil && start {
		err = f.jobs.Docker.Start(ctx, id)
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Ludock restarted during an update, before or while the replacement was
// being checked: the original remains the server. An interruption isn't a
// failed update, so a server that was running starts again, unless the
// replacement had run, since it may have changed the world. Activity says
// which.
func TestUpdateRecoveryUndoesUncheckedReplacement(t *testing.T) {
	for _, c := range []struct {
		stage    string
		guidance string
	}{
		{"", "Ludock restarted during the update, so the update didn’t happen. The server was started again as it was."},
		{swapCreating, "Ludock restarted during the update, so the update didn’t happen. The server was started again as it was."},
		{swapStarting, "Ludock restarted while it checked the updated container, so the update was undone and the previous container kept. " +
			"The server was left stopped, since the new version may already have run and changed the game data. Check the server, then start it."},
	} {
		ran := c.stage == swapStarting
		f := setup(t)
		if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
			t.Fatal(err)
		}
		job := f.interrupt(t, func(id string, swap *swapState, _ *ops.Recovery) {
			if c.stage != "" {
				f.replacement(t, id, ran)
			}
			swap.Stage = c.stage
		})
		// The guidance says what the server was left as, so the entry doesn't
		// ask to check it.
		if job.Status != ops.Interrupted || job.Error != "Ludock restarted while this ran." || job.Result.Guidance != c.guidance {
			t.Fatalf("%+v", job)
		}
		if c.stage == "" {
			if !f.running() {
				t.Fatal("left stopped")
			}
			continue
		}
		original := f.containerNamed("mc")
		if ids := f.engine.SortedIDs(); len(ids) != 2 || original == nil || original.ID != f.game {
			t.Fatal("replacement left behind or original lost:", ids)
		}
		if original.State.Running == ran {
			t.Fatalf("replacement ran: %v, original running: %v", ran, original.State.Running)
		}
	}
}

// Ludock restarted after it began removing the original: the checked
// replacement takes its name and becomes the server, never undone. A
// removal that failed or lost its response can leave the original dead or
// still being removed, so it is removed again first. A replacement that a
// Docker restart stopped meanwhile, having no restart policy then, is
// started once, since the server was running. Activity and an alert say the
// server is stopped when it is at the end: Docker refused the start, the
// new run exited at once, or it had stopped after the update had already
// started it. One restarting, or Docker doesn't answer about, is not called
// stopped.
func TestUpdateRecoveryFinishesReplacement(t *testing.T) {
	finished := "Ludock restarted while the updated container replaced the previous one, so it finished the update."
	stopped := finished + " Its new container is stopped, so the server is stopped. Check its logs, then start it."
	removed := finished + " Its new container was then removed outside Ludock, so the server no longer exists. Recreate it through the tool that created it."
	for _, c := range []struct {
		name                                string
		left, stopped, wasStopped, restored bool
		fails, exits, restarts, unreachable bool
		// looping: Docker restarts the replacement in a loop as Ludock
		// starts; removed: something removed it.
		looping, removed bool
		guidance         string
	}{
		{name: "original removed", guidance: finished},
		{name: "server was stopped", stopped: true, wasStopped: true, guidance: finished},
		{name: "original left dead", left: true, guidance: finished},
		{name: "replacement stopped", stopped: true, guidance: finished},
		{name: "replacement won't start", stopped: true, fails: true, guidance: finished +
			" Its new container had stopped, and Docker couldn’t start it again, so the server is stopped. Check it, then start it."},
		{name: "replacement exits once started", stopped: true, exits: true, guidance: stopped},
		{name: "replacement restarts once started", stopped: true, restarts: true, guidance: finished +
			" Its new container restarted right after it started. Check its logs."},
		{name: "stopped after the update started it", stopped: true, restored: true, guidance: stopped},
		{name: "restarting after the update started it", restored: true, looping: true, guidance: finished +
			" Its new container restarted right after it started. Check its logs."},
		{name: "replacement removed", removed: true, guidance: removed},
		{name: "replacement of a stopped server removed", stopped: true, wasStopped: true, removed: true, guidance: removed},
		{name: "Docker stops answering", stopped: true, unreachable: true, guidance: finished +
			" Ludock couldn’t reach Docker to check that its new container is running. Check the server."},
	} {
		f := setup(t)
		f.wasStopped = c.wasStopped
		f.jobs.Docker.OutageLimit = time.Second
		var replacement string
		starts := 0
		started := f.engine.OnStart
		f.engine.OnStart = func(container *fakedocker.Container) {
			started(container)
			if container.ID != replacement {
				return
			}
			starts++
			if c.exits {
				f.engine.Update(container.ID, func(container *fakedocker.Container) {
					container.State.Status, container.State.Running, container.State.ExitCode = "exited", false, 1
				})
			}
			if c.restarts {
				f.engine.Update(container.ID, func(container *fakedocker.Container) { container.State.Restarting = true })
			}
			if c.unreachable {
				f.engine.AddFault("GET .*/containers/"+replacement+"/json", 500, false, 0)
			}
		}
		job := f.interrupt(t, func(id string, swap *swapState, recovery *ops.Recovery) {
			replacement = f.replacement(t, id, !c.stopped)
			swap.Stage = swapReplacing
			if c.left {
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "dead", false })
			} else {
				f.engine.RemoveContainer(f.game)
			}
			if c.fails {
				f.engine.AddFault("POST .*/containers/"+replacement+"/start", 500, false, 0)
			}
			if c.restored {
				if err := f.jobs.Docker.Rename(ctx, replacement, "mc"); err != nil {
					t.Fatal(err)
				}
				if err := f.jobs.Docker.SetRestartPolicy(ctx, replacement, json.RawMessage(`{"Name":"unless-stopped"}`)); err != nil {
					t.Fatal(err)
				}
				swap.Stage, swap.Replacement = swapReplaced, replacement
				recovery.StartRequested, recovery.StateRestored = true, true
			}
			if c.looping {
				f.engine.Update(replacement, func(c *fakedocker.Container) { c.State.Status, c.State.Restarting = "restarting", true })
			}
			if c.removed {
				swap.Stage, swap.Replacement = swapReplaced, replacement
				f.engine.RemoveContainer(replacement)
			}
		})
		// Only an update that finished with its server as it should be ends
		// as succeeded.
		status := ops.Interrupted
		if c.guidance == finished {
			status = ops.Succeeded
		}
		if job.Status != status || job.Result.Guidance != c.guidance {
			t.Fatalf("%s: %+v", c.name, job)
		}
		down := c.fails || c.exits || (c.restored && c.stopped) || c.removed
		if want := map[bool]int{true: 0, false: 1}[c.wasStopped || c.fails || c.restored || !c.stopped]; starts != want {
			t.Fatalf("%s: replacement started %d times", c.name, starts)
		}
		current, taken := f.containerNamed("mc"), &fakedocker.Container{}
		f.engine.Update(replacement, func(c *fakedocker.Container) { *taken = *c })
		if c.removed {
			if current != nil {
				t.Fatalf("%s: %+v", c.name, current)
			}
		} else if current == nil || current.ID != replacement || current.State.Running != (!down && !c.wasStopped) || f.engine.Container(f.game) != nil ||
			restartPolicy(taken) != "unless-stopped" {
			t.Fatalf("%s: replacement did not take over the server as it was: %+v", c.name, current)
		}
		// A server left stopped on purpose is no outage; one Docker didn't
		// answer about is left to outage tracking.
		var intentional bool
		f.jobs.DB.QueryRow("SELECT intentionally_stopped FROM availability WHERE server_id=?", f.serverID).Scan(&intentional)
		if intentional != (down || c.wasStopped) {
			t.Fatalf("%s: intentionally stopped %v", c.name, intentional)
		}
		if alerted := len(f.alerts) > 0; alerted != (down || c.restarts || c.looping || c.unreachable) {
			t.Fatalf("%s: alerts %q", c.name, f.alerts)
		}
	}
}

func restartPolicy(info *fakedocker.Container) any {
	policy, _ := info.HostConfig["RestartPolicy"].(map[string]any)
	return policy["Name"]
}

// While both containers exist, neither may restart on its own: a Docker
// restart would start both against the same world. The replacement takes
// over the original's policy, the MAC address the owner configured, which
// Docker clears from a stopped container's inspection, and the image
// platform the original ran, which may not be the daemon's own, with that
// platform's defaults left to the new image.
func TestUpdateSwapKeepsPolicyMACAndPlatform(t *testing.T) {
	f := setup(t)
	f.engine.Update(f.game, func(c *fakedocker.Container) {
		c.HostConfig = map[string]any{"NetworkMode": "games", "Binds": []string{f.data + ":" + f.data},
			"RestartPolicy": map[string]any{"Name": "always", "MaximumRetryCount": 0}}
		c.Networks = map[string]docker.EndpointSettings{"games": {Aliases: []string{"mc"}, MacAddress: "52:54:00:12:34:56", IPAddress: "172.20.0.5"}}
		c.ImageManifestDescriptor = &docker.ManifestDescriptor{Digest: "sha256:arm",
			Platform: &docker.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}}
		c.ExtraConfig = map[string]any{"Cmd": []any{"start-arm64"}}
	})
	// The arm64 variant's command is a default, not the owner's; Docker
	// would answer for its own platform's variant unless asked.
	f.engine.SetImageConfig(f.engine.Container(f.game).Image, map[string]any{"Cmd": []any{"start-amd64"}})
	f.engine.SetVariantConfig(f.engine.Container(f.game).Image, docker.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"},
		map[string]any{"Cmd": []any{"start-arm64"}})
	var during []any
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if strings.Contains(c.Name, "-ludock-update-") {
			original, replacement := &fakedocker.Container{}, &fakedocker.Container{}
			f.engine.Update(f.game, func(c *fakedocker.Container) { *original = *c })
			f.engine.Update(c.ID, func(c *fakedocker.Container) { *replacement = *c })
			during = []any{restartPolicy(original), restartPolicy(replacement)}
		}
	}
	if job := f.run(t, "update", skipBackup); job.Status != ops.Succeeded {
		t.Fatalf("%+v", job)
	}
	if len(during) != 2 || during[0] != "no" || during[1] != "no" {
		t.Fatal("restart policies while both existed:", during)
	}
	current := f.containerNamed("mc")
	if current == nil || current.ID == f.game || !current.State.Running {
		t.Fatalf("not replaced: %+v", current)
	}
	var replacement *fakedocker.Container
	f.engine.Update(current.ID, func(c *fakedocker.Container) { replacement = c })
	if restartPolicy(replacement) != "always" || replacement.Networks["games"].MacAddress != "52:54:00:12:34:56" ||
		replacement.Networks["games"].Aliases[0] != "mc" || replacement.Platform != "linux/arm64/v8" || replacement.CreateRequest.Cmd != nil {
		t.Fatalf("lost settings: %v %+v", replacement.HostConfig["RestartPolicy"], replacement.Networks)
	}
	if logical, _ := f.jobs.Identity.Get(ctx, f.serverID); logical.ContainerID != current.ID || logical.Status != identity.StatusActive {
		t.Fatalf("binding did not move to the replacement: %+v", logical)
	}
}

// A replacement that may be writing is removed before the update releases
// the server, even if Docker fails to remove it at first.
func TestUpdateRetriesRemovingFailedReplacement(t *testing.T) {
	f := setup(t)
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if strings.Contains(c.Name, "-ludock-update-") {
			f.engine.AddFault(`^DELETE .*/containers/[^/]+$`, 500, false, 3)
			f.engine.Update(c.ID, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, 1
			})
		}
	}
	if job := f.run(t, "update", skipBackup); job.Status != ops.Failed {
		t.Fatalf("%+v", job)
	}
	if ids := f.engine.SortedIDs(); len(ids) != 2 || f.containerNamed("mc").ID != f.game {
		t.Fatal("replacement left behind:", ids)
	}
}

// After a restart whose create response was lost, a container someone else
// gave the temporary name is not the update's to remove.
func TestUpdateRecoveryLeavesOtherContainers(t *testing.T) {
	f := setup(t)
	f.jobs.Docker.Stop(ctx, f.game, nil)
	var stranger string
	job := f.interrupt(t, func(_ string, swap *swapState, _ *ops.Recovery) {
		stranger = f.replacement(t, "", false)
		swap.Temporary = servers.ReplacementName("mc", "")
	})
	if job.Status != ops.Interrupted || f.engine.Container(stranger) == nil || f.containerNamed("mc").ID != f.game {
		t.Fatalf("%+v", job)
	}
}

// Docker can fail while an update undoes itself. The failed replacement may
// still be writing, so the update keeps retrying, holding the server, until
// Docker confirms it is gone.
func TestUpdateUndoRetriesThroughDockerFailures(t *testing.T) {
	f := setup(t)
	started := f.engine.OnStart
	f.engine.OnStart = func(c *fakedocker.Container) {
		started(c)
		if strings.Contains(c.Name, "-ludock-update-") {
			f.engine.AddFault(".", 500, false, 0)
			f.engine.Update(c.ID, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, 1
			})
			go func() {
				time.Sleep(300 * time.Millisecond)
				f.engine.ClearFaults()
			}()
		}
	}
	if job := f.run(t, "update", skipBackup); job.Status != ops.Failed || job.Error == "" {
		t.Fatalf("%+v", job)
	}
	if ids := f.engine.SortedIDs(); len(ids) != 2 || f.containerNamed("mc").ID != f.game {
		t.Fatal("replacement left behind:", ids)
	}
}

// Another manager removed the replacement after the update began replacing
// the original. The original is kept for review instead of being removed,
// and if it is gone too, recovery says so. Either way an alert goes out.
func TestUpdateRecoveryKeepsOriginalWithoutReplacement(t *testing.T) {
	for _, originalLeft := range []bool{true, false} {
		f := setup(t)
		f.jobs.Docker.Stop(ctx, f.game, nil)
		job := f.interrupt(t, func(_ string, swap *swapState, _ *ops.Recovery) {
			if !originalLeft {
				f.engine.RemoveContainer(f.game)
			}
			swap.Stage = swapReplacing
		})
		if job.Status != ops.Interrupted || !strings.Contains(job.Error, "administrator attention") || len(f.alerts) != 1 {
			t.Fatalf("%+v %q", job, f.alerts)
		}
		if originalLeft && f.containerNamed("mc") == nil {
			t.Fatal("removed the only container left")
		}
	}
}

// Docker refuses to change a dead container's restart policy. Once the
// replacement is confirmed gone, nothing writes any more, so recovery
// reports the policy instead of retrying forever and releases the server.
func TestUpdateRecoveryEndsWhenPolicyCannotBeRestored(t *testing.T) {
	f := setup(t)
	job := f.interrupt(t, func(id string, swap *swapState, _ *ops.Recovery) {
		f.replacement(t, id, true)
		f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "dead", false })
		swap.Stage = swapStarting
	})
	if job.Status != ops.Interrupted || !strings.Contains(job.Error, "administrator attention") {
		t.Fatalf("%+v", job)
	}
	if ids := f.engine.SortedIDs(); len(ids) != 2 {
		t.Fatal("replacement left behind:", ids)
	}
	release, err := f.jobs.Locks.Acquire([]string{locks.ServerKey(f.serverID)}, nil)
	if err != nil {
		t.Fatal("server still held:", err)
	}
	release()
}

// A new version that declares another volume gets it created. It is a new
// data folder, so access shared with others pauses until an administrator
// confirms the change.
func TestUpdateNewImageVolumePausesSharing(t *testing.T) {
	f := setup(t)
	if _, err := f.jobs.DB.Exec(`INSERT INTO users(id, username, role, password_hash, created_at, updated_at) VALUES ('friend', 'friend', 'member', 'x', 1, 1);
    INSERT INTO server_grants(user_id, server_id, capabilities_json, updated_at) VALUES ('friend', ?, '["server.view"]', 1)`, f.serverID); err != nil {
		t.Fatal(err)
	}
	f.engine.SetImageConfig("sha256:"+strings.Repeat("1", 64), map[string]any{"Volumes": map[string]any{"/cache": map[string]any{}}})
	if job := f.run(t, "update", skipBackup); job.Status != ops.Succeeded {
		t.Fatalf("%+v", job)
	}
	current := f.containerNamed("mc")
	if current == nil || !slices.ContainsFunc(current.Mounts, func(m docker.MountPoint) bool { return m.Destination == "/cache" && m.Type == "volume" }) {
		t.Fatalf("volume not created: %+v", current)
	}
	if logical, _ := f.jobs.Identity.Get(ctx, f.serverID); logical.Status != identity.StatusActive || !logical.SharingPaused {
		t.Fatalf("sharing not paused: %+v", logical)
	}
}

// Another manager removed the original before the update began replacing
// it. The replacement was never checked, so it doesn't become the server;
// recovery removes it and asks for review.
func TestUpdateRecoveryNeverPromotesUncheckedReplacement(t *testing.T) {
	f := setup(t)
	var replacement string
	job := f.interrupt(t, func(id string, swap *swapState, _ *ops.Recovery) {
		replacement = f.replacement(t, id, true)
		f.engine.RemoveContainer(f.game)
		swap.Stage = swapStarting
	})
	if job.Status != ops.Interrupted || !strings.Contains(job.Error, "administrator attention") || f.engine.Container(replacement) != nil {
		t.Fatalf("%+v", job)
	}
}

// A destination-only bind from an API client makes an anonymous volume that
// Docker reports without its name. The update finds the volume by where it
// is mounted and has the replacement's bind name it, so the data carries
// over.
func TestUpdateKeepsVolumeOfDestinationOnlyBind(t *testing.T) {
	f := setup(t)
	f.engine.AddImage("itzg/minecraft-server")
	original, err := f.jobs.Docker.Create(ctx, "state", "", map[string]any{"Image": "itzg/minecraft-server",
		"Labels": map[string]string{"ludock.enable": "true"}, "HostConfig": map[string]any{"Binds": []string{"/state"}}})
	if err != nil || f.jobs.Docker.Start(ctx, original) != nil {
		t.Fatal(err)
	}
	volume := f.engine.Container(original).Mounts[0]
	if volume.Name != "" {
		t.Fatal("the fake should leave the name out, as Docker does")
	}
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	views, _ := f.jobs.Servers.List(ctx, f.admin)
	for _, view := range views {
		if view.Name == "state" {
			f.serverID = view.ID
		}
	}
	if job := f.run(t, "update", map[string]any{"request": map[string]any{"skipBackupConfirmation": "state"}}); job.Status != ops.Succeeded {
		t.Fatalf("%+v", job)
	}
	current := f.containerNamed("state")
	if current == nil || current.ID == original || len(current.Mounts) != 1 || current.Mounts[0].Name != path.Base(path.Dir(volume.Source)) {
		t.Fatalf("volume not carried over: %+v", current)
	}
}

// An image that declares a volume, as itzg/minecraft-server does for /data,
// gives its container an unnamed one. The replacement can keep it only by
// naming it, which Docker's rm -v then leaves alone, so the update says so;
// a later update, which finds it named, says nothing.
func TestUpdateNotesItNamedAnUnnamedVolume(t *testing.T) {
	f := setup(t)
	f.engine.AddImage("itzg/minecraft-server")
	f.engine.SetImageConfig("sha256:"+strings.Repeat("2", 64), map[string]any{"Volumes": map[string]any{"/data": map[string]any{}}})
	original, err := f.jobs.Docker.Create(ctx, "state", "", map[string]any{"Image": "itzg/minecraft-server",
		"Labels": map[string]string{"ludock.enable": "true"}, "HostConfig": map[string]any{"NetworkMode": "bridge"}})
	if err != nil || f.jobs.Docker.Start(ctx, original) != nil {
		t.Fatal(err)
	}
	if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
		t.Fatal(err)
	}
	views, _ := f.jobs.Servers.List(ctx, f.admin)
	for _, view := range views {
		if view.Name == "state" {
			f.serverID = view.ID
		}
	}
	request := map[string]any{"request": map[string]any{"skipBackupConfirmation": "state", "forceRecreate": true}}
	job := f.run(t, "update", request)
	if job.Status != ops.Succeeded || job.Result.Guidance != unnamedVolumeGuidance {
		t.Fatalf("%+v", job)
	}
	if current := f.containerNamed("state"); current == nil || current.ID == original {
		t.Fatalf("not replaced: %+v", current)
	}
	if job := f.run(t, "update", request); job.Status != ops.Succeeded || job.Result.Guidance != "" {
		t.Fatalf("%+v", job)
	}
}

// A console helper on the server's network, such as a player reading's,
// holds the server lock until it is removed, so it doesn't block an update;
// any other container on that network does.
func TestUpdateCheckSkipsConsoleHelpers(t *testing.T) {
	f := setup(t)
	onNetwork := func(name string, labels map[string]string) {
		id := f.engine.AddContainer(fakedocker.GameOptions{Name: name, Image: "example/sidecar", Running: true, Labels: labels})
		f.engine.Update(id, func(c *fakedocker.Container) { c.HostConfig = map[string]any{"NetworkMode": "container:" + f.game} })
	}
	check := func() error {
		server, err := f.jobs.Servers.ResolveAuthorized(ctx, f.admin, f.serverID, authz.ServerUpdate, -1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.jobs.Updates.Check(ctx, server)
		return err
	}
	onNetwork("ludock-console-reading", map[string]string{"ludock.enable": "false", "ludock.internal": "console-helper"})
	if err := check(); err != nil {
		t.Fatal(err)
	}
	onNetwork("sidecar", nil)
	if err := check(); err == nil || !strings.Contains(err.Error(), "shares this server’s network") {
		t.Fatal(err)
	}
	// The update panel renders without inspecting every container; the
	// update itself still refuses.
	server, err := f.jobs.Servers.ResolveAuthorized(ctx, f.admin, f.serverID, authz.ServerUpdate, -1)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.engine.Requests)
	if capability := f.jobs.Updates.Capability(ctx, server, func() bool { return false }); !capability.Available {
		t.Fatal(capability.UnavailableReason)
	}
	for _, request := range f.engine.Requests[before:] {
		if strings.Contains(request, "/containers/") && !strings.Contains(request, f.game) {
			t.Fatal("inspected another container", request)
		}
	}
}

// Something outside Ludock starts or stops the server while the update
// pulls its image. The update goes by the state it found when it stopped
// the server: one started meanwhile is saved, or says why not, before the stop and comes back
// running on the new container, and one stopped meanwhile stays stopped, as
// its owner left it.
func TestUpdateKeepsStateChangedDuringPull(t *testing.T) {
	for _, startedElsewhere := range []bool{true, false} {
		f := setup(t)
		if startedElsewhere {
			if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := f.jobs.Servers.RefreshNow(ctx); err != nil {
				t.Fatal(err)
			}
		}
		f.engine.OnPull = func(string) {
			f.engine.OnPull = nil
			if startedElsewhere {
				f.startElsewhere()
			} else {
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "exited", false })
			}
		}
		job := f.run(t, "update", skipBackup)
		current := f.containerNamed("mc")
		var stopped bool
		f.jobs.DB.QueryRow("SELECT intentionally_stopped FROM availability WHERE server_id=?", f.serverID).Scan(&stopped)
		if job.Status != ops.Succeeded || current == nil || current.ID == f.game || current.State.Running != startedElsewhere ||
			job.Result.Running != startedElsewhere || stopped == startedElsewhere || (job.Result.SaveStatus != "") != startedElsewhere {
			t.Fatalf("started elsewhere %v: %+v, running %v, intentionally stopped %v", startedElsewhere, job, current.State.Running, stopped)
		}
	}
}
