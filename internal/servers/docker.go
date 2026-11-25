// Package servers observes managed containers and resolves logical servers
// to the exact container a caller is authorized to act on.
package servers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/self"
)

type Port struct {
	Private int
	Public  int
	Type    string
}

type Exit struct {
	Code      int
	OOMKilled bool
	// Started is when the run that exited began, 0 when unknown.
	Started int64
}

type Console struct {
	ID                 games.AdapterID
	Name               string
	CommandPlaceholder string
	Commands           []games.QuickCommand
	SupportsBroadcast  bool
	// ListsPlayers: Ludock can read who is online through this console.
	ListsPlayers bool
}

type Container struct {
	ID          string
	ShortID     string
	Name        string
	DisplayName string
	Image       string
	State       string
	Status      string
	Health      string // "", starting, healthy, unhealthy
	StateSince  int64  // 0 when unknown
	Exit        *Exit
	GameType    string
	GameName    string
	// AddressLabel is the owner's ludock.address, which replaces port
	// detection when valid.
	AddressLabel *games.Address
	// ConnectPort is the published port for the game's own port, or the
	// first published port for other images.
	ConnectPort int
	Console     *Console
	// Query asks the game itself whether it answers, or is nil.
	Query *games.StatusQuery
	// QueryOff says in plain words why the game's status query is off in
	// this setup, or is "".
	QueryOff string
	// HealthAnswers: a supported game without a status query whose image has
	// a health check, which then says whether the game answers. HealthyAt is
	// when the check last passed in Docker's recent results, or 0.
	HealthAnswers bool
	HealthyAt     int64
	FileRoots     []files.Root
	Ports         []Port
	Created       int64
	// Labels holds approved configuration labels only.
	Labels map[string]string
	// Source is why Ludock manages it: SourceImage, SourceLabel, or
	// SourcePanel.
	Source string
	// ControlsHost: its owner's label added a container that can control
	// the host.
	ControlsHost bool
	// AutoRemove: Docker deletes it as soon as it stops (--rm).
	AutoRemove bool
	// NestedMount: Docker mounts something inside one of its data folders,
	// so it can't be backed up.
	NestedMount bool
}

// ErrAutoRemove refuses anything that would stop a --rm server: Docker
// would delete it, with any unnamed volume holding its world.
var ErrAutoRemove = apperr.New("SERVER_DELETED_ON_STOP", 409, AutoRemoveMessage)

const AutoRemoveMessage = "Docker is set to delete this server as soon as it stops (it was started with --rm), so Ludock won’t stop it. Recreate it without --rm to stop, restart, back up or update it here."

// StopsServer reports whether work of this kind stops the server. Ludock's
// restart is a stop and a start.
func StopsServer(kind string) bool {
	switch kind {
	case ops.KindStop, ops.KindRestart, ops.KindBackup, ops.KindRestore, ops.KindUndoRestore, ops.KindUpdate:
		return true
	}
	return false
}

// Refuses reports whether Ludock refuses work of kind on the server because
// it would stop it and Docker deletes it when it stops (--rm).
func (c *Container) Refuses(kind string) bool { return c.AutoRemove && StopsServer(kind) }

// Refusal is ErrAutoRemove if the server Refuses work of kind.
func (c *Container) Refusal(kind string) error {
	if c.Refuses(kind) {
		return ErrAutoRemove
	}
	return nil
}

// InspectRefusal is Refusal for the container serverID is bound to now, as
// Docker reports it.
func (s *Service) InspectRefusal(ctx context.Context, serverID, kind string) error {
	if !StopsServer(kind) {
		return nil
	}
	binding, err := s.Identity.ResolveBinding(ctx, serverID, -1)
	if err != nil {
		return err
	}
	info, err := s.Observer.Docker.Inspect(ctx, binding.ContainerID)
	if err != nil {
		return err
	}
	if info.Host.AutoRemove {
		return ErrAutoRemove
	}
	return nil
}

// IsRunning reports whether Docker shows the container running.
func IsRunning(info *docker.ContainerInspect) bool {
	return info.State.Running || info.State.Status == "running"
}

type Observed struct {
	Container   Container
	Observation identity.Observation
}

func validContainerID(id string) (string, error) {
	if !docker.ContainerIDPattern.MatchString(id) {
		return "", apperr.New("INVALID_CONTAINER_ID", 400, "Invalid container identifier")
	}
	return id, nil
}

var (
	errNotManaged      = apperr.New("FORBIDDEN", 403, "Container is not managed by Ludock")
	errInvalidIdentity = apperr.New("INVALID_COMPOSE_IDENTITY", 409,
		"Container Compose identity is incomplete; administrator review required")
)

func assertEligible(info *docker.ContainerInspect) error {
	labels := info.Config.Labels
	if !games.Evaluate(info.Config.Image, labels).Eligible {
		return errNotManaged
	}
	if games.InvalidComposeIdentity(labels) {
		return errInvalidIdentity
	}
	return nil
}

type Observer struct {
	Docker *docker.Client
	// Choices are administrators' choices to add or remove servers; nil
	// means none.
	Choices *identity.Store
	// Now returns the current time; tests replace it.
	Now func() time.Time
	// Own is Ludock's own container, whose folders no server may offer.
	Own *self.Container

	mu          sync.Mutex
	inspections map[string]inspection
	// images are the references containers listed by image ID were created
	// from, by container ID.
	images map[string]string

	samplesMu sync.Mutex
	samples   map[string]*statsSample
}

// A Docker stats sample takes about a second. Viewers of the same container
// share one sample: in flight, and for this long after it completes.
const statsReuse = 2 * time.Second

type statsSample struct {
	done  chan struct{}
	taken time.Time
	stats *Stats
	err   error
}

// A container's configuration, labels, and mounts are fixed for its ID; its
// state, health, and name change the summary Docker lists. An inspection is
// reused while the summary is unchanged, and refreshed at least this often.
const inspectionReuse = 30 * time.Second

type inspection struct {
	summary     string
	inspectedAt time.Time
	info        *docker.ContainerInspect
}

func (o *Observer) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func listSummary(container docker.ContainerSummary) string {
	// Without a state summary there is no way to notice a change.
	if container.State == nil || container.Status == nil {
		return ""
	}
	encoded, _ := json.Marshal([]any{container.Names, container.Image, *container.State, *container.Status,
		container.Created, container.Labels})
	return string(encoded)
}

// listContainers lists every container by the image reference it was
// created from. Docker lists a container by its image's ID instead once the
// reference points to another image, as after the pull for an update.
func (o *Observer) listContainers(ctx context.Context) ([]docker.ContainerSummary, error) {
	containers, err := o.Docker.ListContainers(ctx, true, nil)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	known := o.images
	o.mu.Unlock()
	images := map[string]string{}
	listed := containers[:0]
	for _, container := range containers {
		if isImageID(container.Image) {
			reference, ok := known[container.ID]
			if !ok {
				info, err := o.Docker.Inspect(ctx, container.ID)
				// Discovery leaves out containers that vanished or it may not inspect.
				if status := docker.StatusCode(err); status == http.StatusNotFound || status == http.StatusForbidden {
					continue
				}
				if err != nil {
					return nil, err
				}
				reference = info.Config.Image
			}
			images[container.ID], container.Image = reference, reference
		}
		listed = append(listed, container)
	}
	o.mu.Lock()
	o.images = images
	o.mu.Unlock()
	return listed, nil
}

func isImageID(image string) bool {
	id := strings.TrimPrefix(image, "sha256:")
	return len(id) >= 12 && len(id) <= 64 && strings.Trim(id, "0123456789abcdef") == ""
}

// List observes every eligible container. A failure of any inspection other
// than a vanished or forbidden container fails the whole listing, so callers
// never reconcile a partial snapshot.
func (o *Observer) List(ctx context.Context) ([]Observed, error) {
	own, err := o.Own.Read(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := o.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	choices, err := o.choices(ctx)
	if err != nil {
		return nil, err
	}
	var eligible []docker.ContainerSummary
	for _, container := range containers {
		labels, _ := summaryCandidate(container).withChoice(choices)
		if games.Evaluate(container.Image, labels).Eligible && !isReplacement(container.Names) {
			eligible = append(eligible, container)
		}
	}
	o.mu.Lock()
	previous := o.inspections
	o.mu.Unlock()
	results := make([]*Observed, len(eligible))
	current := map[string]inspection{}
	var currentMu sync.Mutex
	var failure error
	var failureOnce sync.Once
	next := 0
	var nextMu sync.Mutex
	var wg sync.WaitGroup
	// Bound daemon load when inspecting changed containers; keep list order.
	for worker := 0; worker < min(4, len(eligible)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				nextMu.Lock()
				index := next
				next++
				stop := index >= len(eligible) || failure != nil
				nextMu.Unlock()
				if stop {
					return
				}
				observed, cached, err := o.observeSummary(ctx, eligible[index], previous, choices, own)
				if err != nil {
					status := docker.StatusCode(err)
					if status == http.StatusNotFound || status == http.StatusForbidden || apperr.Is(err, "INVALID_COMPOSE_IDENTITY") ||
						apperr.Is(err, "FORBIDDEN") {
						continue
					}
					failureOnce.Do(func() {
						nextMu.Lock()
						failure = err
						nextMu.Unlock()
					})
					continue
				}
				if cached != nil {
					currentMu.Lock()
					current[eligible[index].ID] = *cached
					currentMu.Unlock()
				}
				results[index] = observed
			}
		}()
	}
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	// Removed containers leave the cache with each complete listing.
	o.mu.Lock()
	o.inspections = current
	o.mu.Unlock()
	var observed []Observed
	for _, result := range results {
		if result != nil {
			observed = append(observed, *result)
		}
	}
	return observed, nil
}

// An update creates and checks its replacement container under a temporary
// name before removing the original. Until then the original stays the
// server, so the replacement is not discovered as a second one.
const replacementSuffix = "-ludock-update-"

var replacementName = regexp.MustCompile(`-ludock-update-[0-9a-f]{8}$`)

// ReplacementName is the temporary name of an update's replacement for the
// container named name. Long names are shortened to fit Docker's limit.
func ReplacementName(name, operationID string) string {
	sum := sha256.Sum256([]byte(operationID))
	suffix := replacementSuffix + hex.EncodeToString(sum[:4])
	name = strings.TrimPrefix(name, "/")
	return name[:min(len(name), 128-len(suffix))] + suffix
}

func isReplacement(names []string) bool {
	for _, name := range names {
		if replacementName.MatchString(name) {
			return true
		}
	}
	return false
}

func (o *Observer) observeSummary(ctx context.Context, container docker.ContainerSummary, previous map[string]inspection,
	choices map[string]identity.Choice, own self.Info) (*Observed, *inspection, error) {
	id, err := validContainerID(container.ID)
	if err != nil {
		return nil, nil, err
	}
	summary := listSummary(container)
	now := o.now()
	cached, ok := previous[id]
	if summary == "" || !ok || cached.summary != summary || now.Sub(cached.inspectedAt) >= inspectionReuse {
		info, err := o.Docker.Inspect(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		cached = inspection{summary: summary, inspectedAt: now, info: o.ownLabels(ctx, info)}
	}
	observed, _, err := observe(cached.info, choices, own)
	if err != nil {
		return nil, nil, err
	}
	if summary == "" {
		return observed, nil, nil
	}
	return observed, &cached, nil
}

// Inspect observes one container directly. Mutations and their binding
// checks always use this, never the cache. The inspection it returns has an
// administrator's choice applied to its labels, and is never sent to Docker.
func (o *Observer) Inspect(ctx context.Context, id string) (*Observed, *docker.ContainerInspect, error) {
	if _, err := validContainerID(id); err != nil {
		return nil, nil, err
	}
	own, err := o.Own.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	info, err := o.Docker.Inspect(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	choices, err := o.choices(ctx)
	if err != nil {
		return nil, nil, err
	}
	return observe(o.ownLabels(ctx, info), choices, own)
}

func observe(info *docker.ContainerInspect, choices map[string]identity.Choice, own self.Info) (*Observed, *docker.ContainerInspect, error) {
	info, source := applyChoice(info, choices)
	if err := assertEligible(info); err != nil {
		return nil, nil, err
	}
	container, err := toContainer(info, own)
	if err != nil {
		return nil, nil, err
	}
	container.Source, container.ControlsHost = source, inspectCandidate(info).controlsHost()
	return &Observed{Container: *container, Observation: toObservation(info, container)}, info, nil
}

// Docker keeps the last check result after a container stops; only a
// running one is current.
func containerHealth(info *docker.ContainerInspect) string {
	if info.State.Status != "running" || info.State.Health == nil {
		return ""
	}
	switch status := info.State.Health.Status; status {
	case "starting", "healthy", "unhealthy":
		return status
	}
	return ""
}

// dockerTime parses Docker's RFC 3339 times; never-set times are year 0001.
func dockerTime(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UnixMilli() <= 0 {
		return 0
	}
	return parsed.UnixMilli()
}

type binding struct {
	Port
	hostIP string
}

var loopback = regexp.MustCompile(`^127\.`)

// connectPort picks the host port players use. A recognized game counts only
// a reachable mapping of its own port and protocol: TCP and UDP mappings are
// independent, and a loopback-only mapping is unreachable for players.
// Without one no address is offered rather than another port such as RCON.
func connectPort(bindings []binding, gamePort *games.Port) int {
	for _, b := range bindings {
		if b.Public <= 0 || loopback.MatchString(b.hostIP) || b.hostIP == "::1" {
			continue
		}
		if gamePort == nil || (b.Private == gamePort.Port && b.Type == gamePort.Protocol) {
			return b.Public
		}
	}
	return 0
}

func parseLeadingInt(value string) int {
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	number, _ := strconv.Atoi(value[:end])
	return number
}

func toContainer(info *docker.ContainerInspect, own self.Info) (*Container, error) {
	id, err := validContainerID(info.ID)
	if err != nil {
		return nil, err
	}
	labels := info.Config.Labels
	gameType := games.TypeFor(info.Config.Image, labels)
	var bindings []binding
	for containerPort, published := range info.NetworkSettings.Ports {
		port, kind, _ := strings.Cut(containerPort, "/")
		if kind == "" {
			kind = "tcp"
		}
		for _, entry := range published {
			bindings = append(bindings, binding{Port{parseLeadingInt(port), parseLeadingInt(entry.HostPort), kind}, entry.HostIP})
		}
	}
	// Go maps are unordered; Docker's JSON object order is by port key.
	sortBindings(bindings)
	name := strings.TrimPrefix(info.Name, "/")
	container := &Container{
		ID: id, ShortID: id[:min(12, len(id))], Name: name, DisplayName: name, Image: info.Config.Image,
		State: info.State.Status, Status: info.State.Status, Health: containerHealth(info),
		GameType: gameType, GameName: games.DisplayName(gameType), Labels: games.ApprovedLabels(labels),
		AutoRemove: info.Host.AutoRemove,
	}
	if display := labels[games.LabelName]; display != "" {
		container.DisplayName = display
	}
	if info.State.Health != nil {
		container.Status += " (" + info.State.Health.Status + ")"
	}
	switch info.State.Status {
	case "running":
		container.StateSince = dockerTime(info.State.StartedAt)
	case "exited", "dead":
		container.StateSince = dockerTime(info.State.FinishedAt)
		container.Exit = &Exit{Code: info.State.ExitCode, OOMKilled: info.State.OOMKilled, Started: dockerTime(info.State.StartedAt)}
	}
	// Host addresses stay internal; only ports are shown.
	for _, b := range bindings {
		container.Ports = append(container.Ports, b.Port)
	}
	if created, err := time.Parse(time.RFC3339Nano, info.Created); err == nil {
		container.Created = created.UnixMilli()
	}
	if _, present := labels[games.LabelAddress]; present {
		container.AddressLabel = games.ParseAddress(labels[games.LabelAddress])
	}
	var gamePort *games.Port
	if integration := games.Lookup(gameType); integration != nil {
		gamePort = integration.GamePort
	}
	container.ConnectPort = connectPort(bindings, gamePort)
	env := info.Config.Environment()
	if adapter := games.ResolveAdapter(gameType, container.Labels); adapter != nil {
		// A game that announces another way can warn players only while that
		// way is on.
		container.Console = &Console{ID: adapter.ID, Name: adapter.Name, CommandPlaceholder: adapter.CommandPlaceholder,
			Commands: adapter.Commands, ListsPlayers: adapter.Players != nil,
			SupportsBroadcast: games.BroadcastTemplate(gameType, container.Labels) != "" && (adapter.Announce == nil || adapter.Announce.On(env))}
	}
	container.Query = games.ResolveQuery(gameType, container.Labels)
	if container.Query != nil && container.Labels[games.LabelQuery] == "" {
		if container.QueryOff = container.Query.Off(env); container.QueryOff != "" {
			container.Query = nil
		}
	}
	if container.Query == nil && games.Lookup(gameType) != nil && !games.QueryDisabled(container.Labels) && info.State.Health != nil {
		container.HealthAnswers = true
		if container.Health != "" {
			for _, result := range info.State.Health.Log {
				if result.ExitCode == 0 {
					container.HealthyAt = max(container.HealthyAt, dockerTime(result.End))
				}
			}
		}
	}
	mounts := identity.DockerMounts(info)
	container.FileRoots = files.Roots(gameType, container.Labels, mounts, own)
	container.NestedMount = slices.ContainsFunc(container.FileRoots, func(root files.Root) bool { return files.HoldsMount(root.Path, mounts) })
	return container, nil
}

// toObservation is internal only: raw mounts and data folders are hashed by
// the identity package, never shown.
func toObservation(info *docker.ContainerInspect, container *Container) identity.Observation {
	labels := info.Config.Labels
	observation := identity.Observation{ContainerID: container.ID, Name: container.Name, DisplayName: container.DisplayName,
		GameType: container.GameType, Mounts: identity.DockerMounts(info)}
	if project, service, number, ok := games.ComposeLabels(labels); ok {
		observation.Compose = &identity.ComposeIdentity{Project: project, Service: service, ContainerNumber: number}
	}
	// Each root is its own mount and every mount nested inside it, which
	// Files shows as part of the root.
	for _, root := range container.FileRoots {
		owner, _ := files.RootMount(root.Path, observation.Mounts)
		for _, mount := range observation.Mounts {
			if mount != owner && !files.Within(path.Clean(mount.Destination), root.Path) {
				continue
			}
			source := mount.Source
			if mount.Type == "volume" && mount.Name != "" {
				source = mount.Name
			}
			observation.DataFolders = append(observation.DataFolders, identity.DataFolder{Path: root.Path, Type: mount.Type,
				Source: source, Destination: mount.Destination, ReadOnly: !mount.Writable, Subpath: mount.Subpath})
		}
	}
	credentials := map[string]bool{}
	if adapter := games.ResolveAdapter(container.GameType, container.Labels); adapter != nil {
		for _, name := range adapter.PasswordEnvCandidates {
			credentials[name] = true
		}
	}
	if name := labels[games.LabelConsolePasswordEnv]; name != "" {
		credentials[name] = true
	}
	observation.Secrets = secrets(info.Config.Env, labels, credentials, container.Labels)
	return observation
}

// secretEnv names environment variables that hold credentials, beyond the
// names logging already treats as sensitive.
var secretEnv = regexp.MustCompile(`(?i)pass|pwd|private|salt|webhook|gslt|(^|_)(key|pin|code)($|_)`)

// secrets lists the values console and log output must not show: credential
// environment variables by name, the console password, variables named by
// ludock.secret-env, sensitive configuration labels, and passwords inside
// URLs. Containers print their configuration in ways Ludock can't predict,
// so a value is redacted wherever it appears.
func secrets(env []string, labels map[string]string, credentials map[string]bool, ludockLabels map[string]string) []string {
	listed := map[string]bool{}
	for _, name := range strings.Split(labels[games.LabelSecretEnv], ",") {
		listed[strings.TrimSpace(name)] = true
	}
	seen := map[string]bool{}
	var values []string
	add := func(value string, guessed bool) {
		// A guessed name can hold a flag such as "true" or "1"; hiding it
		// would hide ordinary output and protect nothing.
		if value == "" || seen[value] || (guessed && (len(value) < 3 || plainValue[strings.ToLower(value)])) {
			return
		}
		seen[value] = true
		values = append(values, value)
	}
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if credentials[key] || listed[key] || logging.IsSensitiveKey(key) {
			add(value, false)
		} else if secretEnv.MatchString(key) && key != "PWD" && key != "OLDPWD" {
			add(value, true)
		}
		if parsed, err := url.Parse(value); err == nil && parsed.User != nil {
			if password, ok := parsed.User.Password(); ok {
				// The container may print the URL as written, escapes and all.
				add(password, false)
				add(escapedPassword(value), false)
			}
		}
	}
	for key, value := range ludockLabels {
		if key != games.LabelConsolePasswordEnv && key != games.LabelSecretEnv && logging.IsSensitiveKey(key) {
			add(value, false)
		}
	}
	sortByLengthDesc(values)
	return values
}

// escapedPassword is the password of a URL with credentials as written in
// it, before percent-decoding. The authority follows the first "//", with
// or without a scheme before it.
func escapedPassword(value string) string {
	_, authority, _ := strings.Cut(value, "//")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	userinfo := authority[:max(0, strings.LastIndex(authority, "@"))]
	_, password, _ := strings.Cut(userinfo, ":")
	return password
}

var plainValue = map[string]bool{"true": true, "false": true, "yes": true, "off": true, "none": true, "null": true}

type Stats struct {
	CPUPercent float64
	MemUsageMB float64
	// MemLimitMB is the host's memory when the container has no limit of
	// its own (MemLimited false).
	MemLimitMB float64
	MemLimited bool
}

// round2 rounds like Math.round(value * 100) / 100.
func round2(value float64) float64 { return math.Floor(value*100+0.5) / 100 }

// Stats inspects, authorizes, and samples a container. The inspection and
// access check are the caller's own; the sample may be shared.
func (o *Observer) Stats(ctx context.Context, id string, assertAccess func(identity.Observation) error) (*Stats, error) {
	observed, info, err := o.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := assertAccess(observed.Observation); err != nil {
		return nil, err
	}
	shared, err := o.sample(ctx, id)
	if err != nil {
		return nil, err
	}
	stats := *shared
	stats.MemLimited = info.Host.Memory > 0
	return &stats, nil
}

func (o *Observer) sample(ctx context.Context, id string) (*Stats, error) {
	now := o.now()
	o.samplesMu.Lock()
	current := o.samples[id]
	if current != nil {
		select {
		case <-current.done:
			if current.err != nil || now.Sub(current.taken) >= statsReuse {
				current = nil
			}
		default:
		}
	}
	if current == nil {
		if o.samples == nil {
			o.samples = map[string]*statsSample{}
		}
		for key, sample := range o.samples {
			select {
			case <-sample.done:
				if now.Sub(sample.taken) >= statsReuse {
					delete(o.samples, key)
				}
			default:
			}
		}
		current = &statsSample{done: make(chan struct{})}
		o.samples[id] = current
		// The shared sample must not be cancelled by the first viewer leaving.
		go func(ctx context.Context) {
			current.stats, current.err = o.readStats(ctx, id)
			current.taken = o.now()
			close(current.done)
		}(context.WithoutCancel(ctx))
	}
	o.samplesMu.Unlock()
	select {
	case <-current.done:
		return current.stats, current.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (o *Observer) readStats(ctx context.Context, id string) (*Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stats, err := o.Docker.Stats(ctx, id)
	if err != nil {
		return nil, err
	}
	cpuDelta := stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage
	systemDelta := stats.CPUStats.SystemCPUUsage - stats.PreCPUStats.SystemCPUUsage
	cpus := stats.CPUStats.OnlineCPUs
	if cpus == 0 {
		cpus = 1
	}
	cpu := 0.0
	if systemDelta > 0 {
		cpu = cpuDelta / systemDelta * cpus * 100
	}
	return &Stats{CPUPercent: round2(cpu), MemUsageMB: round2(memoryInUse(stats) / (1024 * 1024)),
		MemLimitMB: round2(stats.MemoryStats.Limit / (1024 * 1024))}, nil
}

// memoryInUse matches docker stats: Docker's usage includes file cache the
// kernel can reclaim, reported as total_inactive_file on cgroup v1 and
// inactive_file on v2.
func memoryInUse(stats *docker.ContainerStats) float64 {
	memory := stats.MemoryStats
	inactive, cgroupV1 := memory.Stats["total_inactive_file"]
	if !cgroupV1 {
		inactive = memory.Stats["inactive_file"]
	}
	if inactive < memory.Usage {
		return memory.Usage - inactive
	}
	return memory.Usage
}

type Action string

const (
	Start   Action = "start"
	Stop    Action = "stop"
	Restart Action = "restart"
)

// ChangeState inspects and authorizes the current container immediately
// before acting on it. Docker's 304 means start/stop was already satisfied,
// which is not a failure.
func (o *Observer) ChangeState(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error) error {
	return o.ChangeStateUnsent(ctx, id, action, assertAccess, nil)
}

// ChangeStateUnsent is ChangeState that calls unsent, when not nil, as soon
// as a request is known never to have reached Docker, before any retry.
func (o *Observer) ChangeStateUnsent(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error, unsent func() error) error {
	if err := o.changeStateExact(ctx, id, action, assertAccess, unsent); !errors.Is(err, ErrUnchanged) {
		return err
	}
	return nil
}

// ErrUnchanged: Docker found the container already in the requested state,
// such as one stopped by another tool after the final inspection.
var ErrUnchanged = errors.New("container already in the requested state")

// ChangeStateExact is ChangeState, reporting ErrUnchanged. Under
// docker.RideOut, a request that never reached Docker is made again from a
// fresh inspection and check once Docker answers, and one whose answer was
// lost counts as done if Docker then shows the state it asked for.
func (o *Observer) ChangeStateExact(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error) error {
	return o.changeStateExact(ctx, id, action, assertAccess, nil)
}

// ChangeStateExactUnsent is ChangeStateExact that calls unsent as
// ChangeStateUnsent does.
func (o *Observer) ChangeStateExactUnsent(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error, unsent func() error) error {
	return o.changeStateExact(ctx, id, action, assertAccess, unsent)
}

func (o *Observer) changeStateExact(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error, unsent func() error) error {
	var since time.Time
	for {
		before, err := o.changeState(ctx, id, action, assertAccess)
		if before != nil && docker.NotSent(err) && unsent != nil {
			if unsentErr := unsent(); unsentErr != nil {
				return unsentErr
			}
		}
		if before == nil || !docker.Unavailable(err) || !o.Docker.WaitForDocker(ctx, err, &since) {
			return err
		}
		if docker.NotSent(err) {
			continue
		}
		after, inspectErr := o.Docker.Inspect(ctx, id)
		if inspectErr != nil {
			return err
		}
		// A stopped run that started after the check is another manager's.
		running := after.State.Running || after.State.Restarting
		sameRun := after.State.StartedAt == before.State.StartedAt
		if (action == Stop && !running && sameRun) || (action == Start && running && !sameRun) {
			return nil
		}
		return err
	}
}

// changeState returns the inspection the change was dispatched after, or nil
// if it wasn't.
func (o *Observer) changeState(ctx context.Context, id string, action Action,
	assertAccess func(identity.Observation, *docker.ContainerInspect) error) (*docker.ContainerInspect, error) {
	observed, info, err := o.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if action != Start && info.Host.AutoRemove {
		return nil, ErrAutoRemove
	}
	if assertAccess != nil {
		if err := assertAccess(observed.Observation, info); err != nil {
			return nil, err
		}
	}
	timeout := StopTimeoutSeconds(info)
	switch action {
	case Start:
		err = o.Docker.Start(ctx, id)
	case Stop:
		err = o.stop(ctx, id, info, games.StopCommand(observed.Container.GameType, observed.Container.Labels), timeout)
	case Restart:
		err = o.Docker.Restart(ctx, id, &timeout)
	default:
		return nil, errors.New("unknown container action")
	}
	if action != Restart && docker.StatusCode(err) == http.StatusNotModified {
		return info, ErrUnchanged
	}
	return info, err
}

// stopCommandDelay lets Docker take the stop first: once it has sent the
// stop signal it no longer applies the restart policy, so a game that exits
// on its own command stays stopped.
var stopCommandDelay = 2 * time.Second

// stop asks Docker to stop the container and, for a game that never gets the
// stop signal, also types its shutdown command, so it exits saved instead of
// being killed when the grace period ends.
func (o *Observer) stop(ctx context.Context, id string, info *docker.ContainerInspect, command string, timeout int) error {
	if command == "" || !info.Config.OpenStdin || info.Config.StdinOnce {
		return o.Docker.Stop(ctx, id, &timeout)
	}
	done := make(chan error, 1)
	go func() { done <- o.Docker.Stop(ctx, id, &timeout) }()
	select {
	case err := <-done:
		return err
	case <-time.After(stopCommandDelay):
	}
	// Best effort: without it, Docker still kills the game when the grace
	// period ends. Docker binds the stream to the current run, so only the
	// run being stopped, checked once attached, gets the command.
	attach, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if conn, err := o.Docker.Attach(attach, id, docker.AttachOptions{Stream: true, Stdin: true}); err == nil {
		stop := context.AfterFunc(attach, func() { conn.Close() })
		// A restarting container keeps its old start time until the next run.
		if current, err := o.Docker.Inspect(attach, id); err == nil && current.State.Running && !current.State.Restarting &&
			current.State.StartedAt == info.State.StartedAt {
			conn.Write([]byte(command + "\n"))
		}
		stop()
		conn.Close()
	}
	return <-done
}

// StopTimeoutSeconds is the game's shutdown allowance, or the container's
// own if that is longer. Docker's -1 waits indefinitely and is preserved.
func StopTimeoutSeconds(info *docker.ContainerInspect) int {
	configured := info.Config.StopTimeout
	if configured != nil && *configured == -1 {
		return -1
	}
	preset := games.Lookup(games.TypeFor(info.Config.Image, info.Config.Labels)).StopTimeout()
	if configured != nil && *configured > preset {
		return *configured
	}
	return preset
}

// Startup recovery is bounded even if Docker never received the request or
// the owner configured an indefinite stop.
const stopRecoveryMin = 5 * time.Minute

// StopRecovery is persisted from the final inspection immediately before a
// stop is dispatched.
type StopRecovery struct {
	StopStarted            bool
	StopInitialStartedAt   *int64
	StopRecoveryDeadlineAt int64
}

// StopRecoveryState waits through the full grace period plus time to kill
// the process.
func StopRecoveryState(info *docker.ContainerInspect, now time.Time) StopRecovery {
	running := IsRunning(info)
	state := StopRecovery{StopStarted: running}
	if running {
		if started := dockerTime(info.State.StartedAt); started > 0 {
			state.StopInitialStartedAt = &started
		}
	}
	wait := time.Duration(StopTimeoutSeconds(info)+60) * time.Second
	state.StopRecoveryDeadlineAt = now.Add(max(stopRecoveryMin, wait)).UnixMilli()
	return state
}

// Diagnostic explains an excluded or misconfigured container to
// administrators. It never includes the untrusted label value.
type Diagnostic struct {
	ContainerID string
	Name        string
	Code        string
	Message     string
}

func (o *Observer) Diagnostics(ctx context.Context) ([]Diagnostic, error) {
	containers, err := o.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	var result []Diagnostic
	for _, container := range containers {
		labels := container.Labels
		eligibility := games.Evaluate(container.Image, labels)
		name := ""
		if len(container.Names) > 0 {
			name = strings.TrimPrefix(container.Names[0], "/")
		}
		diagnostic := func(code, message string) {
			result = append(result, Diagnostic{container.ID, name, code, message})
		}
		invalidIdentity := eligibility.Eligible && games.InvalidComposeIdentity(labels)
		address, hasAddress := labels[games.LabelAddress]
		switch {
		case eligibility.Reason == games.ReasonInvalidEnableLabel:
			diagnostic("INVALID_ENABLE_LABEL", "Not added: its ludock.enable label must be true or false.")
		case invalidIdentity:
			diagnostic("INVALID_COMPOSE_IDENTITY", "Not added: its Compose labels name a project, service, or replica number but not all three. Fix them where the container is defined.")
		case eligibility.Eligible && hasAddress && games.ParseAddress(address) == nil:
			diagnostic("INVALID_ADDRESS_LABEL", "Its ludock.address label is ignored: use a host name or IP address with an optional port, such as play.example.com:25565 or [2001:db8::1]:2456. The detected address is shown instead.")
		}
	}
	return result, nil
}
