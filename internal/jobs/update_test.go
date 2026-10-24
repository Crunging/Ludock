package jobs

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
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
// Docker and the saved swap.
func (f *fixture) interrupt(t *testing.T, prepare func(id string, swap *swapState)) *ops.Job {
	t.Helper()
	logical, _ := f.jobs.Identity.Get(ctx, f.serverID)
	id := identity.NewUUID()
	swap := swapState{Stage: swapCreating, Name: "mc", Temporary: servers.ReplacementName("mc", id),
		RestartPolicy: json.RawMessage(`{"Name":"unless-stopped"}`)}
	prepare(id, &swap)
	saved := map[string]any{"containerId": f.game, "fingerprint": logical.BindingFingerprint, "lockKeys": []string{"server:" + f.serverID},
		"initialRunning": true, "stopStarted": true, "stopSettled": true, "dataSafe": true, "swap": swap}
	encoded, _ := json.Marshal(saved)
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

// Ludock restarted while the replacement was still being checked: the
// original remains the server. It starts again only if the replacement
// never ran, since a replacement that ran may have changed the world.
func TestUpdateRecoveryUndoesUncheckedReplacement(t *testing.T) {
	for _, ran := range []bool{false, true} {
		f := setup(t)
		if err := f.jobs.Docker.Stop(ctx, f.game, nil); err != nil {
			t.Fatal(err)
		}
		job := f.interrupt(t, func(id string, swap *swapState) {
			f.replacement(t, id, ran)
			if ran {
				swap.Stage = swapStarting
			}
		})
		if job.Status != ops.Interrupted || job.Error != "Ludock restarted during this operation. Review the server state before retrying." {
			t.Fatalf("%+v", job)
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
// still being removed, so it is removed again first.
func TestUpdateRecoveryFinishesReplacement(t *testing.T) {
	for _, left := range []bool{false, true} {
		f := setup(t)
		var replacement string
		job := f.interrupt(t, func(id string, swap *swapState) {
			replacement = f.replacement(t, id, true)
			swap.Stage = swapReplacing
			if left {
				f.engine.Update(f.game, func(c *fakedocker.Container) { c.State.Status, c.State.Running = "dead", false })
			} else {
				f.engine.RemoveContainer(f.game)
			}
		})
		if job.Status != ops.Interrupted {
			t.Fatalf("%+v", job)
		}
		current := f.containerNamed("mc")
		if current == nil || current.ID != replacement || !current.State.Running || f.engine.Container(f.game) != nil {
			t.Fatalf("original left: %v, replacement did not take the server's name: %+v", left, current)
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
			f.engine.Update(c.ID, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, 1
				f.engine.FailRemoves = 3 // Update holds the engine's lock.
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
	job := f.interrupt(t, func(_ string, swap *swapState) {
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
			f.engine.Update(c.ID, func(c *fakedocker.Container) {
				c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, 1
				f.engine.Fail = 500 // Update holds the engine's lock.
			})
			go func() {
				time.Sleep(300 * time.Millisecond)
				f.engine.Update(f.game, func(*fakedocker.Container) { f.engine.Fail = 0 })
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
// and if it is gone too, recovery says so.
func TestUpdateRecoveryKeepsOriginalWithoutReplacement(t *testing.T) {
	for _, originalLeft := range []bool{true, false} {
		f := setup(t)
		f.jobs.Docker.Stop(ctx, f.game, nil)
		job := f.interrupt(t, func(_ string, swap *swapState) {
			if !originalLeft {
				f.engine.RemoveContainer(f.game)
			}
			swap.Stage = swapReplacing
		})
		if job.Status != ops.Interrupted || !strings.Contains(job.Error, "administrator attention") {
			t.Fatalf("%+v", job)
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
	job := f.interrupt(t, func(id string, swap *swapState) {
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
	release, err := f.jobs.Locks.Acquire([]string{"server:" + f.serverID}, nil)
	if err != nil {
		t.Fatal("server still held:", err)
	}
	release()
}

// A new version that declares another volume gets it created, and since the
// server's writable data changed, an administrator reviews the binding.
func TestUpdateNewImageVolumeNeedsReview(t *testing.T) {
	f := setup(t)
	f.engine.SetImageConfig("sha256:"+strings.Repeat("1", 64), map[string]any{"Volumes": map[string]any{"/cache": map[string]any{}}})
	if job := f.run(t, "update", skipBackup); job.Status != ops.Succeeded {
		t.Fatalf("%+v", job)
	}
	current := f.containerNamed("mc")
	if current == nil || !slices.ContainsFunc(current.Mounts, func(m docker.MountPoint) bool { return m.Destination == "/cache" && m.Type == "volume" }) {
		t.Fatalf("volume not created: %+v", current)
	}
	if logical, _ := f.jobs.Identity.Get(ctx, f.serverID); logical.Status != identity.StatusReviewRequired {
		t.Fatalf("binding not held for review: %+v", logical)
	}
}

// Another manager removed the original before the update began replacing
// it. The replacement was never checked, so it doesn't become the server;
// recovery removes it and asks for review.
func TestUpdateRecoveryNeverPromotesUncheckedReplacement(t *testing.T) {
	f := setup(t)
	var replacement string
	job := f.interrupt(t, func(id string, swap *swapState) {
		replacement = f.replacement(t, id, true)
		f.engine.RemoveContainer(f.game)
		swap.Stage = swapStarting
	})
	if job.Status != ops.Interrupted || !strings.Contains(job.Error, "administrator attention") || f.engine.Container(replacement) != nil {
		t.Fatalf("%+v", job)
	}
}
