package jobs

import (
	"context"
	"slices"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

// AssertRunningForBackupRestart: restarting with a backup returns a running
// server to running. A stopped server is backed up from Backups, or started.
func AssertRunningForBackupRestart(state string) error {
	if state != "running" {
		return apperr.New("SERVER_NOT_RUNNING", 409, "Restart with a backup needs a running server. Start it, or back it up from Backups.")
	}
	return nil
}

// runBackupRestart restarts with a backup taken while the server is
// stopped, starting it once.
func (j *Jobs) runBackupRestart(ctx context.Context, job *ops.Context) (map[string]any, error) {
	server, err := j.resolve(ctx, job, authz.ServerRestart, true)
	if err != nil {
		return nil, err
	}
	assertAccess := func() error {
		if err := j.assertCapability(ctx, job, authz.ServerRestart); err != nil {
			return err
		}
		return j.assertCapability(ctx, job, authz.BackupsCreate)
	}
	return job.WithLocks(append(slices.Clone(server.LockKeys), "backups:storage"), func() (result map[string]any, err error) {
		current, err := j.resolve(ctx, job, authz.ServerRestart, true)
		if err != nil {
			return nil, err
		}
		if err := assertAccess(); err != nil {
			return nil, err
		}
		if err := AssertRunningForBackupRestart(current.Container.State); err != nil {
			return nil, err
		}
		if err := j.Backups.AssertReady(ctx, current); err != nil {
			return nil, err
		}
		if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
			return nil, err
		}
		defer func() {
			background := context.WithoutCancel(ctx)
			// Starting can update the game and change the world, so start only
			// after a completed backup and once the data helper is removed.
			if helper, _ := text(job.Job.Recovery["dataHelperId"]); j.backupCompleted(background, job) && helper == "" {
				if restoreErr := j.restoreInitialState(background, current, job); restoreErr != nil && err == nil {
					err = restoreErr
				}
			}
			j.noteAvailability(j.Monitor.Suppress(background, server.Logical.ID))
		}()
		result, err = func() (map[string]any, error) {
			if err := j.saveWorld(ctx, job, current, func() error {
				if err := assertAccess(); err != nil {
					return err
				}
				_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, current.Observation, job.Job.BindingRevision)
				return err
			}); err != nil {
				return nil, err
			}
			if err := j.stopForData(ctx, current, job); err != nil {
				return nil, err
			}
			backup, err := j.createStoppedBackup(ctx, current, job)
			if err != nil {
				return nil, err
			}
			if err := job.Progress("backup_complete", map[string]any{"backupId": backup.ID}, nil); err != nil {
				return nil, err
			}
			selected, err := j.resolve(ctx, job, authz.ServerRestart, true)
			if err != nil {
				return nil, err
			}
			if err := assertAccess(); err != nil {
				return nil, err
			}
			// Persist before dispatch; recovery never repeats a start that
			// reached Docker.
			if err := job.Progress("starting", map[string]any{"startRequested": true}, nil); err != nil {
				return nil, err
			}
			if err := j.Servers.Observer.ChangeState(ctx, selected.Container.ID, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
				if err := assertAccess(); err != nil {
					return err
				}
				_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, observation, job.Job.BindingRevision)
				return err
			}); err != nil {
				return nil, err
			}
			if err := job.Progress("started", map[string]any{"stateRestored": true}, nil); err != nil {
				return nil, err
			}
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
			recovery := job.Job.Recovery
			return map[string]any{"action": "restart", "backupId": recovery["backupId"], "saveStatus": recovery["saveStatus"],
				"saveReason": recovery["saveReason"]}, nil
		}()
		if err != nil {
			// A recorded stop may have reached Docker even if its result was
			// lost; reconcile it before reporting the server left stopped.
			if _, recorded := job.Job.Recovery["initialRunning"].(bool); recorded && !j.backupCompleted(ctx, job) {
				if keepErr := j.keepStoppedAfterFailedBackup(context.WithoutCancel(ctx), current, job); keepErr != nil {
					log.Warn("Could not confirm the server stayed stopped after a failed backup; review its state")
				}
			}
		}
		return result, err
	})
}

func (j *Jobs) recoverBackupRestart(ctx context.Context, job *ops.Context) error {
	// Starting is safe to reconcile, but only once the backup completed.
	if _, recorded := job.Job.Recovery["initialRunning"].(bool); recorded {
		return j.recoverBackup(ctx, job, j.backupCompleted(ctx, job))
	}
	return nil
}

func (j *Jobs) recoverUpdate(ctx context.Context, job *ops.Context) error {
	if job.Job.Recovery["mutationStarted"] == true {
		return nil
	}
	// Pulling changes no state. Backup recovery applies only after the
	// initial state and binding were recorded before stopping.
	if _, recorded := job.Job.Recovery["initialRunning"].(bool); !recorded {
		return nil
	}
	return j.recoverBackup(ctx, job, true)
}

// UpdateRequest is a confirmed Compose update.
type UpdateRequest struct {
	CreateBackup           bool   `json:"createBackup"`
	SkipBackupConfirmation string `json:"skipBackupConfirmation,omitempty"`
	ForceRecreate          bool   `json:"forceRecreate"`
}

func updateRequest(input map[string]any) (UpdateRequest, error) {
	raw, ok := input["request"].(map[string]any)
	if !ok {
		return UpdateRequest{}, apperr.New("INVALID_OPERATION_STATE", 409, "Saved operation state is invalid; administrator review is required")
	}
	request := UpdateRequest{}
	request.CreateBackup, _ = raw["createBackup"].(bool)
	request.ForceRecreate, _ = raw["forceRecreate"].(bool)
	request.SkipBackupConfirmation, _ = raw["skipBackupConfirmation"].(string)
	return request, nil
}

func (j *Jobs) runUpdate(ctx context.Context, job *ops.Context) (map[string]any, error) {
	request, err := updateRequest(job.Job.Input)
	if err != nil {
		return nil, err
	}
	capability := authz.ServerUpdate
	if request.ForceRecreate {
		capability = authz.ServerRecreate
	}
	server, err := j.resolve(ctx, job, capability, true)
	if err != nil {
		return nil, err
	}
	keys := slices.Clone(server.LockKeys)
	if request.CreateBackup {
		keys = append(keys, "backups:storage")
	}
	return job.WithLocks(keys, func() (result map[string]any, err error) {
		selected, err := j.Updates.Validated(ctx, server)
		if err != nil {
			return nil, err
		}
		defer selected.Snapshot.Cleanup()
		defer func() { j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), server.Logical.ID)) }()
		result, err = j.update(ctx, job, server, request, capability, selected.Snapshot.Fingerprint, selected.Snapshot.Args(), selected.Service, selected.Image)
		if err != nil {
			if job.Job.Recovery["mutationStarted"] != true {
				if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), server, job); restoreErr != nil {
					if err := job.Progress("recovery_required", map[string]any{"recoveryRequired": true}, nil); err != nil {
						log.Warn("Could not save that an update needs recovery; the failed operation still reports it")
					}
				}
			}
			j.notify("operation:"+job.Job.ID, server.Container.DisplayName+": update failed. Review the operation and actual server state.")
		}
		return result, err
	})
}

func (j *Jobs) update(ctx context.Context, job *ops.Context, server *servers.Context, request UpdateRequest, capability authz.Capability,
	fingerprint string, args []string, service, image string) (map[string]any, error) {
	if source, _ := text(job.Job.Input["sourceFingerprint"]); source != fingerprint {
		return nil, apperr.New("SOURCE_CHANGED", 409, "The Compose source changed after confirmation")
	}
	if err := j.Updates.AssertSingleContainer(ctx, server); err != nil {
		return nil, err
	}
	current, err := j.Docker.Inspect(ctx, server.Container.ID)
	if err != nil {
		return nil, err
	}
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return nil, err
	}
	if err := job.Progress("pulling", map[string]any{"containerId": server.Container.ID, "initiallyRunning": current.State.Running,
		"previousImageId": current.Image}, nil); err != nil {
		return nil, err
	}
	runner := j.Updates.Snapshots.Runner
	// Compose stops as soon as the owner loses update authority, as lifecycle
	// and data jobs do.
	if err := j.whileAuthorized(ctx, job, capability, func(ctx context.Context) error {
		_, err := runner.Run(ctx, append(slices.Clone(args), "pull", "--policy", "always", service), 15*time.Minute)
		return err
	}); err != nil {
		return nil, err
	}
	pulled, err := j.Docker.InspectImage(ctx, image)
	if err != nil {
		return nil, err
	}
	imageChanged := pulled != current.Image
	if !imageChanged && !request.ForceRecreate {
		j.notify("operation:"+job.Job.ID, server.Container.DisplayName+": configured image is current.")
		return map[string]any{"alreadyCurrent": true, "imageChanged": false, "recreated": false}, nil
	}
	if _, err := j.resolve(ctx, job, capability, true); err != nil {
		return nil, err
	}
	if err := job.Progress("validating", map[string]any{"selectedImageId": pulled}, nil); err != nil {
		return nil, err
	}
	if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
		return nil, err
	}
	if request.CreateBackup {
		if err := job.Progress("stopping", map[string]any{"stopRequested": true}, nil); err != nil {
			return nil, err
		}
		if err := j.stopForData(ctx, server, job); err != nil {
			return nil, err
		}
		if err := job.Progress("backing_up", nil, nil); err != nil {
			return nil, err
		}
		stopped := *server
		stopped.Container.State = "exited"
		backup, err := j.createStoppedBackup(ctx, &stopped, job)
		if err != nil {
			return nil, err
		}
		if err := job.Progress("validating", map[string]any{"backupId": backup.ID}, nil); err != nil {
			return nil, err
		}
	} else if request.SkipBackupConfirmation != server.Container.DisplayName {
		return nil, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm skipping the backup")
	}
	// Check the live source and image again; execution uses the first vetted
	// snapshot so source edits cannot redirect Compose after this.
	latest, err := j.Updates.Validated(ctx, server)
	if err != nil {
		return nil, err
	}
	latestFingerprint := latest.Snapshot.Fingerprint
	latest.Snapshot.Cleanup()
	if latestFingerprint != fingerprint {
		return nil, apperr.New("SOURCE_CHANGED", 409, "Compose source changed during the update")
	}
	if _, err := j.resolve(ctx, job, capability, true); err != nil {
		return nil, err
	}
	if err := j.Updates.AssertSingleContainer(ctx, server); err != nil {
		return nil, err
	}
	if again, err := j.Docker.InspectImage(ctx, image); err != nil || again != pulled {
		return nil, apperr.New("IMAGE_CHANGED", 409, "Another manager changed the selected image during the update")
	}
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return nil, err
	}
	// Recovery chooses its path from this checkpoint, so Compose runs only
	// once it is saved.
	if err := job.Progress("recreating", map[string]any{"mutationStarted": true}, nil); err != nil {
		return nil, err
	}
	flags := []string{"up", "--no-start"}
	if current.State.Running {
		flags = []string{"up", "-d", "--wait", "--wait-timeout", "180"}
	}
	flags = append(flags, "--no-deps", "--no-build", "--pull", "never")
	if request.ForceRecreate {
		flags = append(flags, "--force-recreate")
	}
	if err := j.whileAuthorized(ctx, job, capability, func(ctx context.Context) error {
		_, err := runner.Run(ctx, append(append(slices.Clone(args), flags...), service), 4*time.Minute)
		return err
	}); err != nil {
		return nil, err
	}
	if err := job.Progress("verifying", nil, nil); err != nil {
		return nil, err
	}
	if _, err := j.Servers.RefreshNow(ctx); err != nil {
		return nil, err
	}
	replacement, err := j.resolve(ctx, job, capability, false)
	if err != nil {
		return nil, err
	}
	actual, err := j.Docker.Inspect(ctx, replacement.Container.ID)
	if err != nil {
		return nil, err
	}
	if actual.Image != pulled || actual.State.Running != current.State.Running || (actual.State.Health != nil && actual.State.Health.Status == "unhealthy") {
		return nil, apperr.New("VERIFY_FAILED", 409, "The resulting server state did not match the requested update; review it through Compose")
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, !current.State.Running))
	action := "update"
	if request.ForceRecreate {
		action = "recreation"
	}
	j.notify("operation:"+job.Job.ID, server.Container.DisplayName+": "+action+" completed.")
	verification := "docker-state"
	if actual.State.Health != nil {
		verification = "docker-health"
	}
	return map[string]any{"imageChanged": imageChanged, "recreated": true, "forced": request.ForceRecreate,
		"running": actual.State.Running, "verification": verification}, nil
}

// whileAuthorized runs fn under a context cancelled as soon as the job's owner
// loses capability, and then reports that loss instead of fn's error.
func (j *Jobs) whileAuthorized(ctx context.Context, job *ops.Context, capability authz.Capability, fn func(context.Context) error) error {
	watch, stopWatch := context.WithCancelCause(ctx)
	go func() {
		ticker := time.NewTicker(j.poll())
		defer ticker.Stop()
		for {
			select {
			case <-watch.Done():
				return
			case <-ticker.C:
				if err := j.assertCapability(watch, job, capability); err != nil && watch.Err() == nil {
					stopWatch(err)
					return
				}
			}
		}
	}()
	err := fn(watch)
	revoked := context.Cause(watch)
	stopWatch(nil)
	if ctx.Err() != nil {
		return err
	}
	// A loss seen as the command finished, or since the last poll, is
	// reported even when the command itself succeeded.
	if revoked != nil {
		return revoked
	}
	if err == nil {
		err = j.assertCapability(ctx, job, capability)
	}
	return err
}
