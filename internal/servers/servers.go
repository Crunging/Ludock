package servers

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
)

// Service combines Docker observation with logical servers and grants.
type Service struct {
	DB       *db.DB
	Identity *identity.Store
	Authz    *authz.Service
	Observer *Observer
	// Now returns the current time; tests replace it.
	Now func() time.Time

	mu      sync.Mutex
	running *refresh
	last    *refresh
}

type refresh struct {
	done    chan struct{}
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
	return s.refresh(ctx, discoveryReuse)
}

// RefreshNow is Refresh from a discovery that starts after the call, for
// callers that must see a change that just happened.
func (s *Service) RefreshNow(ctx context.Context) (map[string]Container, error) {
	return s.refresh(ctx, 0)
}

func (s *Service) refresh(ctx context.Context, reuse time.Duration) (map[string]Container, error) {
	since := s.now().Add(-reuse)
	for {
		s.mu.Lock()
		if last := s.last; last != nil && !last.started.Before(since) && last.writes == s.Observer.Docker.Writes() {
			s.mu.Unlock()
			return last.result, nil
		}
		current := s.running
		if current == nil {
			current = &refresh{done: make(chan struct{}), started: s.now(), writes: s.Observer.Docker.Writes()}
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
		// A discovery that started before the call may predate the change
		// a forced caller needs to see.
		if reuse > 0 || !current.started.Before(since) {
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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discoveryTimeout)
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

// SetConnectionHost stores or clears the public host.
// An invalid host is rejected rather than stored.
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
}

// BackupSummary is the newest complete backup.
type BackupSummary struct {
	CreatedAt int64
	Size      int64
}

// View is a server as one actor may see it.
type View struct {
	Container
	ID              string
	Connection      *Connection
	BindingStatus   identity.Status
	BindingRevision int64
	Permissions     []authz.Capability
	ActiveOperation *ActiveOperation
	LatestBackup    *BackupSummary
}

// Can reports a capability in the view.
func (v *View) Can(capability authz.Capability) bool {
	return slices.Contains(v.Permissions, capability)
}

func (s *Service) view(ctx context.Context, actor *db.SessionUser, logical identity.Server, permissions []authz.Capability, container *Container) (*View, error) {
	view := &View{ID: logical.ID, BindingStatus: logical.Status, BindingRevision: logical.BindingRevision, Permissions: permissions}
	if container != nil {
		view.Container = *container
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
		view.Connection = &Connection{Host: s.ConnectionHost(ctx), Port: container.ConnectPort, Source: "detected"}
	}
	var operation ActiveOperation
	err := s.DB.QueryRowContext(ctx, `SELECT id,kind,status,phase FROM operations
    WHERE server_id=? AND status IN ('queued','running') LIMIT 1`, logical.ID).Scan(&operation.ID, &operation.Kind, &operation.Status, &operation.Phase)
	if err == nil {
		view.ActiveOperation = &operation
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	// Administrators keep backup history while a binding is unavailable. A
	// create grant shows only this summary, never archive data.
	current, err := s.Authz.CurrentActor(ctx, actor)
	if err != nil {
		return nil, err
	}
	if (current != nil && current.Role == db.RoleAdmin) || slices.Contains(permissions, authz.BackupsRead) ||
		slices.Contains(permissions, authz.BackupsCreate) {
		var backup BackupSummary
		err := s.DB.QueryRowContext(ctx, `SELECT created_at,size FROM backups WHERE server_id=? AND state='complete'
      ORDER BY created_at DESC,id LIMIT 1`, logical.ID).Scan(&backup.CreatedAt, &backup.Size)
		if err == nil {
			view.LatestBackup = &backup
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if !slices.Contains(permissions, authz.FilesRead) {
		view.FileRoots = nil
	}
	return view, nil
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
	var views []View
	for _, server := range logical {
		permissions, err := s.Authz.Effective(ctx, actor, server.ID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(permissions, authz.ServerView) {
			continue
		}
		var container *Container
		if found, ok := current[server.ContainerID]; ok && server.ContainerID != "" {
			container = &found
		}
		view, err := s.view(ctx, actor, server, permissions, container)
		if err != nil {
			return nil, err
		}
		views = append(views, *view)
	}
	return views, nil
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
		logical.Status != original.Status || logical.ReviewRequired != original.ReviewRequired
	permissions, err := s.Authz.Effective(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	var container *Container
	if found, ok := current[logical.ContainerID]; ok && !unavailable && logical.ContainerID != "" {
		container = &found
	}
	view, err := s.view(ctx, actor, *logical, permissions, container)
	if err != nil {
		return nil, err
	}
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
	if _, err := s.Authz.Assert(ctx, actor, id, capability); err != nil {
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
	if _, err := s.Authz.Assert(ctx, actor, id, capability); err != nil {
		return nil, err
	}
	if _, err := s.Identity.AssertObservedBinding(ctx, id, observed.Observation, logical.BindingRevision); err != nil {
		return nil, err
	}
	return &Context{Logical: logical, Container: observed.Container, Observation: observed.Observation,
		LockKeys: LockKeys(id, observed.Observation)}, nil
}

// LockKeys are the resources an operation on this server must hold: the
// server, its Compose project, and every writable data source.
func LockKeys(id string, observation identity.Observation) []string {
	keys := []string{"server:" + id}
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
