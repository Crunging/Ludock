package selfupdate

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

var log = logging.New("self-update")

// The helper's exit codes, which the Ludock that runs next reads: how the
// update ended, or anything else when it was cut short.
const (
	ExitUpdated = 0
	// ExitUnchanged: nothing was changed, as when Docker refused to create
	// the new container.
	ExitUnchanged = 10
	// ExitPutBack, plus a Reason's index: the original runs again, and puts
	// the data copy back as it starts.
	ExitPutBack = 20
)

// Reasons the new version was put back, by index into the exit code.
var Reasons = []string{
	"Docker couldn’t start the new container, for example because its port was still in use",
	"the new version stopped right after it started",
	"the new version reported itself unhealthy, as one whose image is older than this Ludock’s data does",
	"the new version didn’t report healthy within 3 minutes",
	"the new image has no health check, so Ludock couldn’t confirm it works",
}

const (
	reasonStart = iota
	reasonStopped
	reasonUnhealthy
	reasonTimeout
	reasonNoHealth
)

// HealthWait is how long the new version has to report healthy.
const HealthWait = 3 * time.Minute

// CopyName is the copy of the database Ludock writes beside it before an
// update hands over, which the old version renames into place when it is
// put back.
const CopyName = "ludock.db.before-update"

// helperKind is the update's helper.
const helperKind = "self-update"

// Helper replaces Ludock's container from a container of the same image:
// it creates the new one from the original's definition, stops the
// original, and waits for the new one's health check; then it removes the
// original, or removes the new one and starts the original again, which
// puts the data copy back. It keeps nothing but its exit code: an
// interruption leaves the rest to the owner.
type Helper struct {
	Docker    *docker.Client
	Container string
	// Image is the pulled image the new container runs.
	Image      string
	Operation  string
	HealthWait time.Duration
	Poll       time.Duration
}

// Run returns the helper's exit code.
func (h *Helper) Run(ctx context.Context) int {
	old, err := h.Docker.InspectSpec(ctx, h.Container)
	if err != nil {
		log.Error("Ludock's update helper couldn't find Ludock's container", logging.Cause(err, docker.StatusCode(err))...)
		return ExitUnchanged
	}
	policy := old.HostConfig["RestartPolicy"]
	plan, err := (&update.Service{Docker: h.Docker}).CheckContainer(ctx, old.ID, false)
	var image *docker.ImageInspect
	if err == nil {
		image, err = h.Docker.InspectImageConfig(ctx, h.Image)
	}
	var body map[string]any
	if err == nil {
		body, err = plan.Replacement(image, h.Operation)
	}
	id := ""
	if err == nil {
		body["HostConfig"].(map[string]json.RawMessage)["RestartPolicy"] = noRestart
		id, err = h.Docker.Create(ctx, servers.ReplacementName(old.Name, h.Operation), plan.Platform(), body)
	}
	if err != nil {
		log.Error("Docker couldn't create Ludock's new container, so nothing was changed", logging.Cause(err, docker.StatusCode(err))...)
		return ExitUnchanged
	}
	// From here until the swap settles the original doesn't restart by
	// itself, so a reboot can't run both.
	if err := h.Docker.SetRestartPolicy(ctx, old.ID, noRestart); err != nil {
		h.Docker.Remove(ctx, id, true, true)
		return ExitUnchanged
	}
	if err := h.Docker.Stop(ctx, old.ID, nil); err != nil && docker.StatusCode(err) != 304 {
		h.Docker.Remove(ctx, id, true, true)
		h.Docker.SetRestartPolicy(ctx, old.ID, policy)
		return ExitUnchanged
	}
	reason := reasonStart
	if err := h.Docker.Start(ctx, id); err == nil {
		reason = h.awaitHealthy(ctx, id)
	}
	if reason < 0 {
		// The original goes only once the new one passed.
		if err := h.Docker.Remove(ctx, old.ID, true, false); err != nil && !docker.IsNotFound(err) {
			log.Error("Ludock's update couldn't remove the previous container; remove it with docker rm", logging.Cause(err, docker.StatusCode(err))...)
		}
		h.Docker.SetRestartPolicy(ctx, id, policy)
		if err := h.Docker.Rename(ctx, id, strings.TrimPrefix(old.Name, "/")); err != nil {
			log.Error("Ludock's update couldn't give the new container Ludock's name", logging.Cause(err, docker.StatusCode(err))...)
		}
		return ExitUpdated
	}
	return h.putBack(ctx, old, id, policy, reason)
}

// putBack removes the new container and starts the original again with
// its restart policy.
func (h *Helper) putBack(ctx context.Context, old *docker.ContainerSpec, id string, policy json.RawMessage, reason int) int {
	h.Docker.Stop(ctx, id, nil)
	if err := h.Docker.Remove(ctx, id, true, true); err != nil && !docker.IsNotFound(err) {
		log.Error("Ludock's update couldn't remove the new container", logging.Cause(err, docker.StatusCode(err))...)
		return 1
	}
	if err := h.Docker.Start(ctx, old.ID); err != nil && docker.StatusCode(err) != 304 {
		log.Error("Ludock's update couldn't start the previous container; start it with docker compose up -d", logging.Cause(err, docker.StatusCode(err))...)
		return 1
	}
	h.Docker.SetRestartPolicy(ctx, old.ID, policy)
	return ExitPutBack + reason
}

var noRestart = json.RawMessage(`{"Name":"no","MaximumRetryCount":0}`)

// awaitHealthy waits for the new container's own health check, from its
// start, and returns -1 or why it failed.
func (h *Helper) awaitHealthy(ctx context.Context, id string) int {
	wait := h.HealthWait
	if wait == 0 {
		wait = HealthWait
	}
	deadline := time.Now().Add(wait)
	for {
		spec, err := h.Docker.InspectSpec(ctx, id)
		switch {
		case err != nil && !docker.IsNotFound(err):
		case err != nil || !spec.State.Running || spec.State.Restarting:
			return reasonStopped
		case spec.State.Health == nil || spec.State.Health.Status == "" || spec.State.Health.Status == "none":
			return reasonNoHealth
		case spec.State.Health.Status == "unhealthy":
			return reasonUnhealthy
		case spec.State.Health.Status == "healthy":
			return -1
		}
		if time.Now().After(deadline) {
			return reasonTimeout
		}
		select {
		case <-ctx.Done():
			return reasonTimeout
		case <-time.After(h.poll()):
		}
	}
}

func (h *Helper) poll() time.Duration {
	if h.Poll == 0 {
		return 2 * time.Second
	}
	return h.Poll
}

// relabelled reports a bind at target relabelled for its container alone
// (-v …:Z), which no other container can reach and mount settings can't
// relabel.
func relabelled(info *docker.ContainerInspect, target string) bool {
	for _, bind := range info.Host.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) == 3 && path.Clean(parts[1]) == path.Clean(target) && slices.Contains(strings.Split(parts[2], ","), "Z") {
			return true
		}
	}
	return false
}

// resolveImage is the ID of the image reference names, the image's own,
// not a platform manifest's, which Docker can't create a container from
// when the image is an index.
func resolveImage(ctx context.Context, client *docker.Client, reference string) (string, error) {
	return client.InspectImage(ctx, reference)
}

func encode(fields map[string]json.RawMessage) []byte {
	encoded, _ := json.Marshal(fields)
	return encoded
}
