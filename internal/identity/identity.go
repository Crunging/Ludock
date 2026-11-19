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
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/textutil"
)

type ComposeIdentity struct {
	Project         string `json:"project"`
	Service         string `json:"service"`
	ContainerNumber string `json:"containerNumber"`
}

type Mount struct {
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Writable    bool   `json:"writable"`
	Name        string `json:"name,omitempty"`
}

func DockerMounts(points []docker.MountPoint) []Mount {
	var mounts []Mount
	for _, point := range points {
		mounts = append(mounts, Mount{Type: point.Type, Source: point.Source, Destination: point.Destination, Writable: point.RW, Name: point.Name})
	}
	return mounts
}

// Observation is one inspected container that Ludock can manage.
type Observation struct {
	ContainerID string           `json:"containerId"`
	Name        string           `json:"name"`
	DisplayName string           `json:"displayName"`
	GameType    string           `json:"gameType"`
	Compose     *ComposeIdentity `json:"compose,omitempty"`
	Mounts      []Mount          `json:"mounts"`
	// DataFolders are input only: they are keyed-hashed, never stored or
	// returned by this package.
	DataFolders []DataFolder `json:"dataFolders,omitempty"`
	// Secrets are values to redact from console and log output, longest
	// first. They are not part of the identity and are never stored.
	Secrets []string `json:"-"`
}

// DataFolder is a folder Ludock reads, backs up, and restores, with the
// mount that holds it. Source is the volume name or the host path.
type DataFolder struct {
	Path        string `json:"path"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"readOnly"`
}

type Status string

const (
	StatusActive    Status = "active"
	StatusMissing   Status = "missing"
	StatusAmbiguous Status = "ambiguous"
)

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
	// SharingPaused is set when the game or data folders changed: people the
	// server is shared with can't use it until an administrator confirms.
	SharingPaused bool
	FirstSeenAt   int64
	LastSeenAt    int64
}

type Store struct{ DB *db.DB }

// Matches session activity: coarse enough that polling causes no routine writes.
const lastSeenWriteInterval = 60_000

var replicaPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// ValidID reports whether value is a logical server ID (a UUID). Lists
// check every row's IDs, so these checks avoid regular expressions.
func ValidID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := range len(value) {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value[index] != '-' {
				return false
			}
		} else if !isHex(value[index]) {
			return false
		}
	}
	return true
}

// ValidContainerID reports whether value is an Engine container reference:
// up to 128 letters, digits, "_", ".", or "-", starting with a letter or
// digit.
func ValidContainerID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index := range len(value) {
		char := value[index]
		alphanumeric := char >= '0' && char <= '9' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
		if !alphanumeric && (index == 0 || char != '_' && char != '.' && char != '-') {
			return false
		}
	}
	return true
}

func isHex(char byte) bool {
	return char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F'
}

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

// BindingDigest is the unkeyed digest of what makes a server the same in
// substance: its game and the data folders Ludock reads, backs up, and
// restores. Anything else in the container can change without a review.
func BindingDigest(o Observation) string {
	folders := []DataFolder{}
	for _, folder := range o.DataFolders {
		folders = append(folders, DataFolder{path.Clean(folder.Path), folder.Type, path.Clean(folder.Source), path.Clean(folder.Destination),
			folder.ReadOnly})
	}
	slices.SortFunc(folders, func(a, b DataFolder) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Destination, b.Destination), cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Source, b.Source))
	})
	encoded, _ := json.Marshal(struct {
		GameType    string       `json:"gameType"`
		DataFolders []DataFolder `json:"dataFolders"`
	}{o.GameType, folders})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// BindingFingerprint is the keyed binding fingerprint stored for a server.
func (s *Store) BindingFingerprint(o Observation) (string, error) {
	return s.DB.BindingFingerprint(BindingDigest(o))
}

const serverColumns = `id, host_id, external_identity, container_id, display_name, game_type, status,
  binding_revision, binding_fingerprint, review_required, first_seen_at, last_seen_at`

type scanner interface{ Scan(dest ...any) error }

// ServerColumns are the columns ScanServer reads, from the logical_servers
// table under alias.
func ServerColumns(alias string) string {
	return alias + "." + strings.ReplaceAll(strings.Join(strings.Fields(serverColumns), " "), ", ", ", "+alias+".")
}

// ScanServer reads ServerColumns into a server after the extra destinations.
func ScanServer(row scanner, extra ...any) (*Server, error) {
	return scanServer(row, extra...)
}

func scanServer(row scanner, extra ...any) (*Server, error) {
	var server Server
	var container sql.NullString
	var status string
	var paused int
	if err := row.Scan(append(extra, &server.ID, &server.HostID, &server.ExternalIdentity, &container, &server.DisplayName,
		&server.GameType, &status, &server.BindingRevision, &server.BindingFingerprint, &paused,
		&server.FirstSeenAt, &server.LastSeenAt)...); err != nil {
		return nil, err
	}
	if !ValidID(server.ID) || (container.Valid && !ValidContainerID(container.String)) {
		return nil, bindingError("INVALID_STORED_IDENTITY", "Stored server identity is invalid; review application data.", 500)
	}
	server.ContainerID = container.String
	server.Status, server.SharingPaused = Status(status), paused == 1
	return &server, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RemovedServer matches a logical_servers row an administrator removed in
// the panel whose container has since left discovery.
const RemovedServer = `status = 'missing' AND EXISTS (SELECT 1 FROM settings
  WHERE key = '` + choicePrefix + `' || external_identity AND json_extract(value_json, '$.managed') = 0)`

// listServers lists a host's servers, or every host's when hostID is "".
// Servers removed in the panel are left out unless withRemoved.
func listServers(ctx context.Context, q queryer, hostID string, withRemoved bool) ([]Server, error) {
	var conditions []string
	var args []any
	if hostID != "" {
		conditions, args = append(conditions, "host_id = ?"), append(args, hostID)
	}
	if !withRemoved {
		conditions = append(conditions, "NOT ("+RemovedServer+")")
	}
	query := "SELECT " + serverColumns + " FROM logical_servers"
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := q.QueryContext(ctx, query+" ORDER BY display_name COLLATE NOCASE, id", args...)
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

// List lists logical servers, optionally for one host. Servers an
// administrator removed are left out; Get still finds them.
func (s *Store) List(ctx context.Context, hostID string) ([]Server, error) {
	return listServers(ctx, s.DB, hostID, false)
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

// ResolveBinding returns the server only while exactly one container is
// bound to it. expectedRevision < 0 skips the revision check. Paused sharing
// is checked by authz, per user.
func (s *Store) ResolveBinding(ctx context.Context, id string, expectedRevision int64) (*Server, error) {
	server, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, bindingError("SERVER_NOT_FOUND", "Server not found", 404)
	}
	if server.Status != StatusActive || server.ContainerID == "" {
		return nil, bindingError("SERVER_BINDING_UNAVAILABLE", "Ludock can't find this server's container right now", 409)
	}
	if expectedRevision >= 0 && server.BindingRevision != expectedRevision {
		return nil, bindingError("SERVER_BINDING_CHANGED", "The server changed in Docker; reload and try again", 409)
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
		return nil, bindingError("SERVER_BINDING_CHANGED", "The server changed in Docker; reload and try again", 409)
	}
	return server, nil
}

// Reconcile applies one complete, successful snapshot of eligible containers
// for a host. A failed or partial listing must not make healthy servers
// missing, so callers only pass complete snapshots. now <= 0 means the
// current time. It returns the host's servers afterwards.
func (s *Store) Reconcile(ctx context.Context, observations []Observation, hostID string, now int64) ([]Server, error) {
	if hostID == "" {
		var err error
		if hostID, err = s.DockerHostID(ctx, "local"); err != nil {
			return nil, err
		}
	}
	if err := s.Apply(ctx, observations, hostID, now); err != nil {
		return nil, err
	}
	return s.List(ctx, hostID)
}

// Apply is Reconcile without reading the servers back.
func (s *Store) Apply(ctx context.Context, observations []Observation, hostID string, now int64) error {
	if hostID == "" {
		var err error
		if hostID, err = s.DockerHostID(ctx, "local"); err != nil {
			return err
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
			return err
		}
		// Docker can never report one container twice in a coherent snapshot.
		if seen[o.ContainerID] {
			return bindingError("INVALID_SERVER_SNAPSHOT", "Duplicate container in Docker snapshot", 409)
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
				return err
			}
			fingerprints[key] = fingerprint
		}
	}
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		existing, err := listServers(ctx, tx, hostID, true)
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
					if err := recordBinding(ctx, tx, id, 1, o.ContainerID, fingerprint, now); err != nil {
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
			changed := server.ContainerID != o.ContainerID || server.BindingFingerprint != fingerprint
			paused := server.SharingPaused
			// A new game or other data folders make it a different server in
			// substance, which nobody it was shared with has seen yet.
			if server.BindingFingerprint != "" && fingerprint != server.BindingFingerprint && !paused {
				if paused, err = sharedWithOthers(ctx, tx, server.ID); err != nil {
					return err
				}
			}
			if !changed && seenRecently && server.DisplayName == o.DisplayName && server.Status == StatusActive &&
				server.GameType == o.GameType && paused == server.SharingPaused {
				continue
			}
			revision := server.BindingRevision
			if changed {
				revision++
			}
			if _, err := tx.ExecContext(ctx, `UPDATE logical_servers SET container_id = ?, display_name = ?, game_type = ?,
          status = 'active', binding_revision = ?, binding_fingerprint = ?, review_required = ?, last_seen_at = ? WHERE id = ?`,
				o.ContainerID, o.DisplayName, o.GameType, revision, fingerprint, paused, now, server.ID); err != nil {
				return err
			}
			if changed {
				if err := recordBinding(ctx, tx, server.ID, revision, o.ContainerID, fingerprint, now); err != nil {
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
	return err
}

func recordBinding(ctx context.Context, tx *sql.Tx, serverID string, revision int64, containerID, fingerprint string, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO server_bindings
    (server_id, binding_revision, container_id, binding_fingerprint, observed_at, accepted)
    VALUES (?, ?, ?, ?, ?, 1)`, serverID, revision, containerID, fingerprint, now)
	return err
}

func sharedWithOthers(ctx context.Context, tx *sql.Tx, serverID string) (bool, error) {
	var shared bool
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM server_grants WHERE server_id = ?)", serverID).Scan(&shared)
	return shared, err
}

// ResumeSharing gives back the access a change to the server's game or data
// folders paused, once an administrator confirms the server at
// expectedRevision. The administrator's session and role are checked again
// in the same transaction.
func (s *Store) ResumeSharing(ctx context.Context, adminID, tokenHash, id string, expectedRevision int64) error {
	return s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		server, err := getServer(ctx, tx, id)
		if err != nil {
			return err
		}
		if server == nil {
			return bindingError("SERVER_NOT_FOUND", "Server not found", 404)
		}
		if !server.SharingPaused {
			return nil
		}
		if server.Status != StatusActive || server.BindingRevision != expectedRevision {
			return bindingError("SERVER_CHANGED_AGAIN", "The server changed again in Docker; reload and check it before confirming", 409)
		}
		_, err = tx.ExecContext(ctx, "UPDATE logical_servers SET review_required = 0 WHERE id = ?", id)
		return err
	})
}
