package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var log = logging.New("self-update")

// HealthWait is how long the new version has to report healthy and ready.
const HealthWait = 3 * time.Minute

// Helper does one update's swap from inside its helper container. Docker
// runs it again after it is killed or the host restarts, and it keeps no
// state of its own: each pass reads what exists and finishes or undoes it.
// The marker containers (see Version) record what it decided: the
// original's name and policy before anything changes, that the replacement
// passed before the original is removed, and that the original goes back.
// It touches only containers it created or the one it was given.
type Helper struct {
	Docker *docker.Client
	// Container, Image, and Operation are the interface's arguments.
	Container, Image, Operation string
	Poll                        time.Duration
	HealthWait                  time.Duration
}

var saveDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

var (
	errBothGone     = errors.New("both Ludock containers are gone")
	errOriginalGone = errors.New("the previous container was removed outside Ludock while it was being put back")
	// Only one Ludock may run on the data: the next pass stops it again.
	errOriginalRunning = errors.New("the previous container was started again outside Ludock")
	errUnconfirmed     = errors.New("the previous container was removed outside Ludock before the new one was confirmed, and the new one doesn't work")
	errNameTaken       = errors.New("another container has taken Ludock's name")
	errNotReady        = errors.New("waiting for Ludock to report that it opened its data")
)

// Run settles the update, then removes the helper's own container. It
// returns only when that removal was sent or ctx ends.
func (h *Helper) Run(ctx context.Context) {
	for delay := h.poll(); ; delay = min(2*delay, 30*time.Second) {
		err := h.pass(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, errBothGone), errors.Is(err, errOriginalGone), errors.Is(err, errUnconfirmed), errors.Is(err, errNameTaken):
			log.Error("Ludock's update can't finish: " + err.Error() + ". Start Ludock again with docker compose up -d, then remove this helper")
			delay = time.Minute
		case errors.Is(err, errNotReady):
			log.Info("Waiting for Ludock to report that it opened its data; its log says why if it doesn't")
		default:
			log.Warn("Could not finish Ludock's update yet; retrying", logging.Cause(err, docker.StatusCode(err))...)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (h *Helper) poll() time.Duration {
	if h.Poll == 0 {
		return 2 * time.Second
	}
	return h.Poll
}

// marker is what the helper recorded before changing anything.
type marker struct {
	name   string
	policy json.RawMessage
	// save is the data save's SHA-256, which restore-data checks.
	save string
}

type undo struct {
	reason  string
	restore bool
}

func (h *Helper) pass(ctx context.Context) error {
	recorded, err := h.marker(ctx)
	if err != nil {
		return err
	}
	old, err := h.inspect(ctx, h.Container)
	if err != nil {
		return err
	}
	if old != nil && old.ID != h.Container {
		return errors.New("another container answers to the previous container's ID")
	}
	decided, err := h.undo(ctx)
	if err != nil {
		return err
	}
	healthy, err := h.healthy(ctx)
	if err != nil {
		return err
	}
	replacement, err := h.replacement(ctx, recorded.name)
	if err != nil {
		return err
	}
	if decided != nil {
		if old == nil {
			return errOriginalGone
		}
		return h.putBack(ctx, old, replacement, recorded, decided)
	}
	if old == nil {
		if replacement == nil {
			return errBothGone
		}
		if replacement.Image != h.Image {
			return errUnconfirmed
		}
		// Gone before the new one was confirmed: nothing is left to put
		// back, so the new one is checked as it would have been.
		if healthy != replacement.ID {
			if replacement.State.Status == "created" {
				if err := h.Docker.Start(ctx, replacement.ID); err != nil && docker.StatusCode(err) != 304 {
					return err
				}
			}
			reason, err := h.awaitHealthy(ctx, replacement.ID, "")
			if err != nil {
				return err
			}
			if reason != "" {
				return errUnconfirmed
			}
			if err := h.markHealthy(ctx, replacement.ID); err != nil {
				return err
			}
		}
		return h.finish(ctx, replacement.ID, recorded)
	}
	if replacement == nil {
		if old.Image == h.Image {
			return h.leave(ctx, old.ID, recorded.name)
		}
		id, reason, err := h.create(ctx, old)
		if err != nil {
			return err
		}
		if reason != "" {
			return h.decide(ctx, reason, !old.State.Running)
		}
		if replacement, err = h.inspect(ctx, id); err != nil || replacement == nil {
			return cmpErr(err, errors.New("the new container disappeared"))
		}
	}
	if replacement.Image != h.Image {
		return h.decide(ctx, ReasonImage, !old.State.Running)
	}
	if healthy != replacement.ID {
		switch {
		case replacement.State.Status == "created":
			if err := h.stopOriginal(ctx, old.ID); err != nil {
				return err
			}
			if err := h.Docker.Start(ctx, replacement.ID); err != nil && docker.StatusCode(err) != 304 {
				if docker.Unavailable(err) && docker.StatusCode(err) == 0 {
					return err
				}
				return h.decide(ctx, ReasonStart, true)
			}
		case old.State.Running:
			// Started again outside Ludock: only one may run on the data.
			if err := h.stopOriginal(ctx, old.ID); err != nil {
				return err
			}
		}
		reason, err := h.awaitHealthy(ctx, replacement.ID, old.ID)
		if err != nil {
			return err
		}
		if reason != "" {
			return h.decide(ctx, reason, true)
		}
		if err := h.stillStopped(ctx, old.ID); err != nil {
			return err
		}
		// From here the swap only goes forward.
		if err := h.markHealthy(ctx, replacement.ID); err != nil {
			return err
		}
	}
	if err := h.removeUntilGone(ctx, old.ID, false); err != nil {
		return err
	}
	return h.finish(ctx, replacement.ID, recorded)
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// ours reports whether spec is this update's container of kind.
func (h *Helper) ours(spec *docker.ContainerSpec, kind string) bool {
	found := labels(spec)
	return found[launch.InternalLabel] == kind && found[operationLabel] == h.Operation
}

// marked inspects this update's marker called name, of kind: nil when there
// is none, an error when another container has the name.
func (h *Helper) marked(ctx context.Context, name, kind string) (*docker.ContainerSpec, error) {
	spec, err := h.inspect(ctx, name)
	if err != nil || spec == nil {
		return nil, err
	}
	if !h.ours(spec, kind) {
		return nil, errors.New("another container has the name of one of the update's markers")
	}
	return spec, nil
}

// marker reads the recorded name and restart policy, recording them from
// the original first.
func (h *Helper) marker(ctx context.Context) (marker, error) {
	for {
		spec, err := h.marked(ctx, stateName(h.Operation), stateKind)
		if err != nil {
			return marker{}, err
		}
		if spec != nil {
			found := labels(spec)
			var policy struct{ Name string }
			if found[nameLabel] == "" || json.Unmarshal([]byte(found[policyLabel]), &policy) != nil ||
				!slices.Contains([]string{"", "no", "always", "unless-stopped", "on-failure"}, policy.Name) || !saveDigest.MatchString(found[saveLabel]) {
				return marker{}, errors.New("the update's marker is unreadable")
			}
			return marker{name: found[nameLabel], policy: json.RawMessage(found[policyLabel]), save: found[saveLabel]}, nil
		}
		// The digest Ludock gave this helper goes into the marker, which a
		// helper started again reads.
		self, err := h.Docker.InspectSpec(ctx, HelperName(h.Operation))
		if err != nil {
			return marker{}, err
		}
		save := labels(self)[saveLabel]
		if !h.ours(self, helperKind) || !saveDigest.MatchString(save) {
			return marker{}, errors.New("this helper wasn't given the update's data save")
		}
		original, err := h.Docker.InspectSpec(ctx, h.Container)
		if err != nil {
			return marker{}, err
		}
		if original.ID != h.Container {
			return marker{}, errors.New("another container answers to the previous container's ID")
		}
		record := map[string]string{nameLabel: strings.TrimPrefix(original.Name, "/"), policyLabel: string(original.HostConfig["RestartPolicy"]),
			saveLabel: save}
		if err := h.createMarker(ctx, stateName(h.Operation), stateKind, record); err != nil {
			return marker{}, err
		}
	}
}

// createMarker creates a never-started container from the helper's image
// that only holds labels. One already there is read by the next pass.
func (h *Helper) createMarker(ctx context.Context, name, kind string, record map[string]string) error {
	all := launch.HelperLabels(kind)
	all[operationLabel] = h.Operation
	maps.Copy(all, record)
	if _, err := h.Docker.Create(ctx, name, "", markerBody(h.Image, all)); err != nil && docker.StatusCode(err) != 409 {
		return err
	}
	return nil
}

// markerBody creates a container that is never started.
func markerBody(image string, labels map[string]string) map[string]any {
	return map[string]any{"Image": image, "Entrypoint": []string{helper.Binary}, "Cmd": []string{"version"}, "Labels": labels,
		"Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig":  map[string]any{"NetworkMode": "none"}}
}

func (h *Helper) undo(ctx context.Context) (*undo, error) {
	spec, err := h.marked(ctx, undoName(h.Operation), undoKind)
	if err != nil || spec == nil {
		return nil, err
	}
	restore, valid := restoreFlag(spec)
	if !valid {
		return nil, errors.New("the update's undo marker is unreadable")
	}
	return &undo{reason: labels(spec)[ReasonLabel], restore: restore}, nil
}

// restoreFlag is whether an undo marker restores the data, and whether it
// says so at all.
func restoreFlag(spec *docker.ContainerSpec) (restore, valid bool) {
	restore, err := strconv.ParseBool(labels(spec)[restoreLabel])
	return restore, err == nil && (labels(spec)[restoreLabel] == "true" || labels(spec)[restoreLabel] == "false")
}

// healthy is the replacement recorded as having passed, or "".
func (h *Helper) healthy(ctx context.Context) (string, error) {
	spec, err := h.marked(ctx, healthyName(h.Operation), healthyKind)
	if err != nil || spec == nil {
		return "", err
	}
	return labels(spec)[containerLabel], nil
}

func (h *Helper) markHealthy(ctx context.Context, replacement string) error {
	if err := h.createMarker(ctx, healthyName(h.Operation), healthyKind, map[string]string{containerLabel: replacement}); err != nil {
		return err
	}
	if healthy, err := h.healthy(ctx); err != nil || healthy != replacement {
		return cmpErr(err, errors.New("the update recorded another container as healthy"))
	}
	return nil
}

// ready reports whether the Ludock in container wrote, for its current run,
// that it opened its data and settles this update.
func (h *Helper) ready(ctx context.Context, container *docker.ContainerSpec) (bool, error) {
	spec, err := h.marked(ctx, readyName(h.Operation, container.ID), readyKind)
	if err != nil || spec == nil {
		return false, err
	}
	found := labels(spec)
	return found[containerLabel] == container.ID && found[startedLabel] == container.State.StartedAt, nil
}

// decide records that the original goes back, and why. restore: the
// original was stopped, so the new version may have changed the data.
func (h *Helper) decide(ctx context.Context, reason string, restore bool) error {
	log.Warn("The new version of Ludock didn't come up; putting the previous one back", logging.F("reason", reason))
	if err := h.createMarker(ctx, undoName(h.Operation), undoKind, map[string]string{ReasonLabel: reason, restoreLabel: strconv.FormatBool(restore)}); err != nil {
		return err
	}
	// The next pass puts it back.
	return errors.New("putting the previous version back")
}

func (h *Helper) inspect(ctx context.Context, id string) (*docker.ContainerSpec, error) {
	spec, err := h.Docker.InspectSpec(ctx, id)
	if docker.IsNotFound(err) {
		return nil, nil
	}
	return spec, err
}

// replacement is the container this update created: it carries the
// update's label and the temporary name or, once renamed, the original's.
func (h *Helper) replacement(ctx context.Context, name string) (*docker.ContainerSpec, error) {
	for _, candidate := range []string{servers.ReplacementName(name, h.Operation), name} {
		spec, err := h.inspect(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if spec != nil && spec.ID != h.Container && labels(spec)[update.OperationLabel] == h.Operation {
			return spec, nil
		}
	}
	return nil, nil
}

// create creates the replacement from the original's definition, as a
// server update does, under the temporary name and with no restart policy.
// It returns why Docker refused, or an error to try again.
func (h *Helper) create(ctx context.Context, old *docker.ContainerSpec) (id, reason string, err error) {
	plan, err := (&update.Service{Docker: h.Docker}).CheckContainer(ctx, old.ID, false)
	if _, refused := apperr.As(err); refused {
		return "", ReasonCreate, nil
	}
	if err != nil {
		return "", "", err
	}
	// The replacement names the tag, which must still be the pulled image;
	// the created container is checked again.
	if current, err := resolveImage(ctx, h.Docker, plan.Image, plan); err != nil || current != h.Image {
		return "", ReasonImage, nil
	}
	image, err := h.Docker.InspectImageConfig(ctx, h.Image)
	if err != nil {
		return "", "", err
	}
	body, err := plan.Replacement(image, h.Operation)
	if err != nil {
		return "", ReasonCreate, nil
	}
	body["HostConfig"].(map[string]json.RawMessage)["RestartPolicy"] = noRestart
	id, err = h.Docker.Create(ctx, servers.ReplacementName(old.Name, h.Operation), plan.Platform(), body)
	switch status := docker.StatusCode(err); {
	case err == nil:
		return id, "", nil
	case status == 0 || status == 409:
		// A create whose answer was lost made it under the name; the next
		// pass finds it by its label.
		if status == 409 {
			if existing, err := h.replacement(ctx, strings.TrimPrefix(old.Name, "/")); err != nil || existing == nil {
				return "", ReasonCreate, err
			}
		}
		return "", "", err
	default:
		return "", ReasonCreate, nil
	}
}

var noRestart = json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`)

// stopOriginal stops the original, which then must not start on its own.
// Docker gives it its own stop timeout, which Ludock's shutdown fits in.
func (h *Helper) stopOriginal(ctx context.Context, id string) error {
	if err := h.Docker.SetRestartPolicy(ctx, id, noRestart); err != nil {
		return err
	}
	if err := h.Docker.Stop(ctx, id, nil); err != nil && docker.StatusCode(err) != 304 {
		return err
	}
	if spec, err := h.inspect(ctx, id); err != nil || spec == nil || spec.State.Running {
		return cmpErr(err, errors.New("the previous container is still running"))
	}
	return nil
}

// stillStopped confirms the original, stopped for the swap, wasn't started
// again from outside, as Docker allows.
func (h *Helper) stillStopped(ctx context.Context, original string) error {
	spec, err := h.inspect(ctx, original)
	switch {
	case err != nil:
		return err
	case spec == nil:
		return errors.New("the previous container disappeared")
	case spec.State.Running || spec.State.Restarting:
		return errOriginalRunning
	}
	return nil
}

// awaitHealthy waits for the replacement's own health check and for it to
// report it opened its data, timed from its start so a helper run again
// keeps the deadline, while original, when set, stays stopped. It returns
// why it failed, or "".
func (h *Helper) awaitHealthy(ctx context.Context, id, original string) (string, error) {
	wait := h.HealthWait
	if wait == 0 {
		wait = HealthWait
	}
	run := ""
	for {
		if original != "" {
			if err := h.stillStopped(ctx, original); err != nil {
				return "", err
			}
		}
		spec, err := h.inspect(ctx, id)
		if err != nil {
			return "", err
		}
		if spec == nil || !spec.State.Running || spec.State.Restarting || (run != "" && spec.State.StartedAt != run) {
			return ReasonStopped, nil
		}
		run = spec.State.StartedAt
		// A helper resuming late judges by the deadline first.
		if started, err := time.Parse(time.RFC3339Nano, run); err == nil && time.Since(started) >= wait {
			return ReasonTimeout, nil
		}
		health := spec.State.Health
		switch {
		case health == nil || health.Status == "" || health.Status == "none":
			return ReasonNoHealth, nil
		case health.Status == "unhealthy":
			return ReasonUnhealthy, nil
		case health.Status == "healthy":
			if ready, err := h.ready(ctx, spec); err != nil || ready {
				return "", err
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(h.poll()):
		}
	}
}

// finish gives the replacement, which passed, the original's restart
// policy and name; a container that took the name meanwhile is left alone.
func (h *Helper) finish(ctx context.Context, id string, recorded marker) error {
	replacement, err := h.inspect(ctx, id)
	if err != nil || replacement == nil {
		return cmpErr(err, errBothGone)
	}
	if !replacement.State.Running {
		if err := h.Docker.Start(ctx, id); err != nil && docker.StatusCode(err) != 304 {
			return err
		}
	}
	if err := h.Docker.SetRestartPolicy(ctx, id, policy(recorded)); err != nil {
		return err
	}
	if replacement.Name != "/"+recorded.name {
		if occupant, err := h.inspect(ctx, recorded.name); err != nil || (occupant != nil && occupant.ID != id) {
			return cmpErr(err, errNameTaken)
		}
		if err := h.Docker.Rename(ctx, id, recorded.name); err != nil {
			return err
		}
	}
	return h.leave(ctx, id, recorded.name)
}

func policy(recorded marker) json.RawMessage {
	if len(recorded.policy) == 0 || !json.Valid(recorded.policy) {
		return noRestart
	}
	return recorded.policy
}

// putBack removes the replacement and starts the original again, first
// restoring the data save if the new version may have changed the data.
func (h *Helper) putBack(ctx context.Context, old, replacement *docker.ContainerSpec, recorded marker, decided *undo) error {
	if replacement != nil {
		if err := h.removeUntilGone(ctx, replacement.ID, false); err != nil {
			return err
		}
	}
	if decided.restore {
		// Once restored, the original may be running on the restored data
		// already; it isn't stopped again.
		code, err := h.runData(ctx, old, h.Image, "self-update-data", "settled", h.Operation)
		if err != nil {
			return err
		}
		if code != dataSettled {
			if err := h.stopOriginal(ctx, old.ID); err != nil {
				return err
			}
			if err := h.restoreData(ctx, old, recorded.save); err != nil {
				return err
			}
		}
	}
	if err := h.Docker.SetRestartPolicy(ctx, old.ID, policy(recorded)); err != nil {
		return err
	}
	if err := h.Docker.Start(ctx, old.ID); err != nil && docker.StatusCode(err) != 304 {
		return err
	}
	return h.leave(ctx, old.ID, recorded.name)
}

// leave removes the helper once exactly one Ludock runs under the name, the
// expected one, with no container of this update beside it, and it has
// reported that it opened its data.
func (h *Helper) leave(ctx context.Context, expected, name string) error {
	current, err := h.inspect(ctx, name)
	if err != nil {
		return err
	}
	if current == nil || current.ID != expected || !current.State.Running {
		return errors.New("ludock isn't running under its name yet")
	}
	if other, err := h.replacement(ctx, name); err != nil || (other != nil && other.ID != expected) {
		return cmpErr(err, errors.New("a second Ludock container is left"))
	}
	if expected != h.Container {
		if old, err := h.inspect(ctx, h.Container); err != nil || old != nil {
			return cmpErr(err, errors.New("the previous container is left"))
		}
	}
	// The Ludock there says when it opened its data; a restart of it, or
	// anything else changing, makes the pass look again.
	for waited := 0; ; waited++ {
		ready, err := h.ready(ctx, current)
		if err != nil {
			return err
		}
		if ready {
			break
		}
		if waited%int(time.Minute/h.poll()+1) == 0 {
			log.Info("Waiting for Ludock to report that it opened its data; its log says why if it doesn't")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(h.poll()):
		}
		if err := h.sameRun(ctx, current); err != nil {
			return err
		}
	}
	self, err := h.marked(ctx, HelperName(h.Operation), helperKind)
	if err != nil || self == nil {
		return cmpErr(err, errors.New("the helper can't find its own container"))
	}
	// Its readiness counts only for the run still going, checked last.
	if err := h.sameRun(ctx, current); err != nil {
		return err
	}
	log.Info("Ludock's update is settled; removing its helper")
	// Removing itself ends this process, so the answer may never come; one
	// that never reached Docker leaves it to retry.
	if err := h.Docker.Remove(ctx, self.ID, true, true); err != nil && !docker.IsNotFound(err) {
		if still, inspectErr := h.inspect(ctx, self.ID); inspectErr != nil || still != nil {
			return err
		}
	}
	return nil
}

// sameRun confirms the Ludock under the name is still current's run; one
// that stopped or restarted sends the pass back to start or check it again.
func (h *Helper) sameRun(ctx context.Context, current *docker.ContainerSpec) error {
	again, err := h.inspect(ctx, strings.TrimPrefix(current.Name, "/"))
	if err != nil || again == nil || again.ID != current.ID || !again.State.Running || again.State.StartedAt != current.State.StartedAt {
		return cmpErr(err, errNotReady)
	}
	return nil
}

// removeUntilGone removes a container; volumes also removes the anonymous
// volumes its image gave it, which a Ludock container's replacement may use.
func (h *Helper) removeUntilGone(ctx context.Context, id string, volumes bool) error {
	if err := h.Docker.Remove(ctx, id, true, volumes); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if spec, err := h.inspect(ctx, id); err != nil || spec != nil {
		return cmpErr(err, errors.New("a container is still being removed"))
	}
	return nil
}

// restoreData puts the data save back: the new image moves the live data
// aside, the old image's own restore-data restores the save, since only the
// old binary can be trusted with its data. A save that version can't
// restore moves the newer data back, so nothing is lost; any other failure
// keeps the save and tries again.
func (h *Helper) restoreData(ctx context.Context, old *docker.ContainerSpec, save string) error {
	dataDir := dataDirOf(old)
	code, err := h.runData(ctx, old, h.Image, "self-update-data", "aside", h.Operation)
	if err != nil || code == dataSettled {
		return err
	}
	if code != 0 {
		return errors.New("could not move the newer data aside")
	}
	outcome := "restored"
	code, err = h.runData(ctx, old, old.Image, "restore-data", "--sha256", save, path.Join(dataDir, SaveName(h.Operation)))
	switch {
	case err != nil:
		return err
	case code == RestoreFinal:
		log.Error("The previous version can't restore its data save, so the newer data was kept; the previous version may refuse it")
		if code, err := h.runData(ctx, old, h.Image, "self-update-data", "back", h.Operation); err != nil || code != 0 {
			return cmpErr(err, errors.New("could not move the newer data back"))
		}
		outcome = "kept"
	case code != 0:
		return errors.New("the data restore failed; trying again")
	}
	if code, err := h.runData(ctx, old, h.Image, "self-update-data", "mark", h.Operation, outcome); err != nil || code != 0 {
		return cmpErr(err, errors.New("could not record the data restore"))
	}
	return nil
}

// dataDirOf is Ludock's data folder inside the original, as config reads it.
func dataDirOf(old *docker.ContainerSpec) string {
	var config struct {
		Env        []string
		WorkingDir string
	}
	json.Unmarshal(encode(old.Config), &config)
	dir := "data"
	for _, entry := range config.Env {
		if value, ok := strings.CutPrefix(entry, "LUDOCK_DATA_DIR="); ok && value != "" {
			dir = value
		}
	}
	if !path.IsAbs(dir) {
		dir = path.Join("/", config.WorkingDir, dir)
	}
	return path.Clean(dir)
}

// errDataMount: the data folder's mount can't be reproduced exactly, so a
// data step could not reach the same files.
var errDataMount = errors.New("ludock's data mount can't be reproduced")

// DataMount is the mount settings that reproduce the container's mount
// holding dataDir: the same volume and subpath, or the same folder with the
// same bind options. Another mount inside the data folder, such as of the
// key folder or the database alone, can't be reproduced by one mount.
func DataMount(info *docker.ContainerInspect, dataDir string) (map[string]any, error) {
	var found *identity.Mount
	mounts := identity.DockerMounts(info)
	for index, mount := range mounts {
		if (mount.Type == "volume" || mount.Type == "bind") && helper.Within(dataDir, mount.Destination) &&
			(found == nil || len(mount.Destination) > len(found.Destination)) {
			found = &mounts[index]
		}
	}
	if found == nil || found.Unresolved {
		return nil, errDataMount
	}
	for _, mount := range mounts {
		if path.Clean(mount.Destination) != path.Clean(found.Destination) && helper.Within(mount.Destination, dataDir) {
			return nil, errDataMount
		}
	}
	settings := map[string]any{"Type": found.Type, "Source": found.Source, "Target": found.Destination, "ReadOnly": !found.Writable}
	for _, declared := range info.Host.Mounts {
		if declared.Type == "bind" && found.Type == "bind" && path.Clean(declared.Target) == path.Clean(found.Destination) && declared.BindOptions != nil {
			settings["BindOptions"] = declared.BindOptions
		}
	}
	if found.Type == "volume" {
		if found.Name == "" || strings.Contains(found.Name, "/") {
			return nil, errDataMount
		}
		settings["Source"] = found.Name
		settings["VolumeOptions"] = map[string]any{"NoCopy": true, "Subpath": found.Subpath}
	}
	return settings, nil
}

// runData runs a data step in a one-off container from image, by ID, as the
// original's user and groups with exactly its data mount, no network, and
// no socket, and returns its exit code. A step left running by an earlier
// pass is waited out first.
func (h *Helper) runData(ctx context.Context, old *docker.ContainerSpec, image string, cmd ...string) (int, error) {
	name := dataName(h.Operation)
	if leftover, err := h.marked(ctx, name, dataKind); err != nil {
		return 0, err
	} else if leftover != nil {
		if _, err := h.Docker.Wait(ctx, leftover.ID); err != nil && !docker.IsNotFound(err) {
			return 0, err
		}
		if err := h.removeUntilGone(ctx, leftover.ID, true); err != nil {
			return 0, err
		}
	}
	info, err := h.Docker.Inspect(ctx, old.ID)
	if err != nil {
		return 0, err
	}
	dataDir := dataDirOf(old)
	mount, err := DataMount(info, dataDir)
	if err != nil {
		return 0, err
	}
	var config struct {
		Env  []string
		User string
	}
	json.Unmarshal(encode(old.Config), &config)
	var host struct{ CapAdd, GroupAdd []string }
	json.Unmarshal(encode(old.HostConfig), &host)
	stepLabels := launch.HelperLabels(dataKind)
	stepLabels[operationLabel] = h.Operation
	body := map[string]any{"Image": image, "Entrypoint": []string{helper.Binary}, "Cmd": cmd, "User": config.User,
		"Env": append(config.Env, "LUDOCK_DATA_DIR="+dataDir), "Labels": stepLabels, "Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "CapAdd": host.CapAdd,
			"GroupAdd": host.GroupAdd, "SecurityOpt": []string{"no-new-privileges:true"}, "Mounts": []map[string]any{mount}}}
	id, err := h.Docker.Create(ctx, name, "", body)
	if err != nil {
		return 0, err
	}
	code := 0
	if err = h.Docker.Start(ctx, id); err == nil {
		code, err = h.Docker.Wait(ctx, id)
	}
	// Removed whatever happened; a step cut short is repeated whole, and a
	// removal that fails is retried by the next pass, which waits it out.
	if removeErr := h.removeUntilGone(context.WithoutCancel(ctx), id, true); err == nil {
		err = removeErr
	}
	return code, err
}

// resolveImage is the ID of the image reference names, which must hold the
// platform the container runs. With the containerd image store, a
// platform-qualified inspect answers with the platform manifest's ID, which
// Docker can't create a container from when the image is an index.
func resolveImage(ctx context.Context, client *docker.Client, reference string, plan *update.Plan) (string, error) {
	image, err := client.InspectImageConfig(ctx, reference)
	if err != nil {
		return "", err
	}
	var want docker.Platform
	if running := plan.Spec.ImageManifestDescriptor; running != nil && running.Platform != nil {
		want = *running.Platform
	} else if parts := strings.SplitN(plan.Platform(), "/", 3); len(parts) >= 2 {
		want = docker.Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) == 3 {
			want.Variant = parts[2]
		}
	}
	if want.OS == "" {
		return image.ID, nil
	}
	same := func(have docker.Platform) bool {
		return have.OS == want.OS && have.Architecture == want.Architecture && (want.Variant == "" || have.Variant == want.Variant)
	}
	if len(image.Manifests) > 0 {
		for _, manifest := range image.Manifests {
			if manifest.Kind == "image" && manifest.Available && manifest.ImageData != nil && same(manifest.ImageData.Platform) {
				return image.ID, nil
			}
		}
	} else if same(docker.Platform{OS: image.OS, Architecture: image.Architecture, Variant: image.Variant}) {
		return image.ID, nil
	}
	return "", errors.New("the image holds no " + plan.Platform() + " variant")
}

func labels(spec *docker.ContainerSpec) map[string]string {
	var config struct{ Labels map[string]string }
	json.Unmarshal(encode(spec.Config), &config)
	return config.Labels
}

func encode(fields map[string]json.RawMessage) []byte {
	encoded, _ := json.Marshal(fields)
	return encoded
}
