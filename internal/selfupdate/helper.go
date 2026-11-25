package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var log = logging.New("self-update")

// HealthWait is how long the new version has to report healthy.
const HealthWait = 3 * time.Minute

// Helper does one update's swap from inside its helper container. Docker
// runs it again after it is killed or the host restarts, and it keeps no
// state of its own: each pass reads what exists and finishes or undoes it.
// Before it changes anything it records the original container's name and
// restart policy in a never-started marker container, and once it decides
// to put the original back, it records why in a second one. The original
// stays the server until the replacement has reported healthy; only then is
// it removed, and from there the swap only goes forward.
type Helper struct {
	Docker *docker.Client
	// Container, Image, and Operation are the interface's arguments.
	Container, Image, Operation string
	Poll                        time.Duration
	HealthWait                  time.Duration
}

var errBothGone = errors.New("both Ludock containers are gone")

// Run settles the update, then removes the helper's own container. It
// returns only when that removal was sent or ctx ends.
func (h *Helper) Run(ctx context.Context) {
	for delay := h.poll(); ; delay = min(2*delay, 30*time.Second) {
		err := h.pass(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if errors.Is(err, errBothGone) {
			log.Error("Ludock's previous and new containers were both removed outside Ludock; start Ludock again with docker compose up -d")
			delay = time.Minute
		} else {
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
	replacement, err := h.replacement(ctx, recorded.name)
	if err != nil {
		return err
	}
	if old == nil {
		if replacement == nil {
			return errBothGone
		}
		return h.finish(ctx, replacement, recorded)
	}
	decided, err := h.undo(ctx)
	if err != nil {
		return err
	}
	if decided != nil {
		return h.putBack(ctx, old, replacement, recorded, decided)
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
	if replacement.State.Status == "created" {
		if err := h.stopOriginal(ctx, old.ID); err != nil {
			return err
		}
		if err := h.Docker.Start(ctx, replacement.ID); err != nil && docker.StatusCode(err) != 304 {
			if docker.Unavailable(err) && docker.StatusCode(err) == 0 {
				return err
			}
			return h.decide(ctx, ReasonStart, true)
		}
	}
	reason, err := h.awaitHealthy(ctx, replacement.ID)
	if err != nil {
		return err
	}
	if reason != "" {
		return h.decide(ctx, reason, true)
	}
	// The new version works: the original goes, and from here the swap only
	// goes forward.
	if err := h.removeUntilGone(ctx, old.ID); err != nil {
		return err
	}
	if replacement, err = h.inspect(ctx, replacement.ID); err != nil || replacement == nil {
		return cmpErr(err, errors.New("the new container disappeared"))
	}
	return h.finish(ctx, replacement, recorded)
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// marker reads the recorded name and restart policy, recording them from
// the original first.
func (h *Helper) marker(ctx context.Context) (marker, error) {
	for {
		spec, err := h.Docker.InspectSpec(ctx, stateName(h.Operation))
		if err == nil {
			var config struct{ Labels map[string]string }
			if json.Unmarshal(encode(spec.Config), &config) != nil || config.Labels[operationLabel] != h.Operation || config.Labels[nameLabel] == "" {
				return marker{}, errors.New("the update's marker is unreadable")
			}
			return marker{name: config.Labels[nameLabel], policy: json.RawMessage(config.Labels[policyLabel])}, nil
		}
		if !docker.IsNotFound(err) {
			return marker{}, err
		}
		original, err := h.Docker.InspectSpec(ctx, h.Container)
		if err != nil {
			return marker{}, err
		}
		labels := map[string]string{nameLabel: strings.TrimPrefix(original.Name, "/"), policyLabel: string(original.HostConfig["RestartPolicy"])}
		if err := h.createMarker(ctx, stateName(h.Operation), labels); err != nil {
			return marker{}, err
		}
	}
}

// createMarker creates a never-started container from the helper's image
// that only holds labels.
func (h *Helper) createMarker(ctx context.Context, name string, labels map[string]string) error {
	all := launch.HelperLabels(markerKind)
	all[operationLabel] = h.Operation
	maps.Copy(all, labels)
	body := map[string]any{"Image": h.Image, "Entrypoint": []string{helper.Binary}, "Cmd": []string{"version"}, "Labels": all,
		"Healthcheck": map[string]any{"Test": []string{"NONE"}}, "HostConfig": map[string]any{"NetworkMode": "none"}}
	if _, err := h.Docker.Create(ctx, name, "", body); err != nil && docker.StatusCode(err) != 409 {
		return err
	}
	return nil
}

func (h *Helper) undo(ctx context.Context) (*undo, error) {
	spec, err := h.Docker.InspectSpec(ctx, undoName(h.Operation))
	if docker.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config struct{ Labels map[string]string }
	json.Unmarshal(encode(spec.Config), &config)
	if config.Labels[operationLabel] != h.Operation {
		return nil, errors.New("the update's undo marker is unreadable")
	}
	return &undo{reason: config.Labels[ReasonLabel], restore: config.Labels[restoreLabel] == "true"}, nil
}

// decide records that the original goes back, and why. restore: the
// original was stopped, so the new version may have changed the data.
func (h *Helper) decide(ctx context.Context, reason string, restore bool) error {
	log.Warn("The new version of Ludock didn't come up; putting the previous one back", logging.F("reason", reason))
	if err := h.createMarker(ctx, undoName(h.Operation), map[string]string{ReasonLabel: reason, restoreLabel: strconv.FormatBool(restore)}); err != nil {
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
		if spec == nil || spec.ID == h.Container {
			continue
		}
		var config struct{ Labels map[string]string }
		if json.Unmarshal(encode(spec.Config), &config) == nil && config.Labels[update.OperationLabel] == h.Operation {
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
	// The replacement names the tag, which must still be the pulled image.
	if current, err := h.Docker.InspectImage(ctx, plan.Image); err != nil || current != h.Image {
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

// awaitHealthy waits for the replacement's own health check, timed from its
// start so a helper run again keeps the deadline. It returns why it failed,
// or "".
func (h *Helper) awaitHealthy(ctx context.Context, id string) (string, error) {
	wait := h.HealthWait
	if wait == 0 {
		wait = HealthWait
	}
	run := ""
	for {
		spec, err := h.inspect(ctx, id)
		if err != nil {
			return "", err
		}
		if spec == nil || !spec.State.Running || spec.State.Restarting || (run != "" && spec.State.StartedAt != run) {
			return ReasonStopped, nil
		}
		run = spec.State.StartedAt
		health := spec.State.Health
		switch {
		case health == nil || health.Status == "" || health.Status == "none":
			return ReasonNoHealth, nil
		case health.Status == "healthy":
			return "", nil
		case health.Status == "unhealthy":
			return ReasonUnhealthy, nil
		}
		if started, err := time.Parse(time.RFC3339Nano, run); err == nil && time.Since(started) >= wait {
			return ReasonTimeout, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(h.poll()):
		}
	}
}

// finish gives the replacement the original's name and restart policy.
func (h *Helper) finish(ctx context.Context, replacement *docker.ContainerSpec, recorded marker) error {
	if replacement.Name != "/"+recorded.name {
		if err := h.Docker.Rename(ctx, replacement.ID, recorded.name); err != nil {
			return err
		}
	}
	if err := h.Docker.SetRestartPolicy(ctx, replacement.ID, policy(recorded)); err != nil {
		return err
	}
	if !replacement.State.Running {
		if err := h.Docker.Start(ctx, replacement.ID); err != nil && docker.StatusCode(err) != 304 {
			return err
		}
	}
	return h.leave(ctx, replacement.ID, recorded.name)
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
		if err := h.removeUntilGone(ctx, replacement.ID); err != nil {
			return err
		}
	}
	if decided.restore {
		if err := h.stopOriginal(ctx, old.ID); err != nil {
			return err
		}
		if err := h.restoreData(ctx, old); err != nil {
			return err
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

// leave removes the helper once exactly one Ludock runs under the name:
// expected, with no container of this update beside it.
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
	log.Info("Ludock's update is settled; removing its helper")
	// Removing itself ends this process; the answer may never come.
	err = h.Docker.Remove(ctx, HelperName(h.Operation), true, false)
	if err != nil && !docker.IsNotFound(err) && docker.StatusCode(err) != 0 {
		return err
	}
	return nil
}

func (h *Helper) removeUntilGone(ctx context.Context, id string) error {
	if err := h.Docker.Remove(ctx, id, true, false); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if spec, err := h.inspect(ctx, id); err != nil || spec != nil {
		return cmpErr(err, errors.New("a container is still being removed"))
	}
	return nil
}

// restoreData puts the data save back: the new image moves the live data
// aside, the old image's own restore-data restores the save, since only the
// old binary can be trusted with its data, and a failed restore moves the
// newer data back so nothing is lost.
func (h *Helper) restoreData(ctx context.Context, old *docker.ContainerSpec) error {
	dataDir := dataDirOf(old)
	code, err := h.runData(ctx, old, h.Image, "self-update-data", "aside", h.Operation)
	if err != nil || code == dataSettled {
		return err
	}
	if code != 0 {
		return errors.New("could not move the newer data aside")
	}
	outcome := "restored"
	code, err = h.runData(ctx, old, old.Image, "restore-data", path.Join(dataDir, SaveName(h.Operation)))
	if err != nil {
		return err
	}
	if code != 0 {
		log.Error("The previous version couldn't restore its data save, so the newer data was kept; the previous version may refuse it")
		if code, err := h.runData(ctx, old, h.Image, "self-update-data", "back", h.Operation); err != nil || code != 0 {
			return cmpErr(err, errors.New("could not move the newer data back"))
		}
		outcome = "kept"
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

// DataMount is the original's mount that holds dataDir, or nil.
func DataMount(mounts []docker.MountPoint, dataDir string) *docker.MountPoint {
	var found *docker.MountPoint
	for index, mount := range mounts {
		if (mount.Type == "volume" || mount.Type == "bind") && helper.Within(dataDir, mount.Destination) &&
			(found == nil || len(mount.Destination) > len(found.Destination)) {
			found = &mounts[index]
		}
	}
	return found
}

// runData runs a data step in a one-off container from image, as the
// original's user with only its data mount, and returns its exit code. A
// step left running by an earlier pass is waited out first.
func (h *Helper) runData(ctx context.Context, old *docker.ContainerSpec, image string, cmd ...string) (int, error) {
	name := dataName(h.Operation)
	if leftover, err := h.inspect(ctx, name); err != nil {
		return 0, err
	} else if leftover != nil {
		if _, err := h.Docker.Wait(ctx, leftover.ID); err != nil && !docker.IsNotFound(err) {
			return 0, err
		}
		if err := h.removeUntilGone(ctx, leftover.ID); err != nil {
			return 0, err
		}
	}
	dataDir := dataDirOf(old)
	mount := DataMount(old.Mounts, dataDir)
	if mount == nil {
		return 0, errors.New("ludock's data folder isn't on a mount")
	}
	source := mount.Source
	if mount.Type == "volume" {
		source = mount.Name
	}
	var config struct {
		Env  []string
		User string
	}
	json.Unmarshal(encode(old.Config), &config)
	var host struct{ CapAdd []string }
	json.Unmarshal(encode(old.HostConfig), &host)
	labels := launch.HelperLabels(dataKind)
	labels[operationLabel] = h.Operation
	body := map[string]any{"Image": image, "Entrypoint": []string{helper.Binary}, "Cmd": cmd, "User": config.User,
		"Env": append(config.Env, "LUDOCK_DATA_DIR="+dataDir), "Labels": labels, "Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "CapAdd": host.CapAdd,
			"SecurityOpt": []string{"no-new-privileges:true"},
			"Mounts":      []map[string]any{{"Type": mount.Type, "Source": source, "Target": mount.Destination}}}}
	id, err := h.Docker.Create(ctx, name, "", body)
	if err != nil {
		return 0, err
	}
	if err := h.Docker.Start(ctx, id); err != nil {
		return 0, err
	}
	code, err := h.Docker.Wait(ctx, id)
	if err != nil {
		return 0, err
	}
	return code, h.removeUntilGone(ctx, id)
}

func encode(fields map[string]json.RawMessage) []byte {
	encoded, _ := json.Marshal(fields)
	return encoded
}
