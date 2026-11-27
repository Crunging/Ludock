package servers

import (
	"context"
	"database/sql"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
)

type Service struct {
	DB       *db.DB
	Identity *identity.Store
	Authz    *authz.Service
	Observer *Observer
	// Now returns the current time; tests replace it.
	Now func() time.Time
	// Looping reports whether a run of a server seen restarting is still
	// one of the loop's. It may be nil.
	Looping func(ctx context.Context, serverID string, container Container) bool

	mu      sync.Mutex
	running *refresh
	last    *refresh
	// discoveries counts discoveries started; each has its number as seq.
	discoveries uint64
}

type refresh struct {
	done    chan struct{}
	seq     uint64
	started time.Time
	writes  uint64
	result  map[string]Container
	err     error
}

const (
	// Reads reuse a discovery that started this recently, unless Ludock
	// changed Docker since. Docker events and mutations refresh immediately.
	discoveryReuse = 2 * time.Second
	// discoveryTimeout bounds one shared discovery, which no single caller
	// can cancel.
	discoveryTimeout = time.Minute
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Refresh discovers the fleet and reconciles logical servers. Concurrent
// callers share one discovery, and reads within a moment of it reuse it.
func (s *Service) Refresh(ctx context.Context) (map[string]Container, error) {
	return s.refresh(ctx, false)
}

// RefreshNow is Refresh from a discovery that starts after the call, for
// callers that must see a change that just happened.
func (s *Service) RefreshNow(ctx context.Context) (map[string]Container, error) {
	return s.refresh(ctx, true)
}

// refresh retries, under docker.RideOut, a discovery Docker cut short; the
// shared discovery itself never waits, so pages sharing it don't either.
func (s *Service) refresh(ctx context.Context, forced bool) (map[string]Container, error) {
	var since time.Time
	for {
		result, err := s.refreshOnce(ctx, forced)
		if err == nil || !docker.Unavailable(err) || !s.Observer.Docker.WaitForDocker(ctx, err, &since) {
			return result, err
		}
	}
}

func (s *Service) refreshOnce(ctx context.Context, forced bool) (map[string]Container, error) {
	s.mu.Lock()
	// A forced caller needs a discovery numbered after every one started
	// before the call.
	after := s.discoveries
	s.mu.Unlock()
	for {
		s.mu.Lock()
		last := s.last
		if !forced && last != nil && s.now().Sub(last.started) < discoveryReuse && last.writes == s.Observer.Docker.Writes() {
			s.mu.Unlock()
			return last.result, nil
		}
		current := s.running
		if current == nil {
			s.discoveries++
			current = &refresh{done: make(chan struct{}), seq: s.discoveries, started: s.now(), writes: s.Observer.Docker.Writes()}
			s.running = current
			s.mu.Unlock()
			s.discover(ctx, current)
			return current.result, current.err
		}
		s.mu.Unlock()
		select {
		case <-current.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if !forced || current.seq > after {
			return current.result, current.err
		}
	}
}

func (s *Service) discover(ctx context.Context, current *refresh) {
	defer func() {
		s.mu.Lock()
		s.running = nil
		if current.err == nil {
			s.last = current
		}
		s.mu.Unlock()
		close(current.done)
	}()
	// The shared discovery must not be cancelled by the first caller leaving.
	ctx, cancel := context.WithTimeout(docker.Promptly(context.WithoutCancel(ctx)), discoveryTimeout)
	defer cancel()
	observed, err := s.Observer.List(ctx)
	if err == nil {
		observations := make([]identity.Observation, len(observed))
		for index, entry := range observed {
			observations[index] = entry.Observation
		}
		if err = s.Identity.Apply(ctx, observations, "", 0); err == nil {
			current.result = map[string]Container{}
			for _, entry := range observed {
				current.result[entry.Container.ID] = entry.Container
			}
		}
	}
	current.err = err
}

const connectionHostSetting = "connection.host"

// ConnectionHost is the administrator-set public host, or "". A stored value
// that no longer validates is treated as unset.
func (s *Service) ConnectionHost(ctx context.Context) string {
	var value string
	if found, err := s.DB.Setting(ctx, connectionHostSetting, &value); err != nil || !found {
		return ""
	}
	host, ok := games.ConnectionHost(value)
	if !ok {
		return ""
	}
	return host
}

// SetConnectionHost stores or clears the public host, rejecting an invalid one.
func (s *Service) SetConnectionHost(ctx context.Context, adminID, tokenHash, host string) error {
	normalized := ""
	if strings.TrimSpace(host) != "" {
		var ok bool
		if normalized, ok = games.ConnectionHost(host); !ok {
			return apperr.New("INVALID_REQUEST", 400, "Enter a host name or IP address, such as play.example.com or 203.0.113.10.")
		}
	}
	// The administrator is checked again in the transaction that saves it.
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if normalized == "" {
			_, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key=?", connectionHostSetting)
			return err
		}
		return s.DB.SetSetting(ctx, tx, connectionHostSetting, normalized)
	})
}

// Connection is the address players use. A "label" source is the owner's
// exact ludock.address; "detected" uses the published game port, with an
// empty host until an administrator sets one.
type Connection struct {
	Host   string
	Port   int
	Source string
}

// ActiveOperation is persisted work that owns a server's controls.
type ActiveOperation struct {
	ID, Kind, Status, Phase string
	Input                   map[string]any
}

type BackupSummary struct {
	CreatedAt int64
	Size      int64
	// Reason is set on a safety backup, which is shown only when the server
	// has no other.
	Reason string
}

// View is a server as one actor may see it.
type View struct {
	Container
	ID              string
	Connection      *Connection
	BindingStatus   identity.Status
	BindingRevision int64
	// SharingPaused is set only on administrators' views.
	SharingPaused   bool
	Permissions     []authz.Capability
	ActiveOperation *ActiveOperation
	LatestBackup    *BackupSummary
	// NoDataFolders: Docker shows no writable data folder Ludock could back
	// up, which only the server's own definition can change.
	NoDataFolders bool
}

func (v *View) Can(capability authz.Capability) bool {
	return slices.Contains(v.Permissions, capability)
}

// details are the saved summaries a view shows, read once for a list.
type details struct {
	admin      bool
	host       *string
	operations map[string]*ActiveOperation
	backups    map[string]*BackupSummary
}

// readDetails reads summaries for one server, or every server when serverID
// is "". actor is the current account.
func (s *Service) readDetails(ctx context.Context, actor *db.SessionUser, serverID string) (*details, error) {
	d := &details{admin: actor != nil && actor.Role == db.RoleAdmin, operations: map[string]*ActiveOperation{},
		backups: map[string]*BackupSummary{}}
	query, args := `SELECT server_id,id,kind,status,phase,input_json FROM operations WHERE status IN ('queued','running')`, []any{}
	if serverID != "" {
		query, args = query+" AND server_id=?", append(args, serverID)
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var server, input string
		var operation ActiveOperation
		if err := rows.Scan(&server, &operation.ID, &operation.Kind, &operation.Status, &operation.Phase, &input); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(input), &operation.Input)
		if d.operations[server] == nil {
			d.operations[server] = &operation
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Each server's newest backup other than a safety backup, or else its
	// newest safety backup, walks backups_server_idx from the newest; retention
	// keeps few safety backups.
	query = `SELECT b.server_id,b.created_at,b.size,b.reason FROM logical_servers s JOIN backups b ON b.rowid=COALESCE(
    (SELECT rowid FROM backups WHERE server_id=s.id AND state='complete' AND reason='' ORDER BY created_at DESC,id LIMIT 1),
    (SELECT rowid FROM backups WHERE server_id=s.id AND state='complete' ORDER BY created_at DESC,id LIMIT 1))`
	if serverID != "" {
		query += " WHERE s.id=?"
	}
	rows, err = s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var server string
		var backup BackupSummary
		if err := rows.Scan(&server, &backup.CreatedAt, &backup.Size, &backup.Reason); err != nil {
			return nil, err
		}
		d.backups[server] = &backup
	}
	return d, rows.Err()
}

func (s *Service) view(ctx context.Context, d *details, logical identity.Server, permissions []authz.Capability, container *Container) *View {
	view := &View{ID: logical.ID, BindingStatus: logical.Status, BindingRevision: logical.BindingRevision, Permissions: permissions,
		SharingPaused: d.admin && logical.SharingPaused}
	if container != nil {
		view.Container = *container
		s.present(ctx, logical.ID, &view.Container)
	} else {
		view.Container = Container{Name: logical.DisplayName, DisplayName: logical.DisplayName, State: string(logical.Status),
			Status: strings.ReplaceAll(string(logical.Status), "_", " "), GameType: logical.GameType,
			GameName: games.DisplayName(logical.GameType), Created: logical.FirstSeenAt}
	}
	// Labels configure integration internals and are never shown.
	view.Labels = map[string]string{}
	if container != nil && container.AddressLabel != nil {
		view.Connection = &Connection{Host: container.AddressLabel.Host, Port: container.AddressLabel.Port, Source: "label"}
	} else if container != nil && container.ConnectPort > 0 {
		if d.host == nil {
			host := s.ConnectionHost(ctx)
			d.host = &host
		}
		view.Connection = &Connection{Host: *d.host, Port: container.ConnectPort, Source: "detected"}
	}
	view.ActiveOperation = d.operations[logical.ID]
	// Administrators keep backup history while a binding is unavailable. A
	// create grant shows only this summary, never archive data.
	if d.admin || slices.Contains(permissions, authz.BackupsRead) || slices.Contains(permissions, authz.BackupsCreate) {
		view.LatestBackup = d.backups[logical.ID]
	}
	view.NoDataFolders = container != nil && len(container.FileRoots) == 0
	if !slices.Contains(permissions, authz.FilesRead) {
		view.FileRoots = nil
	}
	return view
}

// List refreshes discovery and lists the servers the actor may view.
func (s *Service) List(ctx context.Context, actor *db.SessionUser) ([]View, error) {
	current, err := s.Refresh(ctx)
	if err != nil {
		return nil, err
	}
	return s.listViews(ctx, actor, current)
}

// ListSaved lists visible servers from saved metadata only, for a Docker
// outage.
func (s *Service) ListSaved(ctx context.Context, actor *db.SessionUser) ([]View, error) {
	return s.listViews(ctx, actor, nil)
}

func (s *Service) listViews(ctx context.Context, actor *db.SessionUser, current map[string]Container) ([]View, error) {
	logical, err := s.Identity.List(ctx, "")
	if err != nil {
		return nil, err
	}
	user, permissions, err := s.Authz.EffectiveAll(ctx, actor, logical)
	if err != nil || user == nil {
		return nil, err
	}
	d, err := s.readDetails(ctx, user, "")
	if err != nil {
		return nil, err
	}
	var views []View
	for _, server := range logical {
		if !slices.Contains(permissions[server.ID], authz.ServerView) {
			continue
		}
		var container *Container
		if found, ok := current[server.ContainerID]; ok && server.ContainerID != "" {
			container = &found
		}
		views = append(views, *s.view(ctx, d, server, permissions[server.ID], container))
	}
	return views, nil
}

type Name struct{ ID, DisplayName string }

// Names lists the servers the actor may view from saved metadata, without
// the summaries a View reads.
func (s *Service) Names(ctx context.Context, actor *db.SessionUser) ([]Name, error) {
	logical, err := s.Identity.List(ctx, "")
	if err != nil {
		return nil, err
	}
	_, permissions, err := s.Authz.EffectiveAll(ctx, actor, logical)
	if err != nil {
		return nil, err
	}
	var names []Name
	for _, server := range logical {
		if slices.Contains(permissions[server.ID], authz.ServerView) {
			names = append(names, Name{server.ID, server.DisplayName})
		}
	}
	return names, nil
}

// Sample reads a server's statistics for its resource history, after
// confirming the container is still the one bound to the server.
func (s *Service) Sample(ctx context.Context, server identity.Server) (*Stats, error) {
	return s.Observer.Stats(ctx, server.ContainerID, func(observation identity.Observation) error {
		_, err := s.Identity.AssertObservedBinding(ctx, server.ID, observation, server.BindingRevision)
		return err
	})
}

// Snapshot is a server detail read. Saved history stays inspectable during
// a Docker outage; a saved binding does not establish the current
// container's identity or live state.
type Snapshot struct {
	Server               *View
	Stats                *Stats
	DiscoveryUnavailable bool
}

// Snapshot reads one server with live statistics when they can be trusted.
func (s *Service) Snapshot(ctx context.Context, actor *db.SessionUser, id string) (*Snapshot, error) {
	return s.snapshot(ctx, actor, id, true)
}

// Summary is Snapshot without resource statistics, for pages that do not
// show them: a Docker stats sample takes about a second.
func (s *Service) Summary(ctx context.Context, actor *db.SessionUser, id string) (*Snapshot, error) {
	return s.snapshot(ctx, actor, id, false)
}

func (s *Service) snapshot(ctx context.Context, actor *db.SessionUser, id string, withStats bool) (*Snapshot, error) {
	current, refreshErr := s.Refresh(ctx)
	original, err := s.Authz.Assert(ctx, actor, id, authz.ServerView)
	if err != nil {
		return nil, err
	}
	var stats *Stats
	liveUnavailable := refreshErr != nil
	// Only a running container has resources to sample.
	if withStats && refreshErr == nil && original.Status == identity.StatusActive &&
		current[original.ContainerID].State == "running" {
		verified := false
		binding, err := s.Identity.ResolveBinding(ctx, id, original.BindingRevision)
		if err == nil {
			stats, err = s.Observer.Stats(ctx, binding.ContainerID, func(observation identity.Observation) error {
				if _, err := s.Authz.Assert(ctx, actor, id, authz.ServerView); err != nil {
					return err
				}
				if _, err := s.Identity.AssertObservedBinding(ctx, id, observation, binding.BindingRevision); err != nil {
					return err
				}
				verified = true
				return nil
			})
		}
		// A failed identity check also invalidates the earlier live snapshot.
		if err != nil {
			stats = nil
			if !verified {
				liveUnavailable = true
			}
		}
	}
	// Statistics involve more reads. Recheck access and the observed binding
	// before responding, without another Docker read.
	logical, err := s.Authz.Assert(ctx, actor, id, authz.ServerView)
	if err != nil {
		return nil, err
	}
	unavailable := liveUnavailable || logical.ContainerID != original.ContainerID ||
		logical.BindingRevision != original.BindingRevision || logical.BindingFingerprint != original.BindingFingerprint ||
		logical.Status != original.Status || logical.SharingPaused != original.SharingPaused
	permissions, err := s.Authz.Effective(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	user, err := s.Authz.CurrentActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	d, err := s.readDetails(ctx, user, id)
	if err != nil {
		return nil, err
	}
	var container *Container
	if found, ok := current[logical.ContainerID]; ok && !unavailable && logical.ContainerID != "" {
		container = &found
	}
	view := s.view(ctx, d, *logical, permissions, container)
	if unavailable {
		view.State, view.Status, view.Health, view.StateSince, view.Exit = "unknown", "Live status unavailable", "", 0, nil
		stats = nil
	}
	return &Snapshot{Server: view, Stats: stats, DiscoveryUnavailable: unavailable}, nil
}

// Context is a server confirmed against its own container for one action.
type Context struct {
	Logical     *identity.Server
	Container   Container
	Observation identity.Observation
	LockKeys    []string
}

// ResolveAuthorized refreshes discovery, then confirms the binding.
func (s *Service) ResolveAuthorized(ctx context.Context, actor *db.SessionUser, id string, capability authz.Capability, expectedRevision int64) (*Context, error) {
	if _, err := s.RefreshNow(ctx); err != nil {
		return nil, err
	}
	return s.VerifyAuthorized(ctx, actor, id, capability, expectedRevision)
}

// VerifyAuthorized confirms one saved binding against its own container
// without discovering the fleet. Docker events and the background tick still
// reconcile replacements and ambiguity. expectedRevision < 0 skips the pin.
func (s *Service) VerifyAuthorized(ctx context.Context, actor *db.SessionUser, id string, capability authz.Capability, expectedRevision int64) (*Context, error) {
	return s.verify(ctx, id, expectedRevision, func(ctx context.Context) error {
		_, err := s.Authz.Assert(ctx, actor, id, capability)
		return err
	})
}

// ResolveChecked is ResolveAuthorized for work that runs on Ludock's own
// authority, with allowed in place of an account's capability.
func (s *Service) ResolveChecked(ctx context.Context, id string, expectedRevision int64, allowed func(context.Context) error) (*Context, error) {
	if _, err := s.RefreshNow(ctx); err != nil {
		return nil, err
	}
	return s.verify(ctx, id, expectedRevision, allowed)
}

func (s *Service) verify(ctx context.Context, id string, expectedRevision int64, allowed func(context.Context) error) (*Context, error) {
	if err := allowed(ctx); err != nil {
		return nil, err
	}
	logical, err := s.Identity.ResolveBinding(ctx, id, expectedRevision)
	if err != nil {
		return nil, err
	}
	observed, _, err := s.Observer.Inspect(ctx, logical.ContainerID)
	if err != nil {
		return nil, err
	}
	if err := allowed(ctx); err != nil {
		return nil, err
	}
	if _, err := s.Identity.AssertObservedBinding(ctx, id, observed.Observation, logical.BindingRevision); err != nil {
		return nil, err
	}
	observed.Rename(logical.DisplayName)
	return &Context{Logical: logical, Container: observed.Container, Observation: observed.Observation,
		LockKeys: LockKeys(id, observed.Observation)}, nil
}

// Restarting reports a server Docker keeps restarting: restarting now, or in
// a run its restart policy started just after Ludock saw it restarting,
// until the run shows it is back.
func (s *Service) Restarting(ctx context.Context, serverID string, container Container) bool {
	if container.State == "restarting" {
		return true
	}
	return s.Looping != nil && container.State == "running" && container.RestartCount > 0 &&
		s.Observer.sawRestarting(container.ID, container.StateSince) && s.Looping(ctx, serverID, container)
}

// StillStarting reports a server whose game hasn't answered yet in a run
// that started within its monitoring grace period, judged as Looping does.
func (s *Service) StillStarting(ctx context.Context, serverID string) bool {
	if s.Looping == nil {
		return false
	}
	logical, err := s.Identity.ResolveBinding(ctx, serverID, -1)
	if err != nil {
		return false
	}
	observed, _, err := s.Observer.Inspect(ctx, logical.ContainerID)
	return err == nil && observed.Container.State == "running" && s.Looping(ctx, serverID, observed.Container)
}

// present shows a run between a crash loop's restarts as restarting, so a
// page reads the same whenever it looks.
func (s *Service) present(ctx context.Context, serverID string, container *Container) {
	if container.State == "running" && s.Restarting(ctx, serverID, *container) {
		container.State, container.Health, container.StateSince = "restarting", "", 0
	}
}

// Observe confirms a saved server against its own container for background
// work, which runs on Ludock's own authority. assert checks the saved binding
// again right before each later step.
func (s *Service) Observe(ctx context.Context, server identity.Server) (observed *Observed, assert func() error, err error) {
	observed, _, err = s.Observer.Inspect(ctx, server.ContainerID)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.Identity.AssertObservedBinding(ctx, server.ID, observed.Observation, server.BindingRevision); err != nil {
		return nil, nil, err
	}
	observed.Rename(server.DisplayName)
	return observed, func() error {
		_, err := s.Identity.ResolveBinding(ctx, server.ID, server.BindingRevision)
		return err
	}, nil
}

// LockKeys are the resources an operation on this server must hold: the
// server, its Compose project, and every writable data source.
func LockKeys(id string, observation identity.Observation) []string {
	keys := []string{locks.ServerKey(id)}
	if observation.Compose != nil {
		keys = append(keys, "project:"+observation.Compose.Project)
	}
	for _, mount := range observation.Mounts {
		if !mount.Writable {
			continue
		}
		if mount.Type == "volume" {
			name := mount.Name
			if name == "" {
				name = mount.Source
			}
			keys = append(keys, "volume:"+name)
			if path.IsAbs(mount.Source) {
				keys = append(keys, "path:"+path.Clean(mount.Source))
			}
		} else {
			keys = append(keys, "path:"+path.Clean(mount.Source))
		}
	}
	return keys
}
