package servers

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/self"
)

// Why Ludock manages a server.
const (
	SourceImage = "image" // a recognized game image
	SourceLabel = "label" // the owner's ludock.enable: "true"
	SourcePanel = "panel" // an administrator added it in the panel
)

func (o *Observer) choices(ctx context.Context) (map[string]identity.Choice, error) {
	if o.Choices == nil {
		return nil, nil
	}
	return o.Choices.Choices(ctx)
}

// candidate is what decides whether an administrator's choice applies to a
// container.
type candidate struct {
	id, name, image string
	labels          map[string]string
	mounts          []docker.MountPoint
	privileged      bool
}

func summaryCandidate(container docker.ContainerSummary) candidate {
	name := ""
	if len(container.Names) > 0 {
		name = strings.TrimPrefix(container.Names[0], "/")
	}
	return candidate{id: container.ID, name: name, image: container.Image, labels: container.Labels, mounts: container.Mounts}
}

func inspectCandidate(info *docker.ContainerInspect) candidate {
	return candidate{id: info.ID, name: strings.TrimPrefix(info.Name, "/"), image: info.Config.Image, labels: info.Config.Labels,
		mounts: info.Mounts, privileged: info.Host.Privileged}
}

// external is the identity a choice is saved under, or "" when it has none.
func (c candidate) external() string {
	observation := identity.Observation{ContainerID: c.id, Name: c.name}
	if project, service, number, ok := games.ComposeLabels(c.labels); ok {
		observation.Compose = &identity.ComposeIdentity{Project: project, Service: service, ContainerNumber: number}
	}
	external, err := identity.ExternalIdentity(observation)
	if err != nil {
		return ""
	}
	return external
}

// Sockets that control a container engine. A container that mounts one, or
// a folder that holds one, can control every container on the host.
var engineSockets = []string{"docker.sock", "podman.sock", "containerd.sock", "crio.sock"}

// hostFolders can't be bound, nor can any folder that holds one.
var hostFolders = []string{"/run", "/var/run", "/run/containerd", "/run/podman", "/var/lib/docker", "/var/lib/containerd",
	"/var/lib/containers"}

// controlsHost reports a container that can take over its host: privileged,
// or given an engine socket, the host's root, or the folders holding them.
// Its console and files would give whoever holds them the whole host.
func (c candidate) controlsHost() bool {
	if c.privileged {
		return true
	}
	for _, mount := range c.mounts {
		for _, location := range []string{mount.Source, mount.Destination} {
			if location == "" {
				continue
			}
			if slices.Contains(engineSockets, path.Base(location)) {
				return true
			}
		}
		if mount.Type == "bind" && mount.Source != "" {
			source := path.Clean(mount.Source)
			for _, folder := range hostFolders {
				if source == "/" || folder == source || strings.HasPrefix(folder, source+"/") {
					return true
				}
			}
		}
	}
	return false
}

// unaddable says why the panel can never add a container, or "" when it can.
func (c candidate) unaddable() string {
	if _, owner := c.labels[games.LabelEnable]; owner {
		return "label"
	}
	if oneoff, ok := games.ParseBool(c.labels["com.docker.compose.oneoff"], true); ok && oneoff {
		return "one-off"
	}
	if games.InvalidComposeIdentity(c.labels) || c.external() == "" {
		return "identity"
	}
	if isReplacement([]string{c.name}) {
		return "update"
	}
	if repository := games.NormalizeImageRepository(c.image); repository == "ludock" || strings.HasSuffix(repository, "/ludock") {
		return "ludock"
	}
	if c.controlsHost() {
		return "host"
	}
	return ""
}

// withChoice is the container's labels with an administrator's choice
// applied beneath the owner's own: any ludock.enable label decides alone,
// a container that controls the host is left out without one, and a
// container the panel can't add is never added by it. panel reports a
// container the choice added.
func (c candidate) withChoice(choices map[string]identity.Choice) (labels map[string]string, panel bool) {
	if _, owner := c.labels[games.LabelEnable]; owner {
		return c.labels, false
	}
	if c.controlsHost() {
		return c.excluded(), false
	}
	choice, found := choices[c.external()]
	if !found || (choice.Managed && c.unaddable() != "") {
		return c.labels, false
	}
	if !choice.Managed {
		return c.excluded(), false
	}
	labels = maps.Clone(c.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[games.LabelEnable] = "true"
	if choice.Game != "" && strings.TrimSpace(labels[games.LabelGame]) == "" {
		labels[games.LabelGame] = choice.Game
	}
	return labels, true
}

func (c candidate) excluded() map[string]string {
	labels := maps.Clone(c.labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[games.LabelEnable] = "false"
	return labels
}

// ownLabels is info, without a host-controlling container's ludock.enable
// when its image may have set it. Docker merges an image's labels into its
// containers', and only the owner's own label can add such a container: one
// that differs from the image's. It fails closed when the image can't be
// read.
func (o *Observer) ownLabels(ctx context.Context, info *docker.ContainerInspect) *docker.ContainerInspect {
	value := info.Config.Labels[games.LabelEnable]
	if enable, ok := games.ParseBool(value, true); !ok || !enable || !inspectCandidate(info).controlsHost() {
		return info
	}
	image, err := o.Docker.InspectImageConfig(ctx, info.Image)
	if err == nil {
		var labels map[string]string
		if raw := image.Config["Labels"]; len(raw) == 0 || json.Unmarshal(raw, &labels) == nil {
			if inherited, set := labels[games.LabelEnable]; !set || inherited != value {
				return info
			}
		}
	}
	stripped := *info
	stripped.Config.Labels = maps.Clone(info.Config.Labels)
	delete(stripped.Config.Labels, games.LabelEnable)
	return &stripped
}

// applyChoice returns a copy of info whose labels include the choice, which
// is what every eligibility and integration decision reads. The copy is
// never sent to Docker.
func applyChoice(info *docker.ContainerInspect, choices map[string]identity.Choice) (*docker.ContainerInspect, string) {
	labels, panel := inspectCandidate(info).withChoice(choices)
	source := SourceImage
	if panel {
		source = SourcePanel
	} else if enable, ok := games.ParseBool(info.Config.Labels[games.LabelEnable], true); ok && enable {
		source = SourceLabel
	}
	// A choice that applies always adds ludock.enable, which was absent.
	if len(labels) == len(info.Config.Labels) {
		return info, source
	}
	applied := *info
	applied.Config.Labels = labels
	return &applied, source
}

// Managed reports whether Ludock manages an inspected container, including
// an administrator's choice.
func (o *Observer) Managed(ctx context.Context, info *docker.ContainerInspect) (bool, error) {
	choices, err := o.choices(ctx)
	if err != nil {
		return false, err
	}
	applied, _ := applyChoice(o.ownLabels(ctx, info), choices)
	return games.Evaluate(applied.Config.Image, applied.Config.Labels).Eligible, nil
}

// Unmanaged is a container Ludock can see but doesn't manage, which an
// administrator can add. Its text comes from Docker and is untrusted.
type Unmanaged struct {
	ID    string
	Name  string
	Image string
	State string
	// Ports are the published ports only.
	Ports []Port
	// Game is the suggested game type, or "unknown".
	Game string
	// External is the identity a choice is saved under; never shown.
	External string
	// Chosen: an administrator already decided about it, such as removing it.
	Chosen bool
	// ControlsHost: a game that can control the host, which only its owner's
	// label can add. It is listed to explain why it isn't a server.
	ControlsHost bool
}

// Unmanaged lists the containers an administrator can add, likely games
// first, and the games left out because they can control the host.
func (o *Observer) Unmanaged(ctx context.Context) ([]Unmanaged, error) {
	containers, err := o.listContainers(ctx)
	if err != nil {
		return nil, err
	}
	choices, err := o.choices(ctx)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	inspected := o.inspections
	o.mu.Unlock()
	var ids []string
	for _, container := range containers {
		c := summaryCandidate(container)
		labels, _ := c.withChoice(choices)
		reason := c.unaddable()
		// A label that adds a container Ludock doesn't manage may be its
		// image's, on a container that controls the host.
		labeledIn, _ := games.ParseBool(c.labels[games.LabelEnable], reason == "label")
		if reason != "" && reason != "host" && !labeledIn {
			continue
		}
		// A summary has no privileged flag. Discovery caches only the
		// containers it manages, so any other is inspected to find out.
		if games.Evaluate(c.image, labels).Eligible {
			if _, managed := inspected[container.ID]; managed || isReplacement(container.Names) {
				continue
			}
		}
		ids = append(ids, container.ID)
	}
	results := make([]*Unmanaged, len(ids))
	var failure error
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Bound daemon load as discovery does.
	work := make(chan int)
	for range min(4, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				entry, err := o.addable(ctx, ids[index], choices)
				mu.Lock()
				if err != nil && !apperr.Is(err, "NOT_ADDABLE") && docker.StatusCode(err) != http.StatusNotFound &&
					docker.StatusCode(err) != http.StatusForbidden && failure == nil {
					failure = err
				}
				results[index] = entry
				mu.Unlock()
			}
		}()
	}
	for index := range ids {
		work <- index
	}
	close(work)
	wg.Wait()
	if failure != nil {
		return nil, failure
	}
	var list []Unmanaged
	for _, entry := range results {
		if entry != nil {
			list = append(list, *entry)
		}
	}
	slices.SortFunc(list, func(a, b Unmanaged) int {
		if (a.Game == "unknown") != (b.Game == "unknown") {
			if a.Game == "unknown" {
				return 1
			}
			return -1
		}
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
	return list, nil
}

var errNotAddable = apperr.New("NOT_ADDABLE", 409, "This container can’t be added. Refresh the list and try again.")

// Addable inspects one container and confirms an administrator can add it.
func (o *Observer) Addable(ctx context.Context, id string) (*Unmanaged, error) {
	if _, err := validContainerID(id); err != nil {
		return nil, errNotAddable
	}
	choices, err := o.choices(ctx)
	if err != nil {
		return nil, err
	}
	entry, err := o.addable(ctx, id, choices)
	if docker.IsNotFound(err) || (err == nil && entry.ControlsHost) {
		return nil, errNotAddable
	}
	return entry, err
}

func (o *Observer) addable(ctx context.Context, id string, choices map[string]identity.Choice) (*Unmanaged, error) {
	info, err := o.Docker.Inspect(ctx, id)
	if err != nil {
		return nil, err
	}
	c := inspectCandidate(o.ownLabels(ctx, info))
	labels, _ := c.withChoice(choices)
	reason := c.unaddable()
	if (reason != "" && reason != "host") || games.Evaluate(c.image, labels).Eligible {
		return nil, errNotAddable
	}
	// An unmanaged container offers no folders, so Ludock's own isn't read.
	container, err := toContainer(info, self.Info{})
	if err != nil {
		return nil, errNotAddable
	}
	entry := &Unmanaged{ID: container.ID, Name: container.Name, Image: container.Image, State: container.State,
		External: c.external()}
	for _, port := range container.Ports {
		if port.Public > 0 {
			entry.Ports = append(entry.Ports, port)
		}
	}
	// Docker reports no live mappings for a stopped container.
	if len(entry.Ports) == 0 {
		for key, bindings := range info.Host.PortBindings {
			number, kind, _ := strings.Cut(key, "/")
			for _, binding := range bindings {
				if public := parseLeadingInt(binding.HostPort); public > 0 {
					entry.Ports = append(entry.Ports, Port{Private: parseLeadingInt(number), Public: public, Type: cmp.Or(kind, "tcp")})
				}
			}
		}
		slices.SortFunc(entry.Ports, func(a, b Port) int { return cmp.Or(cmp.Compare(a.Public, b.Public), cmp.Compare(a.Type, b.Type)) })
	}
	entry.Game = games.Suggest(container.Image, container.Name, gamePorts(entry.Ports))
	choice, chosen := choices[entry.External]
	if choice.Game != "" {
		entry.Game = choice.Game
	}
	entry.Chosen = chosen
	// Tools such as Portainer can control the host too; only games, or a
	// container once added in the panel, need explaining.
	if reason == "host" {
		if entry.Game == "unknown" && !chosen {
			return nil, errNotAddable
		}
		entry.ControlsHost = true
	}
	return entry, nil
}

func gamePorts(ports []Port) []games.Port {
	result := make([]games.Port, len(ports))
	for index, port := range ports {
		result[index] = games.Port{Port: port.Private, Protocol: port.Type}
	}
	return result
}

// Add makes Ludock manage a container an administrator picked from the
// unmanaged list. game is a game type, "unknown" for another game, or ""
// for the suggestion. It returns the new server after discovery.
func (s *Service) Add(ctx context.Context, adminID, tokenHash, containerID, game string, audit db.AuditEntry) (*identity.Server, error) {
	entry, err := s.Observer.Addable(ctx, containerID)
	if err != nil {
		return nil, err
	}
	switch {
	case game == "":
		game = entry.Game
	case game != "unknown":
		integration := games.Lookup(game)
		if integration == nil {
			return nil, apperr.New("INVALID_REQUEST", 400, "Choose a game from the list.")
		}
		game = integration.GameType
	}
	choice := identity.Choice{Managed: true}
	// The image's own game needs no choice, so recognition stays the same
	// whether the server was added by hand or found by itself.
	if game != games.InferGameType(entry.Image) {
		choice.Game = game
	}
	audit.Action, audit.Details = "server.added", map[string]any{"container": entry.Name, "gameType": game}
	if err := s.Identity.SetChoice(ctx, adminID, tokenHash, entry.External, choice, &audit, nil); err != nil {
		return nil, err
	}
	if _, err := s.RefreshNow(ctx); err != nil {
		return nil, err
	}
	return s.byExternal(ctx, entry.External)
}

func (s *Service) byExternal(ctx context.Context, external string) (*identity.Server, error) {
	list, err := s.Identity.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for index := range list {
		if list[index].ExternalIdentity == external {
			return &list[index], nil
		}
	}
	return nil, apperr.New("SERVER_NOT_FOUND", 404, "The container changed before it could be added. Refresh the list and try again.")
}

// Remove stops Ludock managing a server, as a ludock.enable: "false" label
// would: its sharing, schedules, and backups are kept for when it is added
// again, and it leaves the server list once its container is out of
// discovery.
func (s *Service) Remove(ctx context.Context, adminID, tokenHash, id string, audit db.AuditEntry) (*identity.Server, error) {
	current, err := s.RefreshNow(ctx)
	if err != nil {
		return nil, err
	}
	server, err := s.Identity.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, apperr.New("SERVER_NOT_FOUND", 404, "Server not found")
	}
	if container, ok := current[server.ContainerID]; ok && server.ContainerID != "" && container.Source == SourceLabel {
		return nil, apperr.New("KEPT_BY_LABEL", 409,
			`Its ludock.enable: "true" label keeps this server in Ludock. To take it out, set the label to "false" where the container is defined and recreate it.`)
	}
	audit.Action, audit.TargetType, audit.TargetID = "server.removed", "server", server.ID
	choices, err := s.Observer.choices(ctx)
	if err != nil {
		return nil, err
	}
	// The game picked when it was added is offered again with it.
	removed := identity.Choice{Game: choices[server.ExternalIdentity].Game}
	err = s.Identity.SetChoice(ctx, adminID, tokenHash, server.ExternalIdentity, removed, &audit, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM operations WHERE server_id = ? AND status IN ('queued','running')",
			id).Scan(&active); err != nil {
			return err
		}
		if active > 0 {
			return apperr.New("OPERATION_ACTIVE", 409, "Wait for the work in progress to finish, then remove it.")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, err := s.RefreshNow(ctx); err != nil {
		return nil, err
	}
	return server, nil
}
