// Package identity maps Docker containers to Ludock's logical servers. URL
// IDs are logical IDs; they never reach Docker without a confirmed binding.
package identity

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/textutil"
)

// ComposeIdentity is the Compose project, service, and replica number.
type ComposeIdentity struct {
	Project         string `json:"project"`
	Service         string `json:"service"`
	ContainerNumber string `json:"containerNumber"`
}

// Mount is one container mount as Docker reports it.
type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
	Name        string `json:"name,omitempty"`
}

// Observation is one inspected container that Ludock can manage.
type Observation struct {
	ContainerID string           `json:"containerId"`
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName"`
	GameType    string           `json:"gameType"`
	Compose     *ComposeIdentity `json:"compose,omitempty"`
	// ComposeSourceLabels are internal source hints, never shown.
	ComposeSourceLabels map[string]string `json:"-"`
	Mounts              []Mount           `json:"mounts"`
	// GameConfiguration is input only: values are keyed-hashed, never stored
	// or returned by this package.
	GameConfiguration map[string]string `json:"gameConfiguration,omitempty"`
}

// Status of a logical server's binding.
type Status string

const (
	StatusActive         Status = "active"
	StatusMissing        Status = "missing"
	StatusAmbiguous      Status = "ambiguous"
	StatusReviewRequired Status = "review_required"
)

// Server is a logical server.
type Server struct {
	ID                 string
	HostID             string
	ExternalIdentity   string
	ContainerID        string // empty when unbound
	DisplayName        string
	GameType           string
	Status             Status
	BindingRevision    int64
	BindingFingerprint string
	PendingFingerprint string
	PendingGameType    string
	ReviewRequired     bool
	FirstSeenAt        int64
	LastSeenAt         int64
}

// Store reads and reconciles logical servers.
type Store struct{ DB *db.DB }

// Matches session activity: coarse enough that polling causes no routine writes.
const lastSeenWriteInterval = 60_000

var (
	logicalIDPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	containerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
	replicaPattern     = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// ValidID reports whether value is a logical server ID (a UUID).
func ValidID(value string) bool { return logicalIDPattern.MatchString(value) }

// ValidContainerID reports whether value is an Engine container reference.
func ValidContainerID(value string) bool { return containerIDPattern.MatchString(value) }

// NewUUID returns a random version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func bindingError(code, message string, status int) *apperr.Error {
	return apperr.New(code, status, message)
}

// DockerHostID returns the ID of the named Docker host, creating it.
func (s *Store) DockerHostID(ctx context.Context, name string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, "SELECT id FROM docker_hosts WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = NewUUID()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO docker_hosts (id, name, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING",
		id, name, db.Now()); err != nil {
		return "", err
	}
	err = s.DB.QueryRowContext(ctx, "SELECT id FROM docker_hosts WHERE name = ?", name).Scan(&id)
	return id, err
}

func validIdentityPart(value string) bool {
	if value == "" || textutil.Length(value) > 512 {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool { return r < 32 || r == 127 }) < 0
}

// ExternalIdentity is the stable identity of an observation across container
// recreation.
func ExternalIdentity(o Observation) (string, error) {
	if !validIdentityPart(o.ContainerID) || !validIdentityPart(o.Name) {
		return "", bindingError("INVALID_SERVER_OBSERVATION", "Invalid Docker server identity", 409)
	}
	if c := o.Compose; c != nil {
		if !validIdentityPart(c.Project) || !validIdentityPart(c.Service) || !replicaPattern.MatchString(c.ContainerNumber) {
			return "", bindingError("INVALID_SERVER_OBSERVATION", "Incomplete Compose server identity", 409)
		}
		return "compose:" + url.QueryEscape(c.Project) + ":" + url.QueryEscape(c.Service) + ":" + c.ContainerNumber, nil
	}
	return "standalone:" + url.QueryEscape(strings.TrimPrefix(o.Name, "/")), nil
}

// BindingDigest is the unkeyed digest of what makes a binding material:
// game type, writable mounts, and game configuration.
func BindingDigest(o Observation) string {
	type mount struct {
		Type        string `json:"type"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	mounts := []mount{}
	for _, m := range o.Mounts {
		if !m.Writable {
			continue
		}
		source := path.Clean(m.Source)
		if m.Type == "volume" && m.Name != "" {
			source = m.Name
		}
		mounts = append(mounts, mount{m.Type, source, path.Clean(m.Destination)})
	}
	slices.SortFunc(mounts, func(a, b mount) int {
		return cmp.Or(cmp.Compare(a.Type, b.Type), cmp.Compare(a.Source, b.Source), cmp.Compare(a.Destination, b.Destination))
	})
	// encoding/json writes map keys sorted.
	configuration := o.GameConfiguration
	if configuration == nil {
		configuration = map[string]string{}
	}
	encoded, _ := json.Marshal(struct {
		GameType      string            `json:"gameType"`
		Mounts        []mount           `json:"mounts"`
		Configuration map[string]string `json:"configuration"`
	}{o.GameType, mounts, configuration})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// BindingFingerprint is the keyed binding fingerprint stored for a server.
func (s *Store) BindingFingerprint(o Observation) (string, error) {
	return s.DB.BindingFingerprint(BindingDigest(o))
}

const serverColumns = `id, host_id, external_identity, container_id, display_name, game_type, status,
  binding_revision, binding_fingerprint, pending_fingerprint, pending_game_type, review_required,
  first_seen_at, last_seen_at`

type scanner interface{ Scan(dest ...any) error }

func scanServer(row scanner) (*Server, error) {
	var server Server
	var container, pendingFingerprint, pendingGame sql.NullString
	var status string
	var review int
	if err := row.Scan(&server.ID, &server.HostID, &server.ExternalIdentity, &container, &server.DisplayName,
		&server.GameType, &status, &server.BindingRevision, &server.BindingFingerprint, &pendingFingerprint,
		&pendingGame, &review, &server.FirstSeenAt, &server.LastSeenAt); err != nil {
		return nil, err
	}
	if !ValidID(server.ID) || (container.Valid && !ValidContainerID(container.String)) {
		return nil, bindingError("INVALID_STORED_IDENTITY", "Stored server identity is invalid; review application data.", 500)
	}
	server.ContainerID, server.PendingFingerprint, server.PendingGameType = container.String, pendingFingerprint.String, pendingGame.String
	server.Status, server.ReviewRequired = Status(status), review == 1
	return &server, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func listServers(ctx context.Context, q queryer, hostID string) ([]Server, error) {
	query := "SELECT " + serverColumns + " FROM logical_servers ORDER BY display_name COLLATE NOCASE, id"
	var args []any
	if hostID != "" {
		query = "SELECT " + serverColumns + " FROM logical_servers WHERE host_id = ? ORDER BY display_name COLLATE NOCASE, id"
		args = append(args, hostID)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var servers []Server
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		servers = append(servers, *server)
	}
	return servers, rows.Err()
}

// List lists logical servers, optionally for one host.
func (s *Store) List(ctx context.Context, hostID string) ([]Server, error) {
	return listServers(ctx, s.DB, hostID)
}

// Get returns a logical server or nil.
func (s *Store) Get(ctx context.Context, id string) (*Server, error) {
	return getServer(ctx, s.DB, id)
}

func getServer(ctx context.Context, q queryer, id string) (*Server, error) {
	server, err := scanServer(q.QueryRowContext(ctx, "SELECT "+serverColumns+" FROM logical_servers WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return server, err
}

// ResolveBinding returns the server only while its binding is usable.
// expectedRevision < 0 skips the revision check.
func (s *Store) ResolveBinding(ctx context.Context, id string, expectedRevision int64) (*Server, error) {
	server, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, bindingError("SERVER_NOT_FOUND", "Server not found", 404)
	}
	if server.Status != StatusActive || server.ReviewRequired || server.ContainerID == "" {
		return nil, bindingError("SERVER_BINDING_UNAVAILABLE", "Server binding is unavailable or requires administrator review", 409)
	}
	if expectedRevision >= 0 && server.BindingRevision != expectedRevision {
		return nil, bindingError("SERVER_BINDING_CHANGED", "The server binding changed; reload before trying again", 409)
	}
	return server, nil
}

// AssertObservedBinding validates a fresh inspect immediately before a
// mutation. It never reconciles a replacement silently into an operation
// that was authorized for an older container.
func (s *Store) AssertObservedBinding(ctx context.Context, id string, o Observation, expectedRevision int64) (*Server, error) {
	server, err := s.ResolveBinding(ctx, id, expectedRevision)
	if err != nil {
		return nil, err
	}
	external, err := ExternalIdentity(o)
	if err != nil {
		return nil, err
	}
	fingerprint, err := s.BindingFingerprint(o)
	if err != nil {
		return nil, err
	}
	if server.ContainerID != o.ContainerID || server.ExternalIdentity != external || server.BindingFingerprint != fingerprint {
		return nil, bindingError("SERVER_BINDING_CHANGED", "Docker server identity changed; refresh and review before continuing", 409)
	}
	return server, nil
}

// Reconcile applies one complete, successful snapshot of eligible containers
// for a host. A failed or partial listing must not make healthy servers
// missing, so callers only pass complete snapshots. now <= 0 means the
// current time.
func (s *Store) Reconcile(ctx context.Context, observations []Observation, hostID string, now int64) ([]Server, error) {
	if hostID == "" {
		var err error
		if hostID, err = s.DockerHostID(ctx, "local"); err != nil {
			return nil, err
		}
	}
	if now <= 0 {
		now = db.Now()
	}
	groups := map[string][]Observation{}
	var order []string
	seen := map[string]bool{}
	for _, o := range observations {
		key, err := ExternalIdentity(o)
		if err != nil {
			return nil, err
		}
		// Docker can never report one container twice in a coherent snapshot.
		if seen[o.ContainerID] {
			return nil, bindingError("INVALID_SERVER_SNAPSHOT", "Duplicate container in Docker snapshot", 409)
		}
		seen[o.ContainerID] = true
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], o)
	}
	fingerprints := map[string]string{}
	for _, key := range order {
		if group := groups[key]; len(group) == 1 {
			fingerprint, err := s.BindingFingerprint(group[0])
			if err != nil {
				return nil, err
			}
			fingerprints[key] = fingerprint
		}
	}
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		existing, err := listServers(ctx, tx, hostID)
		if err != nil {
			return err
		}
		current := map[string]Server{}
		for _, server := range existing {
			current[server.ExternalIdentity] = server
		}
		for _, external := range order {
			group := groups[external]
			o := group[0]
			ambiguous := len(group) != 1
			server, found := current[external]
			if !found {
				id := NewUUID()
				containerID, gameType, status, revision, fingerprint := any(o.ContainerID), o.GameType, StatusActive, 1, fingerprints[external]
				if ambiguous {
					containerID, gameType, status, revision, fingerprint = nil, "unknown", StatusAmbiguous, 0, ""
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO logical_servers
          (id, host_id, external_identity, container_id, display_name, game_type,
           status, binding_revision, binding_fingerprint, first_seen_at, last_seen_at)
          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					id, hostID, external, containerID, o.DisplayName, gameType, string(status), revision, fingerprint, now, now); err != nil {
					return err
				}
				if !ambiguous {
					if err := recordBinding(ctx, tx, id, 1, o.ContainerID, fingerprint, now, true); err != nil {
						return err
					}
				}
				continue
			}
			// Every discovery reconciles the fleet. Skip rewriting an unchanged
			// row so routine polling does not commit to disk.
			seenRecently := now-server.LastSeenAt < lastSeenWriteInterval
			if ambiguous {
				if server.Status == StatusAmbiguous && server.ContainerID == "" && seenRecently {
					continue
				}
				if _, err := tx.ExecContext(ctx, "UPDATE logical_servers SET status = 'ambiguous', container_id = NULL, last_seen_at = ? WHERE id = ?",
					now, server.ID); err != nil {
					return err
				}
				continue
			}
			fingerprint := fingerprints[external]
			material := server.BindingFingerprint != "" && fingerprint != server.BindingFingerprint
			review := server.ReviewRequired || material
			changed := server.ContainerID != o.ContainerID
			if review {
				changed = changed || server.PendingFingerprint != fingerprint
			} else {
				changed = changed || server.BindingFingerprint != fingerprint
			}
			unchanged := !changed && seenRecently && server.DisplayName == o.DisplayName
			if review {
				unchanged = unchanged && server.Status == StatusReviewRequired && server.ReviewRequired && server.PendingGameType == o.GameType
			} else {
				unchanged = unchanged && server.Status == StatusActive && server.GameType == o.GameType
			}
			if unchanged {
				continue
			}
			revision := server.BindingRevision
			if changed {
				revision++
			}
			if review {
				_, err = tx.ExecContext(ctx, `UPDATE logical_servers SET container_id = ?, display_name = ?,
          status = 'review_required', binding_revision = ?, pending_fingerprint = ?,
          pending_game_type = ?, review_required = 1, last_seen_at = ? WHERE id = ?`,
					o.ContainerID, o.DisplayName, revision, fingerprint, o.GameType, now, server.ID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE logical_servers SET container_id = ?, display_name = ?, game_type = ?,
          status = 'active', binding_revision = ?, binding_fingerprint = ?, last_seen_at = ? WHERE id = ?`,
					o.ContainerID, o.DisplayName, o.GameType, revision, fingerprint, now, server.ID)
			}
			if err != nil {
				return err
			}
			if changed {
				if err := recordBinding(ctx, tx, server.ID, revision, o.ContainerID, fingerprint, now, !review); err != nil {
					return err
				}
			}
		}
		for _, server := range existing {
			if _, ok := groups[server.ExternalIdentity]; ok || (server.Status == StatusMissing && server.ContainerID == "") {
				continue
			}
			if _, err := tx.ExecContext(ctx, "UPDATE logical_servers SET status = 'missing', container_id = NULL WHERE id = ?", server.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.List(ctx, hostID)
}

func recordBinding(ctx context.Context, tx *sql.Tx, serverID string, revision int64, containerID, fingerprint string, now int64, accepted bool) error {
	acceptedValue := 0
	if accepted {
		acceptedValue = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO server_bindings
    (server_id, binding_revision, container_id, binding_fingerprint, observed_at, accepted)
    VALUES (?, ?, ?, ?, ?, ?)`, serverID, revision, containerID, fingerprint, now, acceptedValue)
	return err
}

// ReviewBinding accepts the exact pending binding an administrator reviewed.
// The caller must authorize an administrator and freshly reconcile Docker
// first. expectedRevision < 0 skips the revision pin.
func (s *Store) ReviewBinding(ctx context.Context, id, expectedFingerprint string, expectedRevision int64) (*Server, error) {
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		server, err := getServer(ctx, tx, id)
		if err != nil {
			return err
		}
		if server == nil {
			return bindingError("SERVER_NOT_FOUND", "Server not found", 404)
		}
		if server.Status != StatusReviewRequired || server.ContainerID == "" || server.PendingFingerprint == "" ||
			server.PendingFingerprint != expectedFingerprint || (expectedRevision >= 0 && server.BindingRevision != expectedRevision) {
			return bindingError("SERVER_REVIEW_CHANGED", "The pending server binding changed; reload the review", 409)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE logical_servers SET status = 'active', binding_revision = binding_revision + 1,
      binding_fingerprint = pending_fingerprint, game_type = pending_game_type,
      pending_fingerprint = NULL, pending_game_type = NULL, review_required = 0 WHERE id = ?`, id); err != nil {
			return err
		}
		return recordBinding(ctx, tx, id, server.BindingRevision+1, server.ContainerID, expectedFingerprint, db.Now(), true)
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}
