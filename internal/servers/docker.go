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
	"regexp"
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
)

// Port is a published port.
type Port struct {
	Private int
	Public  int
	Type    string
}

// Exit describes how an exited container ended.
type Exit struct {
	Code      int
	OOMKilled bool
}

// Console summarizes a console adapter.
type Console struct {
	ID                 games.AdapterID
	Name               string
	CommandPlaceholder string
	Commands           []games.QuickCommand
	SupportsBroadcast  bool
}

// Container is the live state of a managed container.
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
	FileRoots   []files.Root
	Ports       []Port
	Created     int64
	// Labels holds approved configuration labels only.
	Labels map[string]string
}

// Observed pairs a container with its identity observation.
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

// Observer reads managed containers from Docker.
type Observer struct {
	Docker *docker.Client
	// Now returns the current time; tests replace it.
	Now func() time.Time

	mu          sync.Mutex
	inspections map[string]inspection

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

// List observes every eligible container. A failure of any inspection other
// than a vanished or forbidden container fails the whole listing, so callers
// never reconcile a partial snapshot.
func (o *Observer) List(ctx context.Context) ([]Observed, error) {
	containers, err := o.Docker.ListContainers(ctx, true, nil)
	if err != nil {
		return nil, err
	}
	var eligible []docker.ContainerSummary
	for _, container := range containers {
		if games.Evaluate(container.Image, container.Labels).Eligible && !isReplacement(container.Names) {
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
				observed, cached, err := o.observeSummary(ctx, eligible[index], previous)
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

func (o *Observer) observeSummary(ctx context.Context, container docker.ContainerSummary, previous map[string]inspection) (*Observed, *inspection, error) {
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
		cached = inspection{summary: summary, inspectedAt: now, info: info}
	}
	observed, err := observe(cached.info)
	if err != nil {
		return nil, nil, err
	}
	if summary == "" {
		return observed, nil, nil
	}
	return observed, &cached, nil
}

// Inspect observes one container directly. Mutations and their binding
// checks always use this, never the cache.
func (o *Observer) Inspect(ctx context.Context, id string) (*Observed, *docker.ContainerInspect, error) {
	if _, err := validContainerID(id); err != nil {
		return nil, nil, err
	}
	info, err := o.Docker.Inspect(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	observed, err := observe(info)
	return observed, info, err
}

func observe(info *docker.ContainerInspect) (*Observed, error) {
	if err := assertEligible(info); err != nil {
		return nil, err
	}
	container, err := toContainer(info)
	if err != nil {
		return nil, err
	}
	return &Observed{Container: *container, Observation: toObservation(info, container)}, nil
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

func toContainer(info *docker.ContainerInspect) (*Container, error) {
	id, err := validContainerID(info.ID)
	if err != nil {
		return nil, err
	}
	labels := info.Config.Labels
	gameType := strings.TrimSpace(labels[games.LabelGame])
	if gameType == "" {
		gameType = games.InferGameType(info.Config.Image)
	}
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
		container.Exit = &Exit{Code: info.State.ExitCode, OOMKilled: info.State.OOMKilled}
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
	if adapter := games.ResolveAdapter(gameType, container.Labels); adapter != nil {
		container.Console = &Console{ID: adapter.ID, Name: adapter.Name, CommandPlaceholder: adapter.CommandPlaceholder,
			Commands: adapter.Commands, SupportsBroadcast: games.BroadcastTemplate(gameType, container.Labels) != ""}
	}
	container.FileRoots = files.Roots(gameType, container.Labels, fileMounts(info.Mounts))
	return container, nil
}

func fileMounts(mounts []docker.MountPoint) []files.Mount {
	var result []files.Mount
	for _, mount := range mounts {
		result = append(result, files.Mount{Type: mount.Type, Source: mount.Source, Destination: mount.Destination, RW: mount.RW, Name: mount.Name})
	}
	return result
}

// toObservation is internal only: raw mounts and configuration are hashed by
// the identity package, never shown.
func toObservation(info *docker.ContainerInspect, container *Container) identity.Observation {
	labels := info.Config.Labels
	observation := identity.Observation{ContainerID: container.ID, Name: container.Name, DisplayName: container.DisplayName,
		GameType: container.GameType, GameConfiguration: map[string]string{}}
	if project, service, number, ok := games.ComposeLabels(labels); ok {
		observation.Compose = &identity.ComposeIdentity{Project: project, Service: service, ContainerNumber: number}
	}
	adapter := games.ResolveAdapter(container.GameType, container.Labels)
	credentials := map[string]bool{}
	if adapter != nil {
		for _, name := range adapter.PasswordEnvCandidates {
			credentials[name] = true
		}
	}
	if name := labels[games.LabelConsolePasswordEnv]; name != "" {
		credentials[name] = true
	}
	for key, value := range container.Labels {
		// Presentation labels can change without a review of the identity,
		// and so can a longer list of values to redact.
		if key != games.LabelEnable && key != games.LabelName && key != games.LabelAddress && key != games.LabelSecretEnv {
			observation.GameConfiguration[key] = value
		}
	}
	for _, entry := range info.Config.Env {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if credentials[key] || logging.IsSensitiveKey(key) || key == "RCON_PORT" || key == "ENABLE_RCON" {
			observation.GameConfiguration["env:"+key] = value
		}
	}
	observation.Secrets = secrets(info.Config.Env, labels, credentials, observation.GameConfiguration)
	if adapter != nil && adapter.ID == games.StdinConsole {
		observation.GameConfiguration["stdin:open"] = strconv.FormatBool(info.Config.OpenStdin)
		observation.GameConfiguration["stdin:once"] = strconv.FormatBool(info.Config.StdinOnce)
	}
	for _, mount := range info.Mounts {
		observation.Mounts = append(observation.Mounts, identity.Mount{Type: mount.Type, Source: mount.Source,
			Destination: mount.Destination, Writable: mount.RW, Name: mount.Name})
	}
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
func secrets(env []string, labels map[string]string, credentials map[string]bool, configuration map[string]string) []string {
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
				add(password, true)
			}
		}
	}
	for key, value := range configuration {
		if !strings.HasPrefix(key, "env:") && key != games.LabelConsolePasswordEnv && logging.IsSensitiveKey(key) {
			add(value, false)
		}
	}
	sortByLengthDesc(values)
	return values
}

var plainValue = map[string]bool{"true": true, "false": true, "yes": true, "off": true, "none": true, "null": true}

// Stats are rounded resource figures.
type Stats struct {
	CPUPercent float64
	MemUsageMB float64
	MemLimitMB float64
}

// round2 rounds like Math.round(value * 100) / 100.
func round2(value float64) float64 { return math.Floor(value*100+0.5) / 100 }

// Stats inspects, authorizes, and samples a container. The inspection and
// access check are the caller's own; the sample may be shared.
func (o *Observer) Stats(ctx context.Context, id string, assertAccess func(identity.Observation) error) (*Stats, error) {
	observed, _, err := o.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := assertAccess(observed.Observation); err != nil {
		return nil, err
	}
	return o.sample(ctx, id)
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
	return &Stats{CPUPercent: round2(cpu), MemUsageMB: round2(stats.MemoryStats.Usage / (1024 * 1024)),
		MemLimitMB: round2(stats.MemoryStats.Limit / (1024 * 1024))}, nil
}

// Action is a container state change.
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
	observed, info, err := o.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if assertAccess != nil {
		if err := assertAccess(observed.Observation, info); err != nil {
			return err
		}
	}
	timeout := StopTimeoutSeconds(info)
	switch action {
	case Start:
		err = o.Docker.Start(ctx, id)
	case Stop:
		err = o.Docker.Stop(ctx, id, &timeout)
	case Restart:
		return o.Docker.Restart(ctx, id, &timeout)
	default:
		return errors.New("unknown container action")
	}
	if docker.StatusCode(err) == http.StatusNotModified {
		return nil
	}
	return err
}

// StopTimeoutSeconds is the game's shutdown allowance, or the container's
// own if that is longer. Docker's -1 waits indefinitely and is preserved.
func StopTimeoutSeconds(info *docker.ContainerInspect) int {
	configured := info.Config.StopTimeout
	if configured != nil && *configured == -1 {
		return -1
	}
	gameType := strings.TrimSpace(info.Config.Labels[games.LabelGame])
	if gameType == "" {
		gameType = games.InferGameType(info.Config.Image)
	}
	preset := games.Lookup(gameType).StopTimeout()
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
	StopStarted            bool   `json:"stopStarted"`
	StopInitialStartedAt   *int64 `json:"stopInitialStartedAt"`
	StopRecoveryDeadlineAt int64  `json:"stopRecoveryDeadlineAt"`
}

// StopRecoveryState waits through the full grace period plus time to kill
// the process.
func StopRecoveryState(info *docker.ContainerInspect, now time.Time) StopRecovery {
	running := info.State.Running || info.State.Status == "running"
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

// Diagnostics lists discovery problems.
func (o *Observer) Diagnostics(ctx context.Context) ([]Diagnostic, error) {
	containers, err := o.Docker.ListContainers(ctx, true, nil)
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
			diagnostic("INVALID_ENABLE_LABEL", "Container excluded: ludock.enable must be true or false (case-insensitive; surrounding spaces are allowed).")
		case invalidIdentity:
			diagnostic("INVALID_COMPOSE_IDENTITY", "Container excluded: Compose project, service, and replica number must form a complete identity. Correct the owning manager's metadata before managing this container.")
		case eligibility.Eligible && hasAddress && games.ParseAddress(address) == nil:
			diagnostic("INVALID_ADDRESS_LABEL", "ludock.address is ignored: use a host name or IP address with an optional port, such as play.example.com:25565 or [2001:db8::1]:2456. The detected address is shown instead.")
		}
	}
	return result, nil
}
