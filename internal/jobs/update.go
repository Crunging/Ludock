package jobs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/identity"
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

// recoverUpdate reconciles an interrupted update. Once it began swapping
// containers, the swap is settled first: undone before the original's
// removal began, finished after. An undone swap leaves the original stopped
// if the replacement may have run, since the new game version may have
// changed the world.
func (j *Jobs) recoverUpdate(ctx context.Context, job *ops.Context) error {
	swap, err := savedSwap(job)
	if err != nil {
		return err
	}
	if swap != nil {
		held, err := recoveryLocks(job)
		if err != nil {
			return err
		}
		release, err := j.Locks.AcquireForRecovery(held)
		if err != nil {
			return err
		}
		err = j.settle(ctx, job)
		release()
		if err != nil || swap.reached(swapReplacing) {
			return err
		}
	}
	// Pulling changes no state. Backup recovery applies only after the
	// initial state and binding were recorded before stopping.
	if _, recorded := job.Job.Recovery["initialRunning"].(bool); !recorded {
		return nil
	}
	if swap != nil && swap.reached(swapStarting) {
		return j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
			return j.keepStopped(ctx, server, job, failedReplacement)
		})
	}
	return j.recoverBackup(ctx, job, true)
}

const failedReplacement = "the update did not finish after its new container started, which may have changed the game data, " +
	"so the previous container was restored and left stopped. Review the operation, then start it."

// Stages of a swap, in order. Each is saved before the step it names, so
// recovery knows how far Docker may have got.
const (
	swapCreating  = "creating"  // the replacement may exist
	swapStarting  = "starting"  // the replacement may have run
	swapReplacing = "replacing" // the original may be partly removed; only forward from here
	swapReplaced  = "replaced"
)

var swapStages = []string{swapCreating, swapStarting, swapReplacing, swapReplaced}

// swapState is the saved progress of replacing a server's container.
type swapState struct {
	Stage string `json:"stage"`
	// Name is the original's name, which the replacement takes.
	Name string `json:"name"`
	// Temporary is the replacement's name until then.
	Temporary string `json:"temporary"`
	// Replacement is the replacement's ID once Docker returned it.
	Replacement   string          `json:"replacement,omitempty"`
	RestartPolicy json.RawMessage `json:"restartPolicy,omitempty"`
}

func (s *swapState) reached(stage string) bool {
	return slices.Index(swapStages, s.Stage) >= slices.Index(swapStages, stage)
}

// savedSwap is the update's saved swap, or nil before it began.
func savedSwap(job *ops.Context) (*swapState, error) {
	value, present := job.Job.Recovery["swap"]
	if !present {
		return nil, nil
	}
	encoded, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytesReader(encoded))
	decoder.DisallowUnknownFields()
	var swap swapState
	if decoder.Decode(&swap) != nil || !slices.Contains(swapStages, swap.Stage) || swap.Name == "" || swap.Temporary == "" {
		return nil, errUpdateState
	}
	return &swap, nil
}

func saveSwap(job *ops.Context, swap swapState) error {
	return job.Progress(swap.Stage, map[string]any{"swap": swap}, nil)
}

// UpdateRequest is a confirmed image update.
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
		"Docker could not pull the server's image. Check the image name and the registry; for a private registry, set LUDOCK_DOCKER_CONFIG.")
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
	errImageChanged         = apperr.New("IMAGE_CHANGED", 409, "Another manager changed the selected image during the update")
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
		keys = append(keys, "backups:storage")
	}
	return job.WithLocks(keys, func() (result map[string]any, err error) {
		plan, err := j.Updates.Check(ctx, server)
		if err != nil {
			return nil, err
		}
		defer func() { j.noteAvailability(j.Monitor.Suppress(context.WithoutCancel(ctx), server.Logical.ID)) }()
		result, err = j.update(ctx, job, server, request, capability, plan)
		if err != nil {
			if undoErr := j.undoUpdate(context.WithoutCancel(ctx), server, job); undoErr != nil {
				if err := job.Progress("recovery_required", map[string]any{"recoveryRequired": true}, nil); err != nil {
					log.Warn("Could not save that an update needs recovery; the failed operation still reports it")
				}
			}
			j.notify("operation:"+job.Job.ID, server.Container.DisplayName+": update failed. Review the operation and actual server state.")
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
			return err
		}
		if swap.reached(swapStarting) {
			return j.keepStopped(ctx, server, job, failedReplacement)
		}
	}
	return j.restoreInitialState(ctx, server, job)
}

// update pulls the image, then builds and checks the replacement under a
// temporary name that discovery ignores, while the stopped original stays
// the server. Only a replacement that started cleanly replaces it.
func (j *Jobs) update(ctx context.Context, job *ops.Context, server *servers.Context, request UpdateRequest, capability authz.Capability,
	plan *update.Plan) (map[string]any, error) {
	initiallyRunning := plan.Spec.State.Running
	if err := j.assertCapability(ctx, job, capability); err != nil {
		return nil, err
	}
	if err := job.Progress("pulling", map[string]any{"containerId": server.Container.ID, "initiallyRunning": initiallyRunning,
		"previousImageId": plan.Spec.Image}, nil); err != nil {
		return nil, err
	}
	auth, err := j.Updates.RegistryAuth(plan)
	if err != nil {
		return nil, err
	}
	// Pulling stops as soon as the owner loses update authority, as
	// lifecycle and data jobs do.
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
	pulled := image.ID
	imageChanged := pulled != plan.Spec.Image
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
	if !request.CreateBackup && request.SkipBackupConfirmation != server.Container.DisplayName {
		return nil, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm skipping the backup")
	}
	if err := j.Monitor.Suppress(ctx, server.Logical.ID); err != nil {
		return nil, err
	}
	if err := job.Progress("stopping", map[string]any{"stopRequested": true}, nil); err != nil {
		return nil, err
	}
	// A forced stop only blocks a backup; replacing the container is what
	// the stop was for.
	if err := j.stopForData(ctx, server, job); err != nil && (request.CreateBackup || !apperr.Is(err, "UNCLEAN_SHUTDOWN")) {
		return nil, err
	}
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
		if err := job.Progress("validating", map[string]any{"backupId": backup.ID}, nil); err != nil {
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
	// The update is complete; discovery binds the server to the replacement
	// on its next pass if this one fails.
	if _, err := j.Servers.RefreshNow(ctx); err != nil {
		log.Warn("Could not refresh servers after an update")
	}
	j.noteAvailability(j.Monitor.SetIntentionalStop(ctx, server.Logical.ID, !initiallyRunning))
	action := "update"
	if request.ForceRecreate {
		action = "recreation"
	}
	j.notify("operation:"+job.Job.ID, server.Container.DisplayName+": "+action+" completed.")
	return map[string]any{"imageChanged": imageChanged, "recreated": true, "forced": request.ForceRecreate,
		"running": initiallyRunning, "verification": verification}, nil
}

// swap replaces the stopped original with a container created from body,
// which must run the pulled image. The replacement is created and checked
// under a temporary name discovery ignores, so the original stays the
// server until the replacement started cleanly. It returns how the
// replacement was checked.
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
		// The owner may have lost authority while Docker created the
		// container, and another manager may have started the original.
		if err := j.assertCapability(ctx, job, capability); err != nil {
			return "", err
		}
		if err := j.assertStopped(ctx, server, job); err != nil {
			return "", err
		}
		state.Stage = swapStarting
		if err := saveSwap(job, state); err != nil {
			return "", err
		}
		if err := j.Docker.Start(ctx, replacement); err != nil {
			return "", errStart
		}
		if verification, err = j.awaitReplacement(ctx, job, capability, replacement); err != nil {
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

// awaitReplacement waits for a started replacement to prove itself: healthy
// when it has a healthcheck, otherwise still running after the settle time.
// It fails as soon as the replacement stops, restarts, or turns unhealthy,
// while the original can still be restored. A healthcheck still starting
// after three minutes is accepted: a game server can take longer than that
// to download its files, and nothing has failed.
func (j *Jobs) awaitReplacement(ctx context.Context, job *ops.Context, capability authz.Capability, id string) (string, error) {
	settle := j.ReplacementSettle
	if settle == 0 {
		settle = 10 * time.Second
	}
	began := time.Now()
	startedAt := ""
	for {
		info, err := j.Docker.Inspect(ctx, id)
		if err != nil {
			return "", err
		}
		if startedAt == "" {
			startedAt = info.State.StartedAt
		}
		if !info.State.Running || info.State.Restarting || info.State.StartedAt != startedAt {
			return "", errReplacementStopped
		}
		if health := info.State.Health; health != nil && health.Status != "" && health.Status != "none" {
			switch {
			case health.Status == "healthy":
				return "docker-health", nil
			case health.Status == "unhealthy":
				return "", errReplacementUnhealthy
			case time.Since(began) >= 3*time.Minute:
				return "health-pending", nil
			}
		} else if time.Since(began) >= settle {
			return "docker-state", nil
		}
		if err := j.assertCapability(ctx, job, capability); err != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
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
	original, _ := text(job.Job.Recovery["containerId"])
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
	swap.Stage = swapReplaced
	return saveSwap(job, *swap)
}

// ownedReplacement is the ID of the swap's replacement, or "" if there is
// none. A create whose response was lost left no saved ID; a container under
// the temporary name is then this update's only if it carries this
// operation's label, so a container someone else gave that name is never
// removed or renamed.
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
	if original, _ := text(job.Job.Recovery["containerId"]); id == original {
		return "", errUpdateState
	}
	if _, err := j.Docker.Inspect(ctx, id); docker.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return id, nil
}

// removeUntilGone removes a container that may be running against the
// server's data, or that Docker may be removing already. Like an operation
// helper, it retries until Docker confirms the container is gone.
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
