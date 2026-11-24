package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
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
// stopped, starting it once if it was running when Ludock stopped it.
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
	return job.WithLocks(append(slices.Clone(server.LockKeys), locks.StorageKey), func() (result map[string]any, err error) {
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
		if err := j.assertBackupPossible(ctx, current, job); err != nil {
			return nil, err
		}
		if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
			return nil, err
		}
		defer func() {
			background := context.WithoutCancel(ctx)
			// Starting can update the game and change the world, so start only
			// after a completed backup and once the data helper is removed.
			if j.backupCompleted(background, job) && job.Job.Recovery.DataHelperID == "" {
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
			if err := job.Progress("backup_complete", func(r *ops.Recovery) { r.BackupID = backup.ID }, nil); err != nil {
				return nil, err
			}
			// One stopped outside Ludock after the check above stays stopped,
			// as its owner left it.
			if initial := job.Job.Recovery.InitialRunning; initial == nil || !*initial {
				return withSave(job, map[string]any{"action": "restart", "backupId": backup.ID, "guidance": stoppedElsewhere}), nil
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
			if err := job.Progress("starting", func(r *ops.Recovery) { r.StartRequested = true }, nil); err != nil {
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
			if err := job.Progress("started", func(r *ops.Recovery) { r.StateRestored = true }, nil); err != nil {
				return nil, err
			}
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, false))
			return withSave(job, map[string]any{"action": "restart", "backupId": job.Job.Recovery.BackupID}), nil
		}()
		// Work cut by shutdown is recovered, and reported, on the next start.
		if err != nil && ctx.Err() == nil {
			// A recorded stop may have reached Docker even if its result was
			// lost; reconcile it before reporting the server left stopped.
			if job.Job.Recovery.InitialRunning != nil && !j.backupCompleted(ctx, job) {
				if keepErr := j.keepStoppedAfterFailedBackup(context.WithoutCancel(ctx), current, job); keepErr != nil {
					log.Warn("Could not confirm the server stayed stopped after a failed backup; review its state")
				}
			}
		}
		return result, err
	})
}

// recoverBackupRestart returns the server to running, as the restart was
// asked to. Ludock restarting is not a failed backup, so a server stopped
// for one is started again even if the backup didn't complete, and the
// backup is not taken again.
func (j *Jobs) recoverBackupRestart(ctx context.Context, job *ops.Context) (map[string]any, error) {
	completed := j.backupCompleted(ctx, job)
	if !completed {
		defer j.notifyInterrupted(ctx, job, notify.FailedWork, "backup-failed:"+job.Job.ID, backupInterrupted)
	}
	// Cut before the stop, such as while the world saved: nothing changed,
	// but a check before the stop may have left its helper.
	if job.Job.Recovery.InitialRunning == nil {
		return nil, j.removeOperationHelpers(ctx, job)
	}
	if err := j.recoverBackup(ctx, job, true); err != nil {
		return nil, err
	}
	result := map[string]any{"action": "restart"}
	running := *job.Job.Recovery.InitialRunning
	switch {
	case completed && !running:
		result["guidance"] = stoppedElsewhere
	case !completed && running:
		result["guidance"] = "Ludock restarted before the backup finished, so there’s no new backup. The server was started again."
	case !completed:
		result["guidance"] = "Ludock restarted before the backup finished, so there’s no new backup. Something outside Ludock had stopped the server before the backup, so Ludock didn’t start it."
	}
	if completed {
		result["backupId"] = job.Job.Recovery.BackupID
	}
	return withSave(job, result), nil
}

const stoppedElsewhere = "Something outside Ludock stopped the server before the restart did, so Ludock backed it up and left it stopped."

// recoverUpdate reconciles an interrupted update. Once it began swapping
// containers, the swap is settled first: undone before the original's
// removal began, finished after. An undone swap leaves the original stopped
// if the replacement may have run, since the new game version may have
// changed the world.
func (j *Jobs) recoverUpdate(ctx context.Context, job *ops.Context) (map[string]any, error) {
	swap, err := savedSwap(job)
	if err != nil {
		return nil, err
	}
	if swap != nil {
		release, err := j.Locks.AcquireForRecovery(recoveryLocks(job))
		if err != nil {
			return nil, err
		}
		err = j.settle(ctx, job)
		var started error
		if err == nil && swap.reached(swapReplacing) {
			// Recovery runs without RideOut; an outage is waited out before
			// the replacement is judged either way.
			started = j.startReplacement(docker.RideOut(ctx, nil), job)
		}
		release()
		if err != nil {
			return nil, err
		}
		// Ludock stopping mid-check judged nothing: the next start does.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if swap.reached(swapReplacing) {
			return j.finishedUpdate(ctx, job, started), nil
		}
	}
	// Pulling changes no state. Backup recovery applies only after the
	// initial state and binding were recorded before stopping.
	initial := job.Job.Recovery.InitialRunning
	if initial == nil {
		return map[string]any{"guidance": "Ludock restarted before the update changed anything, so the update didn’t happen."}, j.removeOperationHelpers(ctx, job)
	}
	if swap != nil && swap.reached(swapStarting) {
		err := j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
			return j.keepStopped(ctx, server, job, notify.Results, failedReplacement)
		})
		if err != nil {
			return nil, err
		}
		if info, err := j.Docker.Inspect(ctx, job.Job.Recovery.ContainerID); err == nil && info.State.Running {
			return map[string]any{"guidance": "Ludock restarted while it checked the updated container, so the update was undone and the previous container kept. " +
				"Something outside Ludock started it again meanwhile."}, nil
		}
		return map[string]any{"guidance": "Ludock restarted while it checked the updated container, so the update was undone and the previous container kept. " +
			"The server was left stopped, since the new version may already have run and changed the game data. Check the server, then start it."}, nil
	}
	// Ludock restarting is not a failed update: the server is returned to
	// how it was.
	if err := j.recoverBackup(ctx, job, true); err != nil {
		return nil, err
	}
	if *initial {
		return withSave(job, map[string]any{"guidance": "Ludock restarted during the update, so the update didn’t happen. The server was started again as it was."}), nil
	}
	return withSave(job, map[string]any{"guidance": "Ludock restarted during the update, so the update didn’t happen. The server stays stopped, as it was."}), nil
}

// finishedUpdate reports an update recovery finished, from the
// replacement's state that startReplacement confirmed.
func (j *Jobs) finishedUpdate(ctx context.Context, job *ops.Context, started error) map[string]any {
	finished := "Ludock restarted while the updated container replaced the previous one, so it finished the update."
	alert := func(message string) {
		j.notify(notify.Results, "operation:"+job.Job.ID, "An update finished after Ludock restarted, but "+message)
	}
	switch {
	case started == nil:
		// A server that was stopped stays stopped on purpose, as when the
		// update finishes without a restart.
		running := job.Job.Recovery.InitialRunning != nil && *job.Job.Recovery.InitialRunning
		j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, job.Job.ServerID, !running))
		return map[string]any{"guidance": finished}
	case apperr.Is(started, errReplacementNotStarted.Code):
		j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, job.Job.ServerID, true))
		alert("the server’s new container is stopped and couldn’t be started. Check it, then start it.")
		return map[string]any{"guidance": finished + " Its new container had stopped, and Docker couldn’t start it again, so the server is stopped. Check it, then start it."}
	case apperr.Is(started, errReplacementDown.Code):
		j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, job.Job.ServerID, true))
		alert("the server’s new container is stopped. Check its logs, then start it.")
		return map[string]any{"guidance": finished + " Its new container is stopped, so the server is stopped. Check its logs, then start it."}
	case apperr.Is(started, errReplacementRestarted.Code):
		j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, job.Job.ServerID, false))
		alert("the server’s new container restarted right after it started. Check its logs.")
		return map[string]any{"guidance": finished + " Its new container restarted right after it started. Check its logs."}
	default:
		// Not known to be stopped: outage tracking reports it if it is.
		j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, job.Job.ServerID, false))
		check := "check"
		if docker.Unavailable(started) {
			check = "reach Docker to check"
		}
		alert("Ludock couldn’t " + check + " that the server is running. Check it.")
		return map[string]any{"guidance": finished + " Ludock couldn’t " + check + " that its new container is running. Check the server."}
	}
}

const failedReplacement = "the update did not finish after its new container started, which may have changed the game data, " +
	"so the previous container was restored and left stopped. Check its Activity, then start it."

// Stages of a swap, in order. Each is saved before the step it names, so
// recovery knows how far Docker may have got.
const (
	swapCreating  = "creating"  // the replacement may exist
	swapStarting  = "starting"  // the replacement may have run
	swapReplacing = "replacing" // the original may be partly removed; only forward from here
	swapReplaced  = "replaced"
)

var swapStages = []string{swapCreating, swapStarting, swapReplacing, swapReplaced}

type swapState struct {
	Stage string `json:"stage"`
	// Name is the original's name, which the replacement takes.
	Name string `json:"name"`
	// Temporary is the replacement's name until then.
	Temporary string `json:"temporary"`
	// Replacement is the replacement's ID once Docker returned it.
	Replacement   string          `json:"replacement,omitempty"`
	RestartPolicy json.RawMessage `json:"restartPolicy,omitempty"`
	// VerifiedRun is the start time of the replacement's run that was seen
	// to last before it took over.
	VerifiedRun string `json:"verifiedRun,omitempty"`
}

func (s *swapState) reached(stage string) bool {
	return slices.Index(swapStages, s.Stage) >= slices.Index(swapStages, stage)
}

// savedSwap is the update's saved swap, or nil before it began.
func savedSwap(job *ops.Context) (*swapState, error) {
	if len(job.Job.Recovery.Swap) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytesReader(job.Job.Recovery.Swap))
	decoder.DisallowUnknownFields()
	var swap swapState
	if decoder.Decode(&swap) != nil || !slices.Contains(swapStages, swap.Stage) || swap.Name == "" || swap.Temporary == "" {
		return nil, errUpdateState
	}
	return &swap, nil
}

func saveSwap(job *ops.Context, swap swapState) error {
	encoded, err := json.Marshal(swap)
	if err != nil {
		return err
	}
	return job.Progress(swap.Stage, func(r *ops.Recovery) { r.Swap = encoded }, nil)
}

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

// noRestart is the restart policy both containers have during a swap.
var noRestart = json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`)

var (
	errPull = apperr.New("PULL_FAILED", 409,
		"Docker couldn’t download the server’s new image. Check the machine’s internet connection and the image name; for a private registry, set LUDOCK_DOCKER_CONFIG.")
	errCreate = apperr.New("CREATE_FAILED", 409,
		"Docker could not create the updated container. The server stays on its previous container.")
	errStart = apperr.New("START_FAILED", 409,
		"Docker could not start the updated container, for example because a port is in use. The previous container is restored and left stopped.")
	errReplacementStopped = apperr.New("REPLACEMENT_FAILED", 409,
		"The updated container stopped or restarted right after starting. The previous container is restored and left stopped, since the new version may have changed the game data.")
	errReplacementGone = apperr.New("REPLACEMENT_MISSING", 409,
		"The updated container was removed outside Ludock while it replaced the previous one. The previous container was kept; review it before starting it.")
	errOriginalGone = apperr.New("ORIGINAL_MISSING", 409,
		"The previous container was removed outside Ludock during the update, so the unchecked updated container was removed too. Recreate the server through the tool that created it.")
	errServerGone = apperr.New("REPLACEMENT_MISSING", 409,
		"Both the updated and the previous container were removed outside Ludock. Recreate the server through the tool that created it.")
	errPolicyNotRestored = apperr.New("RESTART_POLICY_NOT_RESTORED", 409,
		"The update was undone, but Docker refused to restore the previous container's restart policy. Set it again in Docker.")
	errUpdateState        = apperr.New("INVALID_OPERATION_STATE", 409, "Saved update state is invalid; administrator review is required.")
	errUnnamedReplacement = apperr.New("REPLACEMENT_UNNAMED", 409,
		"The updated container replaced the previous one but could not take its name and restart policy. Rename it and set its restart policy in Docker.")
	errImageChanged         = apperr.New("IMAGE_CHANGED", 409, "Something outside Ludock changed the server’s image during the update. Check the server before trying again.")
	errReplacementUnhealthy = apperr.New("REPLACEMENT_UNHEALTHY", 409,
		"The updated container reported itself unhealthy. The previous container is restored and left stopped, since the new version may have changed the game data.")
)

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
		keys = append(keys, locks.StorageKey)
	}
	return job.WithLocks(keys, func() (result map[string]any, err error) {
		plan, err := j.Updates.Check(ctx, server)
		if err != nil {
			return nil, err
		}
		defer func() {
			// A cancelled update never touched the server, so outage tracking stays as it was.
			if !errors.Is(err, ops.ErrCancelled) {
				j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), server.Logical.ID))
			}
		}()
		result, err = j.update(ctx, job, server, request, capability, plan)
		// Cancelled while it warned players: nothing was stopped or changed.
		if err != nil && !errors.Is(err, ops.ErrCancelled) {
			if undoErr := j.undoUpdate(context.WithoutCancel(ctx), server, job); undoErr != nil {
				if err := job.Progress("recovery_required", nil, nil); err != nil {
					log.Warn("Could not save that an update needs recovery; the failed operation still reports it")
				}
			}
			if ctx.Err() == nil {
				j.notify(notify.Results, "operation:"+job.Job.ID, server.Container.DisplayName+": update failed. Its Activity says why; check the server before starting it.")
			}
		}
		return result, err
	})
}

// undoUpdate returns a failed update's server to its original container,
// in its initial state unless the replacement may have run, or, once the
// original's removal began, finishes the swap.
func (j *Jobs) undoUpdate(ctx context.Context, server *servers.Context, job *ops.Context) error {
	swap, err := savedSwap(job)
	if err != nil {
		return err
	}
	if swap != nil {
		if err := j.settle(ctx, job); err != nil || swap.reached(swapReplacing) {
			if err == nil {
				err = j.startReplacement(ctx, job)
			}
			return err
		}
		if swap.reached(swapStarting) {
			return j.keepStopped(ctx, server, job, notify.Results, failedReplacement)
		}
	}
	return j.restoreInitialState(ctx, server, job)
}

// update pulls the image, then builds and checks the replacement under a
// temporary name that discovery ignores, while the stopped original stays
// the server. Only a replacement that started cleanly replaces it.
func (j *Jobs) update(ctx context.Context, job *ops.Context, server *servers.Context, request UpdateRequest, capability authz.Capability,
	plan *update.Plan) (map[string]any, error) {
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return nil, err
	}
	if err := warnedTooLate(job); err != nil {
		return nil, err
	}
	if err := job.Progress("pulling", func(r *ops.Recovery) { r.ContainerID = server.Container.ID }, nil); err != nil {
		return nil, err
	}
	auth, err := j.Updates.RegistryAuth(plan)
	if err != nil {
		return nil, err
	}
	// Pulling stops as soon as the owner loses update authority.
	if err := j.whileAuthorized(ctx, job, capability, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		if err := j.Docker.Pull(ctx, plan.Image, auth, plan.Platform()); err != nil {
			return errPull
		}
		return nil
	}); err != nil {
		return nil, err
	}
	image, err := j.Docker.InspectImageConfig(ctx, plan.Image)
	if err != nil {
		return nil, err
	}
	if j.Pulled != nil {
		defer j.Pulled(server.Logical.ID)
	}
	pulled := image.ID
	imageChanged := pulled != plan.Spec.Image
	if !imageChanged && !request.ForceRecreate {
		return map[string]any{"alreadyCurrent": true, "imageChanged": false, "recreated": false}, nil
	}
	if _, err := j.resolve(ctx, job, capability, true); err != nil {
		return nil, err
	}
	if err := job.Progress("validating", nil, nil); err != nil {
		return nil, err
	}
	if !request.CreateBackup && request.SkipBackupConfirmation != server.Container.DisplayName {
		return nil, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm skipping the backup")
	}
	if request.CreateBackup {
		if err := j.assertBackupPossible(ctx, server, job); err != nil {
			return nil, err
		}
	}
	// Warned only now, once the update is sure to replace the server.
	if err := j.warnFirst(ctx, job, capability, true); err != nil {
		return nil, err
	}
	if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
		return nil, err
	}
	if err := job.Progress("stopping", nil, nil); err != nil {
		return nil, err
	}
	// A forced stop only blocks a backup; replacing the container is what
	// the stop was for.
	if err := j.stopForData(ctx, server, job); err != nil && (request.CreateBackup || !apperr.Is(err, "UNCLEAN_SHUTDOWN")) {
		return nil, err
	}
	// The server returns to how it was when it was stopped, not when the
	// update was asked for: its owner may have started or stopped it during
	// the pull.
	initiallyRunning := job.Job.Recovery.InitialRunning != nil && *job.Job.Recovery.InitialRunning
	if request.CreateBackup {
		if err := job.Progress("backing_up", nil, nil); err != nil {
			return nil, err
		}
		stopped := *server
		stopped.Container.State = "exited"
		backup, err := j.createStoppedBackup(ctx, &stopped, job)
		if err != nil {
			return nil, err
		}
		if err := job.Progress("validating", func(r *ops.Recovery) { r.BackupID = backup.ID }, nil); err != nil {
			return nil, err
		}
	}
	// Check the stopped container again: another manager may have changed
	// it, the image tag, or the containers that depend on it.
	if _, err := j.resolve(ctx, job, capability, true); err != nil {
		return nil, err
	}
	stopped, err := j.Updates.Recheck(ctx, server, plan)
	if err != nil {
		return nil, err
	}
	if again, err := j.Docker.InspectImage(ctx, plan.Image); err != nil || again != pulled {
		return nil, errImageChanged
	}
	body, err := stopped.Replacement(image, job.Job.ID)
	if err != nil {
		return nil, err
	}
	verification, err := j.swap(ctx, job, server, capability, stopped, body, pulled, initiallyRunning)
	if err != nil {
		return nil, err
	}
	if err := j.startReplacement(ctx, job); err != nil {
		if apperr.Is(err, errReplacementDown.Code) || apperr.Is(err, errReplacementNotStarted.Code) {
			j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, true))
		}
		return nil, err
	}
	// The update is complete; discovery binds the server to the replacement
	// on its next pass if this one fails.
	if _, err := j.Servers.RefreshNow(ctx); err != nil {
		log.Warn("Could not refresh servers after an update")
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, !initiallyRunning))
	result := map[string]any{"imageChanged": imageChanged, "recreated": true, "forced": request.ForceRecreate,
		"running": initiallyRunning, "verification": verification}
	if stopped.NamesUnnamedVolumes() {
		result["guidance"] = unnamedVolumeGuidance
	}
	return withSave(job, result), nil
}

// Docker's rm -v, which compose down -v relies on, removes only volumes a
// container got without naming them, and the Engine API hands a new container
// an existing volume only by name. VolumesFrom would keep it unnamed, but
// leaves a reference to the removed original that breaks the next recreation
// by Portainer, Watchtower, or Ludock.
const unnamedVolumeGuidance = "The server kept its unnamed volume. As after Compose recreates a container, docker compose down -v " +
	"now leaves that volume behind; remove it with docker volume rm. To name it in the Compose file instead, copy its data into the named volume first."

// swap replaces the stopped original with a container created from body, which
// must run the pulled image, and returns how the replacement was checked.
func (j *Jobs) swap(ctx context.Context, job *ops.Context, server *servers.Context, capability authz.Capability, stopped *update.Plan,
	body map[string]any, pulled string, initiallyRunning bool) (string, error) {
	// Until the swap completes, neither container may restart on its own: a
	// Docker restart would otherwise start both against the same data. The
	// replacement gets the original's policy when it takes over.
	host, _ := body["HostConfig"].(map[string]json.RawMessage)
	if host == nil {
		host = map[string]json.RawMessage{}
		body["HostConfig"] = host
	}
	host["RestartPolicy"] = noRestart
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return "", err
	}
	name := strings.TrimPrefix(stopped.Spec.Name, "/")
	state := swapState{Stage: swapCreating, Name: name, Temporary: servers.ReplacementName(name, job.Job.ID),
		RestartPolicy: stopped.RestartPolicy()}
	if err := saveSwap(job, state); err != nil {
		return "", err
	}
	if err := j.Docker.SetRestartPolicy(ctx, server.Container.ID, noRestart); err != nil {
		return "", err
	}
	replacement, err := j.Docker.Create(ctx, state.Temporary, stopped.Platform(), body)
	if err != nil {
		return "", errCreate
	}
	state.Replacement = replacement
	if err := saveSwap(job, state); err != nil {
		return "", err
	}
	// Create resolves the tag again; another manager may have moved it.
	if created, err := j.Docker.Inspect(ctx, replacement); err != nil || created.Image != pulled {
		return "", errImageChanged
	}
	verification := "created"
	if initiallyRunning {
		var since time.Time
		for {
			// The owner may have lost authority while Docker created the
			// container, and another manager may have started the original.
			if err := j.assertCapability(ctx, job, capability); err != nil {
				return "", err
			}
			// The checks hold only together: after waiting for Docker, another
			// manager may have started the original, so all are made again.
			if err := j.assertStopped(docker.Promptly(ctx), server, job); err != nil {
				if docker.Unavailable(err) && j.Docker.WaitForDocker(ctx, err, &since) {
					continue
				}
				return "", err
			}
			state.Stage = swapStarting
			if err := saveSwap(job, state); err != nil {
				return "", err
			}
			err := j.Docker.Start(ctx, replacement)
			if err == nil {
				break
			}
			// A start that never reached Docker is checked and sent again.
			if !docker.NotSent(err) || !j.Docker.WaitForDocker(ctx, err, &since) {
				return "", errStart
			}
		}
		if verification, state.VerifiedRun, err = j.awaitReplacement(ctx, job, capability, replacement); err != nil {
			return "", err
		}
	}
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return "", err
	}
	if err := j.assertStillStopped(ctx, server, job); err != nil {
		return "", err
	}
	state.Stage = swapReplacing
	if err := saveSwap(job, state); err != nil {
		return "", err
	}
	return verification, j.settle(ctx, job)
}

// awaitReplacement waits until a started replacement is healthy or, without a
// healthcheck, still running after the settle time. It fails as soon as the
// replacement stops, restarts, or turns unhealthy. A healthcheck still starting
// after three minutes is accepted, since a game server can take that long to
// download its files.
func (j *Jobs) awaitReplacement(ctx context.Context, job *ops.Context, capability authz.Capability, id string) (verification, run string, err error) {
	settle := j.replacementSettle()
	began := time.Now()
	startedAt := ""
	for {
		info, err := j.Docker.Inspect(ctx, id)
		if err != nil {
			return "", "", err
		}
		if startedAt == "" {
			startedAt = info.State.StartedAt
		}
		if !info.State.Running || info.State.Restarting || info.State.StartedAt != startedAt {
			return "", "", errReplacementStopped
		}
		if health := info.State.Health; health != nil && health.Status != "" && health.Status != "none" {
			switch {
			case health.Status == "healthy":
				return "docker-health", startedAt, nil
			case health.Status == "unhealthy":
				return "", "", errReplacementUnhealthy
			case time.Since(began) >= 3*time.Minute:
				return "health-pending", startedAt, nil
			}
		} else if time.Since(began) >= settle {
			return "docker-state", startedAt, nil
		}
		if err := j.assertCapability(ctx, job, capability); err != nil {
			return "", "", err
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(j.poll()):
		}
	}
}

// settle settles an update's swap, retrying through Docker failures: until
// the original is gone or the replacement is removed, the replacement may
// still be writing, so the locks stay held. On shutdown the operation stays
// running for the next startup.
func (j *Jobs) settle(ctx context.Context, job *ops.Context) error {
	for {
		err := j.settleSwap(ctx, job)
		if _, public := apperr.As(err); err == nil || public {
			return err
		}
		log.Warn("Could not reconcile an update's containers yet; retrying")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(j.poll()):
		}
	}
}

// settleSwap undoes a swap until the original's removal began, and finishes
// it after. Undoing removes the replacement and restores the original's
// restart policy; finishing removes the original and gives the replacement
// its name and restart policy.
func (j *Jobs) settleSwap(ctx context.Context, job *ops.Context) error {
	swap, err := savedSwap(job)
	if err != nil || swap == nil || swap.Stage == swapReplaced {
		return err
	}
	original := job.Job.Recovery.ContainerID
	policy := swap.RestartPolicy
	if len(policy) == 0 {
		policy = noRestart
	}
	replacement, err := j.ownedReplacement(ctx, job, swap)
	if err != nil {
		return err
	}
	if !swap.reached(swapReplacing) {
		if replacement != "" {
			if err := j.removeUntilGone(ctx, replacement); err != nil {
				return err
			}
		}
		if _, err := j.Docker.Inspect(ctx, original); docker.IsNotFound(err) {
			return errOriginalGone
		} else if err != nil {
			return err
		}
		// Nothing writes any more; a policy Docker refuses, as for a dead
		// container, is reported instead of retried.
		if err := j.Docker.SetRestartPolicy(ctx, original, policy); err != nil {
			return errPolicyNotRestored
		}
		return nil
	}
	if replacement == "" {
		// Another manager removed the replacement: whatever remains of the
		// original is all that is left of the server.
		if _, err := j.Docker.Inspect(ctx, original); err == nil || !docker.IsNotFound(err) {
			return errReplacementGone
		}
		return errServerGone
	}
	// A removal that failed or lost its response can leave the original
	// dead or still being removed, so it is finished, never undone.
	if err := j.removeUntilGone(ctx, original); err != nil {
		return err
	}
	if err := j.Docker.SetRestartPolicy(ctx, replacement, policy); err != nil {
		return errUnnamedReplacement
	}
	current, err := j.Docker.Inspect(ctx, replacement)
	if err != nil {
		return errUnnamedReplacement
	}
	if current.Name != "/"+swap.Name {
		if err := j.Docker.Rename(ctx, replacement, swap.Name); err != nil {
			return errUnnamedReplacement
		}
	}
	swap.Stage, swap.Replacement = swapReplaced, replacement
	return saveSwap(job, *swap)
}

var (
	errReplacementNotStarted = apperr.New("REPLACEMENT_NOT_STARTED", 409,
		"The server was updated, but its new container stopped before the update finished, as when Docker restarts, and Docker couldn’t start it again. Check the server, then start it.")
	errReplacementDown = apperr.New("REPLACEMENT_DOWN", 409,
		"The server was updated, but its new container is stopped. Check its logs, then start it.")
	errReplacementRestarted = apperr.New("REPLACEMENT_RESTARTED", 409,
		"The server was updated, but its new container restarted right after it started. Check its logs.")
)

// startReplacement starts the replacement of a server that was running once
// it took over: during the swap neither container may restart on its own,
// so one Docker stopped meanwhile stays stopped. A start that reached Docker
// is never repeated, and the new run must last the settle time. It returns
// nil only when the replacement is running, or the server was stopped.
func (j *Jobs) startReplacement(ctx context.Context, job *ops.Context) error {
	recovery := job.Job.Recovery
	swap, err := savedSwap(job)
	if err != nil || swap == nil || swap.Stage != swapReplaced || recovery.InitialRunning == nil || !*recovery.InitialRunning {
		return err
	}
	info, err := j.Docker.Inspect(ctx, swap.Replacement)
	if docker.IsNotFound(err) {
		return errReplacementNotStarted
	}
	if err != nil {
		return err
	}
	if info.State.Running || info.State.Restarting {
		if recovery.StateRestored {
			return nil
		}
		// A run not yet seen to last, started by Ludock or by Docker's own
		// restart policy, is watched; the one verified before the swap isn't
		// watched twice.
		if info.State.StartedAt != swap.VerifiedRun || info.State.Restarting {
			if err := j.survives(ctx, swap.Replacement); err != nil {
				return err
			}
		}
		return job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.StateRestored = true }, nil)
	}
	if recovery.StateRestored || recovery.StartRequested {
		return errReplacementDown
	}
	// The replacement is started only as the server it became: discovery
	// binds it, and the start confirms that binding.
	if _, err := j.Servers.RefreshNow(ctx); err != nil {
		if docker.Unavailable(err) {
			return err
		}
		return errReplacementNotStarted
	}
	logical, err := j.Identity.Get(ctx, job.Job.ServerID)
	if err != nil || logical == nil || logical.ContainerID != swap.Replacement {
		return errReplacementNotStarted
	}
	// A lost answer, or Docker's 304 for a started container, is judged by
	// what Docker shows, never by starting again. The marker is saved once
	// the start is about to be sent.
	if err := j.Servers.Observer.ChangeState(ctx, swap.Replacement, servers.Start, func(observation identity.Observation, _ *docker.ContainerInspect) error {
		if _, err := j.Identity.AssertObservedBinding(ctx, logical.ID, observation, logical.BindingRevision); err != nil {
			return err
		}
		return job.Progress("restarting", func(r *ops.Recovery) { r.StartRequested = true }, nil)
	}); err != nil {
		info, inspectErr := j.Docker.Inspect(ctx, swap.Replacement)
		if inspectErr != nil && !docker.IsNotFound(inspectErr) {
			return inspectErr
		}
		if inspectErr != nil || !info.State.Running {
			return errReplacementNotStarted
		}
	}
	if err := j.survives(ctx, swap.Replacement); err != nil {
		return err
	}
	return job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.StateRestored = true }, nil)
}

// survives confirms a started container keeps its run through the settle
// time, as a replacement is checked before it takes over. One that ended
// its run but runs again, or is restarting, is not called stopped.
func (j *Jobs) survives(ctx context.Context, id string) error {
	var began time.Time
	startedAt := ""
	for {
		info, err := j.Docker.Inspect(ctx, id)
		if docker.IsNotFound(err) {
			return errReplacementDown
		}
		if err != nil {
			return err
		}
		if !info.State.Running && !info.State.Restarting {
			return errReplacementDown
		}
		if startedAt == "" {
			startedAt, began = info.State.StartedAt, time.Now()
		}
		if info.State.Restarting || info.State.StartedAt != startedAt {
			return errReplacementRestarted
		}
		if time.Since(began) >= j.replacementSettle() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(j.poll()):
		}
	}
}

func (j *Jobs) replacementSettle() time.Duration {
	if j.ReplacementSettle == 0 {
		return 10 * time.Second
	}
	return j.ReplacementSettle
}

// ownedReplacement is the ID of the swap's replacement, or "". Without a saved
// ID, a container under the temporary name counts only if it carries this
// operation's label.
func (j *Jobs) ownedReplacement(ctx context.Context, job *ops.Context, swap *swapState) (string, error) {
	id := swap.Replacement
	if id == "" {
		spec, err := j.Docker.InspectSpec(ctx, swap.Temporary)
		if docker.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		var config struct{ Labels map[string]string }
		encoded, _ := json.Marshal(spec.Config)
		if json.Unmarshal(encoded, &config) != nil || config.Labels[update.OperationLabel] != job.Job.ID {
			return "", nil
		}
		id = spec.ID
	}
	if id == job.Job.Recovery.ContainerID {
		return "", errUpdateState
	}
	if _, err := j.Docker.Inspect(ctx, id); docker.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return id, nil
}

// removeUntilGone retries until Docker confirms the container is gone.
func (j *Jobs) removeUntilGone(ctx context.Context, id string) error {
	for {
		err := j.Docker.Remove(ctx, id, true, false)
		if err == nil || docker.IsNotFound(err) {
			if _, err := j.Docker.Inspect(ctx, id); docker.IsNotFound(err) {
				return nil
			}
		}
		log.Warn("Could not remove a container an update replaces yet; retrying")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(j.poll()):
		}
	}
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
