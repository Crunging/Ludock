package update

import (
	"context"
	"database/sql"
	"errors"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("updates")

const (
	checksSetting = "updates.checks"
	stateSetting  = "updates.state"

	checkInterval = 6 * time.Hour
	retryInterval = time.Hour
	// Images first seen wait up to firstDelay, so a fleet isn't checked at once.
	firstDelay = 15 * time.Minute
	// checkNowSpacing keeps repeated Check now presses off the registry:
	// Docker Hub counts each lookup against its pull rate limit.
	checkNowSpacing = 10 * time.Second
)

// Check states a server can be in. Servers Update server doesn't support
// have no state.
const (
	CheckPending   = "pending"
	CheckCurrent   = "current"
	CheckAvailable = "available"
	// CheckUnknown: the server's image records no registry digest for its
	// repository, such as one built or loaded locally.
	CheckUnknown = "unknown"
	// CheckPlatform: the tag no longer offers the server's platform, so
	// pulling it would not update the server.
	CheckPlatform = "platform"
)

// Checks looks for newer versions of servers' images in their registries
// through Docker, without pulling them, and notifies once per server and
// version found.
type Checks struct {
	Updates  *Service
	DB       *db.DB
	Identity *identity.Store
	Notify   func(key, message string)

	// run serializes passes that reach Docker; mu guards state, which pages
	// read without waiting for a registry.
	run   chan struct{}
	once  sync.Once
	mu    sync.Mutex
	state *checkState
	// generation counts Forget calls per server, so an observation made
	// before one is not applied after it.
	generation map[string]int
	// forgotten counts Forget calls per image key, so a registry answer
	// asked for before one is not kept after it.
	forgotten map[string]int
}

type checkState struct {
	Images  map[string]*imageCheck  `json:"images"`
	Servers map[string]*serverCheck `json:"servers"`
}

// imageCheck is one image reference's registry state, keyed by its
// normalized reference.
type imageCheck struct {
	// Reference is the image as a container configured it, which is what
	// Update server pulls.
	Reference string            `json:"reference"`
	Digest    string            `json:"digest,omitempty"`
	Platforms []docker.Platform `json:"platforms,omitempty"`
	CheckedAt int64             `json:"checkedAt,omitempty"`
	TriedAt   int64             `json:"triedAt,omitempty"`
	// Problem is "registry" or "config" while the last try failed.
	Problem string `json:"problem,omitempty"`
	NextAt  int64  `json:"nextAt"`
}

type serverCheck struct {
	ContainerID string `json:"containerId"`
	// Image is the normalized reference, or "" when Update server doesn't
	// support the container.
	Image      string `json:"image,omitempty"`
	Status     string `json:"status,omitempty"`
	ObservedAt int64  `json:"observedAt,omitempty"`
	// Notified is the newest registry digest already announced.
	Notified string `json:"notified,omitempty"`
}

// CheckStatus is what an administrator sees for one server.
type CheckStatus struct {
	State     string
	CheckedAt int64 // 0 until the registry first answered
	// Failed reports that the last registry check, at TriedAt, failed;
	// ConfigProblem that LUDOCK_DOCKER_CONFIG could not be read.
	Failed        bool
	ConfigProblem bool
	TriedAt       int64
}

type checkSettings struct {
	Enabled bool `json:"enabled"`
}

func (c *Checks) init() {
	c.once.Do(func() {
		c.run = make(chan struct{}, 1)
		c.generation = map[string]int{}
		c.forgotten = map[string]int{}
	})
}

// Enabled reports whether administrators left the checks on, the default.
// An unreadable setting counts as on: the checks only read.
func (c *Checks) Enabled(ctx context.Context) bool {
	value := checkSettings{Enabled: true}
	if _, err := c.DB.Setting(ctx, checksSetting, &value); err != nil {
		return true
	}
	return value.Enabled
}

// SetEnabled turns the checks on or off, checking the administrator again in
// the transaction that saves it.
func (c *Checks) SetEnabled(ctx context.Context, adminID, tokenHash string, enabled bool) error {
	return c.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		return c.DB.SetSetting(ctx, tx, checksSetting, checkSettings{Enabled: enabled})
	})
}

// loadLocked reads the saved state once; mu is held.
func (c *Checks) loadLocked(ctx context.Context) (*checkState, error) {
	if c.state != nil {
		return c.state, nil
	}
	state := &checkState{}
	if _, err := c.DB.Setting(ctx, stateSetting, state); err != nil {
		return nil, err
	}
	if state.Images == nil {
		state.Images = map[string]*imageCheck{}
	}
	if state.Servers == nil {
		state.Servers = map[string]*serverCheck{}
	}
	c.state = state
	return state, nil
}

func (c *Checks) saveLocked(ctx context.Context) {
	if err := c.DB.SetSetting(ctx, nil, stateSetting, c.state); err != nil {
		log.Warn("Could not save image update checks; they will be saved after the next check")
	}
}

// Status is the check state of the server running containerID, or nil when
// there is none to show: Update server doesn't support it, or it was
// recreated since.
func (c *Checks) Status(ctx context.Context, serverID, containerID string) *CheckStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.loadLocked(ctx)
	if err != nil {
		return nil
	}
	entry := state.Servers[serverID]
	if entry == nil || entry.ContainerID != containerID || entry.Image == "" {
		return nil
	}
	status := &CheckStatus{State: entry.Status}
	if image := state.Images[entry.Image]; image != nil {
		status.CheckedAt, status.TriedAt = image.CheckedAt, image.TriedAt
		status.Failed, status.ConfigProblem = image.Problem != "", image.Problem == "config"
	}
	return status
}

// Forget marks a server's result stale after its image was pulled, and
// drops its tag's last answer until the registry is asked again: the pull
// may have found a newer version than the one last seen.
func (c *Checks) Forget(serverID string) {
	c.init()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation[serverID]++
	if c.state != nil {
		if entry := c.state.Servers[serverID]; entry != nil {
			entry.ContainerID = ""
			if image := c.state.Images[entry.Image]; image != nil {
				image.Digest, image.NextAt = "", 0
				c.forgotten[entry.Image]++
				for _, other := range c.state.Servers {
					if other.Image == entry.Image {
						other.Status = CheckPending
					}
				}
			}
		}
	}
}

func jitter(base, spread time.Duration) int64 {
	return (base - spread/2 + rand.N(spread)).Milliseconds()
}

// acquire takes the pass lock, or fails when ctx ends first.
func (c *Checks) acquire(ctx context.Context) bool {
	c.init()
	select {
	case c.run <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Checks) release() { <-c.run }

// Run is one background pass over the servers Docker currently runs (by
// container ID): it compares the image of each new or recreated server with
// what its registry last said, then asks the registry about at most one due
// image reference.
func (c *Checks) Run(ctx context.Context, current map[string]servers.Container, now int64) {
	if current == nil || !c.acquire(ctx) {
		return
	}
	defer c.release()
	if !c.Enabled(ctx) {
		return
	}
	active, err := c.active(ctx, current)
	if err != nil {
		return
	}
	c.mu.Lock()
	state, err := c.loadLocked(ctx)
	if err != nil {
		c.mu.Unlock()
		return
	}
	changed := false
	for id := range state.Servers {
		if _, ok := active[id]; !ok {
			delete(state.Servers, id)
			changed = true
		}
	}
	var stale []string
	for id, container := range active {
		// Refusals are looked at again in time: another container sharing a
		// namespace can go away without this one being recreated.
		if entry := state.Servers[id]; entry == nil || entry.ContainerID != container.ID ||
			(entry.Image == "" && now-entry.ObservedAt >= checkInterval.Milliseconds()) {
			stale = append(stale, id)
		}
	}
	slices.Sort(stale)
	c.mu.Unlock()

	for _, id := range stale {
		c.apply(ctx, c.observe(ctx, id, active[id]), now, false)
		changed = true
	}

	c.mu.Lock()
	used := map[string]bool{}
	for _, entry := range state.Servers {
		used[entry.Image] = true
	}
	due := ""
	for key, image := range state.Images {
		if !used[key] {
			delete(state.Images, key)
			changed = true
		} else if image.NextAt <= now && (due == "" || image.NextAt < state.Images[due].NextAt) {
			due = key
		}
	}
	var checked []string
	if due != "" {
		for id, entry := range state.Servers {
			if entry.Image == due {
				checked = append(checked, id)
			}
		}
	}
	slices.Sort(checked)
	c.mu.Unlock()

	if due != "" {
		c.ask(ctx, due, now)
		// The containers are inspected again: whether Update server supports
		// one can change without it being recreated.
		for _, id := range checked {
			c.apply(ctx, c.observe(ctx, id, active[id]), now, false)
		}
		changed = true
	}
	if changed {
		c.mu.Lock()
		c.saveLocked(ctx)
		c.mu.Unlock()
	}
}

// active maps each server with an active binding to its container.
func (c *Checks) active(ctx context.Context, current map[string]servers.Container) (map[string]servers.Container, error) {
	logical, err := c.Identity.List(ctx, "")
	if err != nil {
		return nil, err
	}
	active := map[string]servers.Container{}
	for _, server := range logical {
		if container, ok := current[server.ContainerID]; ok && server.ContainerID != "" && server.Status == identity.StatusActive {
			active[server.ID] = container
		}
	}
	return active, nil
}

// CheckNow asks the registry about one server's image right away, unless
// it answered moments ago, and returns the result.
func (c *Checks) CheckNow(ctx context.Context, serverID string, container servers.Container) *CheckStatus {
	if !c.acquire(ctx) {
		return nil
	}
	defer c.release()
	if !c.Enabled(ctx) {
		return nil
	}
	now := db.Now()
	o := c.observe(ctx, serverID, container)
	c.mu.Lock()
	ask := false
	if state, err := c.loadLocked(ctx); err != nil {
		c.mu.Unlock()
		return nil
	} else if image := state.Images[o.key]; o.supported {
		ask = image == nil || now-image.TriedAt >= checkNowSpacing.Milliseconds()
	}
	c.mu.Unlock()
	// A registry answer about to be replaced is not announced.
	c.apply(ctx, o, now, ask)
	if ask {
		c.ask(ctx, o.key, now)
		c.apply(ctx, c.observe(ctx, serverID, container), now, false)
		// Other servers on the tag are compared with the new answer next pass.
		c.mu.Lock()
		for id, entry := range c.state.Servers {
			if id != serverID && entry.Image == o.key {
				entry.ContainerID = ""
			}
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	c.saveLocked(ctx)
	c.mu.Unlock()
	return c.Status(ctx, serverID, container.ID)
}

// observation is what Docker says about one server's container and image.
type observation struct {
	serverID   string
	container  servers.Container
	generation int
	// retry: Docker could not be read, so the server is compared again on
	// the next pass.
	retry       bool
	supported   bool
	reference   string
	key         string
	repoDigests []string
	platform    string
}

func (c *Checks) observe(ctx context.Context, serverID string, container servers.Container) observation {
	c.mu.Lock()
	o := observation{serverID: serverID, container: container, generation: c.generation[serverID]}
	c.mu.Unlock()
	plan, err := c.Updates.check(ctx, &servers.Context{Container: container}, nil, true)
	if err != nil {
		// Public errors are the cases Update server refuses.
		_, refused := apperr.As(err)
		o.retry = !refused
		return o
	}
	info, err := c.Updates.Docker.InspectImageConfig(ctx, plan.Spec.Image)
	if err != nil {
		o.retry = true
		return o
	}
	o.supported, o.reference, o.key, o.repoDigests, o.platform = true, plan.Image, imageKey(plan.Image), info.RepoDigests, plan.Platform()
	return o
}

// apply records an observation against the registry's last answer and,
// unless quiet, notifies when it finds a version not announced before.
func (c *Checks) apply(ctx context.Context, o observation, now int64, quiet bool) {
	// Checks turned off while a lookup ran announce nothing.
	quiet = quiet || !c.Enabled(ctx)
	c.mu.Lock()
	state, err := c.loadLocked(ctx)
	if err != nil || c.generation[o.serverID] != o.generation {
		c.mu.Unlock()
		return
	}
	previous := state.Servers[o.serverID]
	if o.retry {
		delete(state.Servers, o.serverID)
		if previous != nil && previous.Notified != "" {
			state.Servers[o.serverID] = &serverCheck{Notified: previous.Notified, Image: previous.Image}
		}
		c.mu.Unlock()
		return
	}
	entry := &serverCheck{ContainerID: o.container.ID, ObservedAt: now}
	if previous != nil {
		entry.Notified = previous.Notified
	}
	state.Servers[o.serverID] = entry
	if !o.supported {
		c.mu.Unlock()
		return
	}
	entry.Image = o.key
	image := state.Images[o.key]
	if image == nil {
		image = &imageCheck{Reference: o.reference, NextAt: now + time.Minute.Milliseconds() + rand.N(firstDelay).Milliseconds()}
		state.Images[o.key] = image
	}
	entry.Status = CheckPending
	if image.Digest != "" {
		entry.Status = compareDigests(o.key, o.repoDigests, image.Digest)
		if entry.Status == CheckAvailable && !offers(image.Platforms, o.platform) {
			entry.Status = CheckPlatform
		}
	}
	notify := !quiet && entry.Status == CheckAvailable && entry.Notified != image.Digest
	if notify {
		entry.Notified = image.Digest
	}
	digest := image.Digest
	c.mu.Unlock()
	if notify && c.Notify != nil {
		c.Notify("image-update:"+o.serverID+":"+digest, updateMessage(o.container))
	}
}

// updateMessage names the server but not its image: the reference can name
// a private registry or repository, and notifications leave the host.
func updateMessage(container servers.Container) string {
	message := container.DisplayName + ": a newer version of its container image is available. An administrator can update it from the server's Update tab in Ludock."
	if integration := games.ImageIntegration(container.Image); integration != nil && integration.UpdatesOnStart {
		message += " This is the image itself, such as its startup scripts and system packages; the game already updates on every restart."
	}
	return message
}

// ask records what the registry says key's tag points to now.
func (c *Checks) ask(ctx context.Context, key string, now int64) {
	c.mu.Lock()
	reference, forgotten := key, c.forgotten[key]
	if image := c.state.Images[key]; image != nil && image.Reference != "" {
		reference = image.Reference
	}
	c.mu.Unlock()
	auth, err := registryAuth(c.Updates.DockerConfig, reference)
	var info *docker.DistributionInspect
	if err == nil {
		info, err = c.Updates.Docker.DistributionInspect(ctx, reference, auth)
	}
	if err == nil && !digestPattern.MatchString(info.Descriptor.Digest) {
		err = errors.New("docker returned an invalid registry digest")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	image := c.state.Images[key]
	if image == nil || c.forgotten[key] != forgotten {
		return
	}
	image.TriedAt = now
	if err != nil {
		// The reference can name a private registry, so it isn't logged.
		log.Warn("Could not check a server image for a newer version; retrying later")
		image.Problem = "registry"
		if errors.Is(err, errRegistryConfig) {
			image.Problem = "config"
		}
		image.NextAt = now + jitter(retryInterval, 30*time.Minute)
		return
	}
	image.Problem, image.Digest, image.Platforms, image.CheckedAt = "", info.Descriptor.Digest, info.Platforms, now
	image.NextAt = now + jitter(checkInterval, time.Hour)
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imageKey normalizes a tagged image reference to registry/repository:tag,
// as Docker resolves it, so spellings of one tag share a check.
func imageKey(reference string) string {
	repository, tag := splitTag(reference)
	if tag == "" {
		tag = "latest"
	}
	return normalizeRepository(repository) + ":" + tag
}

// splitTag separates a reference's tag, dropping any digest.
func splitTag(reference string) (repository, tag string) {
	repository, _, _ = strings.Cut(reference, "@")
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		return repository[:colon], repository[colon+1:]
	}
	return repository, ""
}

// normalizeRepository spells a repository with its registry host, and
// Docker Hub's official images under library/.
func normalizeRepository(repository string) string {
	host, path := "docker.io", repository
	if first, rest, found := strings.Cut(repository, "/"); found && isRegistry(first) {
		host, path = normalizeRegistry(first), rest
	}
	if host == "docker.io" && !strings.Contains(path, "/") {
		path = "library/" + path
	}
	return host + "/" + path
}

// compareDigests compares remote, the digest key's tag points to now, with
// the image's repository digests. Both name the tag's top-level manifest
// (its index when multi-platform): the registry returns it, and a pull
// records it.
func compareDigests(key string, repoDigests []string, remote string) string {
	repository, _ := splitTag(key)
	found := false
	for _, entry := range repoDigests {
		name, digest, ok := strings.Cut(entry, "@")
		if !ok || normalizeRepository(name) != repository {
			continue
		}
		if digest == remote {
			return CheckCurrent
		}
		found = true
	}
	if !found {
		return CheckUnknown
	}
	return CheckAvailable
}

// offers reports whether a tag's platforms include one Docker would pull
// for platform: the same OS and architecture, and a variant no newer than
// the server's, such as arm/v6 for arm/v7. An empty list or platform can't
// rule it out.
func offers(platforms []docker.Platform, platform string) bool {
	if len(platforms) == 0 || platform == "" {
		return true
	}
	parts := strings.SplitN(platform, "/", 3)
	if len(parts) < 2 {
		return true
	}
	wanted := ""
	if len(parts) == 3 {
		wanted = parts[2]
	}
	return slices.ContainsFunc(platforms, func(offered docker.Platform) bool {
		if offered.OS != parts[0] || offered.Architecture != parts[1] {
			return false
		}
		if offered.Variant == "" || wanted == "" || offered.Variant == wanted {
			return true
		}
		have, errHave := strconv.ParseFloat(strings.TrimPrefix(offered.Variant, "v"), 64)
		want, errWant := strconv.ParseFloat(strings.TrimPrefix(wanted, "v"), 64)
		return errHave == nil && errWant == nil && have <= want
	})
}
