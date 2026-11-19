package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/resources"
	"github.com/Crunging/Ludock/internal/servers"
)

const (
	// DiskInterval is how often a monitored server's data folders are
	// measured, in milliseconds. Each measurement starts a file helper per
	// folder.
	DiskInterval = 10 * 60_000
	// diskAlertingInterval applies while a folder alerts, so freed space
	// clears it soon.
	diskAlertingInterval = 60_000
	// Hysteresis is how many percentage points below its threshold a value
	// must fall to end an alert.
	Hysteresis = 5
)

// DiskSpace is one data folder's filesystem, as a helper measured it.
type DiskSpace struct {
	Path                   string
	Total, Available, Used int64
	// Failed: the folder couldn't be measured this time.
	Failed bool
}

// Folder is the last measurement of a data folder's disk and its alert.
type Folder struct {
	Path           string `json:"path"`
	TotalBytes     int64  `json:"total"`
	AvailableBytes int64  `json:"available"`
	UsedBytes      int64  `json:"used"`
	// MeasuredAt is the last successful measurement; 0 when none.
	MeasuredAt int64 `json:"measuredAt"`
	// Unknown: the latest attempt failed. The figures are older.
	Unknown    bool  `json:"unknown,omitempty"`
	AlertSince int64 `json:"alertSince,omitempty"`
	Notified   bool  `json:"notified,omitempty"`
}

// Percent is how full the disk is, as df counts it: space reserved for
// root counts as neither used nor available.
func (f Folder) Percent() float64 {
	if f.UsedBytes+f.AvailableBytes <= 0 {
		return 0
	}
	return float64(f.UsedBytes) / float64(f.UsedBytes+f.AvailableBytes) * 100
}

type DiskState struct {
	// CheckedAt is the last attempt; 0 makes the next tick measure.
	CheckedAt int64    `json:"checkedAt"`
	Folders   []Folder `json:"folders"`
	// Revision is the binding the folders were measured under.
	Revision int64 `json:"revision,omitempty"`
}

func (d DiskState) alerting() bool {
	for _, folder := range d.Folders {
		if folder.AlertSince != 0 {
			return true
		}
	}
	return false
}

type MemoryState struct {
	AlertSince int64 `json:"alertSince,omitempty"`
	Notified   bool  `json:"notified,omitempty"`
	// Revision is the binding judged; only samples from its first checked
	// minute, From, count.
	Revision int64 `json:"revision,omitempty"`
	From     int64 `json:"from,omitempty"`
}

// over decides an alert from a percentage: it starts at threshold and, once
// started, lasts until the value falls Hysteresis points below it.
func over(alerting bool, percent float64, threshold int) bool {
	// Rounded so a value exactly at a line, such as 55 of 100, isn't 55.00000000000001.
	percent = math.Round(percent*1e6) / 1e6
	if alerting {
		return percent > float64(threshold-Hysteresis)
	}
	return percent >= float64(threshold)
}

// evaluateDisk applies a measurement. A folder that couldn't be measured
// keeps its figures and alert. It returns the folders whose alert ended.
func evaluateDisk(previous DiskState, spaces []DiskSpace, threshold int, now int64) (DiskState, []Folder) {
	next := DiskState{CheckedAt: now}
	var ended []Folder
	for _, space := range spaces {
		folder := Folder{Path: space.Path}
		for _, old := range previous.Folders {
			if old.Path == space.Path {
				folder = old
			}
		}
		if space.Failed {
			folder.Unknown = true
		} else {
			folder.TotalBytes, folder.AvailableBytes, folder.UsedBytes = space.Total, space.Available, space.Used
			folder.MeasuredAt, folder.Unknown = now, false
			alerting := over(folder.AlertSince != 0, folder.Percent(), threshold)
			if alerting && folder.AlertSince == 0 {
				folder.AlertSince, folder.Notified = now, false
			} else if !alerting && folder.AlertSince != 0 {
				ended = append(ended, folder)
				folder.AlertSince, folder.Notified = 0, false
			}
		}
		next.Folders = append(next.Folders, folder)
	}
	return next, ended
}

type memorySample struct{ at, kib int64 }

// evaluateMemory applies the samples of the last minutes, oldest first. An
// alert starts when every minute of the window has a sample at percent of
// the limit, and ends when the current minute's sample falls Hysteresis
// points below it. Without a current sample nothing changes.
func evaluateMemory(state MemoryState, samples []memorySample, minute, limitKiB int64, percent, minutes int) MemoryState {
	if len(samples) == 0 || samples[len(samples)-1].at != minute {
		return state
	}
	share := func(kib int64) float64 { return float64(kib) / float64(limitKiB) * 100 }
	if state.AlertSince != 0 {
		if !over(true, share(samples[len(samples)-1].kib), percent) {
			return MemoryState{}
		}
		return state
	}
	start := minute - int64(minutes-1)*resources.Interval
	count := 0
	for _, sample := range samples {
		if sample.at >= start && sample.at <= minute && over(false, share(sample.kib), percent) {
			count++
		}
	}
	if count < minutes {
		return state
	}
	return MemoryState{AlertSince: start}
}

func diskKey(serverID string) string   { return "monitor.disk:" + serverID }
func memoryKey(serverID string) string { return "monitor.memory:" + serverID }

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// loadState reads a saved state and returns its raw value, "" when none.
func loadState(ctx context.Context, q querier, key string, value any) (string, error) {
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value_json FROM settings WHERE key=?", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return raw, json.Unmarshal([]byte(raw), value)
}

// saveState replaces a state only if it is still previous and the policy
// it was judged by is still saved, so a policy saved meanwhile and its reset
// are kept. It returns the new raw value.
func (s *Service) saveState(ctx context.Context, watched monitored, key, previous string, value any) (string, bool, error) {
	encoded, _ := json.Marshal(value)
	const unchanged = "EXISTS (SELECT 1 FROM availability WHERE server_id=? AND policy_json=?)"
	var result sql.Result
	var err error
	if previous == "" {
		result, err = s.DB.ExecContext(ctx, "INSERT INTO settings(key,value_json) SELECT ?,? WHERE "+unchanged+" ON CONFLICT(key) DO NOTHING",
			key, string(encoded), watched.serverID, watched.raw)
	} else {
		result, err = s.DB.ExecContext(ctx, "UPDATE settings SET value_json=? WHERE key=? AND value_json=? AND "+unchanged,
			string(encoded), key, previous, watched.serverID, watched.raw)
	}
	if err != nil {
		return "", false, err
	}
	changed, err := result.RowsAffected()
	return string(encoded), changed == 1, err
}

// resetAlerts ends the alerts whose settings changed, without notifying,
// and makes the next tick measure disks again. It runs in Configure's
// transaction.
func resetAlerts(ctx context.Context, tx *sql.Tx, serverID string, previous, policy Policy) error {
	if !policy.Enabled || !previous.Enabled || previous.DiskAlert != policy.DiskAlert || previous.DiskPercent != policy.DiskPercent {
		var disk DiskState
		raw, err := loadState(ctx, tx, diskKey(serverID), &disk)
		if err != nil {
			return err
		}
		if raw != "" {
			disk.CheckedAt = 0
			for index := range disk.Folders {
				disk.Folders[index].AlertSince, disk.Folders[index].Notified = 0, false
			}
			encoded, _ := json.Marshal(disk)
			if _, err := tx.ExecContext(ctx, "UPDATE settings SET value_json=? WHERE key=?", string(encoded), diskKey(serverID)); err != nil {
				return err
			}
		}
	}
	if !policy.Enabled || previous.MemoryAlert != policy.MemoryAlert || previous.MemoryPercent != policy.MemoryPercent ||
		previous.MemoryMinutes != policy.MemoryMinutes {
		if _, err := tx.ExecContext(ctx, "DELETE FROM settings WHERE key=?", memoryKey(serverID)); err != nil {
			return err
		}
	}
	return nil
}

// Disk reads a server's last disk measurements.
func (s *Service) Disk(ctx context.Context, serverID string) (DiskState, error) {
	var disk DiskState
	_, err := loadState(ctx, s.DB, diskKey(serverID), &disk)
	return disk, err
}

// watched reads the servers, the policies of those monitored now, and the
// servers with active operations.
func (s *Service) watched(ctx context.Context) ([]identity.Server, map[string]monitored, map[string]bool, error) {
	logical, err := s.Store.List(ctx, "")
	if err != nil {
		return nil, nil, nil, err
	}
	rows, err := s.rows(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	active, err := s.active(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	policies := map[string]monitored{}
	for serverID, r := range rows {
		if policy, err := ParsePolicy([]byte(r.policy)); err == nil && policy.Enabled && !policy.Maintenance {
			policies[serverID] = monitored{Policy: policy, serverID: serverID, raw: r.policy}
		}
	}
	return logical, policies, active, nil
}

// monitored is a policy as a check read it.
type monitored struct {
	Policy
	serverID, raw string
}

// alertingPaths names the folders that alert, to tell when the set changes.
func (d DiskState) alertingPaths() string {
	var paths []string
	for _, folder := range d.Folders {
		if folder.AlertSince != 0 {
			paths = append(paths, folder.Path)
		}
	}
	return strings.Join(paths, "\x00")
}

func bound(server identity.Server, current map[string]servers.Container) bool {
	_, found := current[server.ContainerID]
	return server.Status == identity.StatusActive && !server.ReviewRequired && server.ContainerID != "" && found
}

// CheckDisk measures the data folders of monitored servers that are due,
// running or not. current is the tick's discovery result, or nil when
// Docker is unreachable; then nothing is measured.
func (s *Service) CheckDisk(ctx context.Context, current map[string]servers.Container, now int64) error {
	if current == nil || s.MeasureDisk == nil {
		return nil
	}
	logical, policies, active, err := s.watched(ctx)
	if err != nil {
		return err
	}
	for _, server := range logical {
		policy, ok := policies[server.ID]
		if !ok || !policy.DiskAlert || !bound(server, current) || active[server.ID] || (s.Locks != nil && s.Locks.ServerBusy(server.ID)) {
			continue
		}
		var previous DiskState
		raw, err := loadState(ctx, s.DB, diskKey(server.ID), &previous)
		if err != nil {
			return err
		}
		shown := previous.alertingPaths()
		// An accepted replacement may bring another disk: its folders start
		// without the old binding's alerts and are measured at once.
		if previous.Revision != server.BindingRevision {
			previous.CheckedAt = 0
			for index := range previous.Folders {
				previous.Folders[index].AlertSince, previous.Folders[index].Notified = 0, false
			}
		}
		interval := int64(DiskInterval)
		if previous.alerting() {
			interval = diskAlertingInterval
		}
		if previous.CheckedAt != 0 && now-previous.CheckedAt < interval {
			continue
		}
		spaces, err := s.MeasureDisk(ctx, server)
		if err != nil {
			// Skipped, such as for other work on the server: try next tick.
			continue
		}
		next, ended := evaluateDisk(previous, spaces, policy.DiskPercent, now)
		next.Revision = server.BindingRevision
		saved, changed, err := s.saveState(ctx, policy, diskKey(server.ID), raw, next)
		if err != nil || !changed {
			log.Warn("Could not save a server's disk space; it will be measured again")
			continue
		}
		for _, folder := range ended {
			if folder.Notified {
				s.notify("disk-recovery:"+server.ID+":"+folder.Path+":"+strconv.FormatInt(folder.AlertSince, 10),
					fmt.Sprintf("%s: the disk under %s has space again (%.0f%% full).", server.DisplayName, folder.Path, folder.Percent()))
			}
		}
		s.raiseDisk(ctx, server, policy, saved, next)
		if next.alertingPaths() != shown {
			s.changed(server.ID)
		}
	}
	return nil
}

// raiseDisk sends the alerts saved but not yet sent, then records them as
// sent. Each is keyed by its saved start, so a retry sends it once.
func (s *Service) raiseDisk(ctx context.Context, server identity.Server, policy monitored, raw string, state DiskState) {
	sent := false
	for index := range state.Folders {
		folder := &state.Folders[index]
		if folder.AlertSince == 0 || folder.Notified {
			continue
		}
		s.notify("disk:"+server.ID+":"+folder.Path+":"+strconv.FormatInt(folder.AlertSince, 10),
			fmt.Sprintf("%s: the disk under %s is %.0f%% full (%s free).", server.DisplayName, folder.Path, folder.Percent(),
				datasize.Format(folder.AvailableBytes)))
		folder.Notified, sent = true, true
	}
	if sent {
		if _, _, err := s.saveState(ctx, policy, diskKey(server.ID), raw, state); err != nil {
			log.Warn("Could not save a server's disk alert; it may be sent again")
		}
	}
}

// CheckMemory runs after each minute's resource samples. limits holds, for
// each server sampled this minute, its container's memory limit in KiB, or
// 0 when it has none. current is the same tick's discovery result, or nil
// when Docker is unreachable.
func (s *Service) CheckMemory(ctx context.Context, limits map[string]int64, current map[string]servers.Container, minute int64) error {
	if current == nil {
		return nil
	}
	logical, policies, active, err := s.watched(ctx)
	if err != nil {
		return err
	}
	for _, server := range logical {
		policy, ok := policies[server.ID]
		if !ok || !policy.MemoryAlert || active[server.ID] || (s.Locks != nil && s.Locks.ServerBusy(server.ID)) {
			continue
		}
		var previous MemoryState
		raw, err := loadState(ctx, s.DB, memoryKey(server.ID), &previous)
		if err != nil {
			return err
		}
		// An accepted replacement starts without the old binding's alert,
		// and the old container's samples don't count against its limit.
		judged := MemoryState{Revision: server.BindingRevision, From: previous.From}
		if raw != "" && previous.Revision != server.BindingRevision {
			judged.From = minute
		}
		next := previous
		if previous.Revision != server.BindingRevision {
			next = judged
		}
		limit, sampled := limits[server.ID]
		switch {
		case !bound(server, current) || current[server.ContainerID].State != "running":
			// A stopped game uses no memory.
			next = judged
		case !sampled:
			// Unknown this minute.
		case limit <= 0:
			next = judged
		default:
			samples, err := s.memorySamples(ctx, server.ID, max(minute-int64(policy.MemoryMinutes)*resources.Interval, judged.From-1), minute)
			if err != nil {
				return err
			}
			next = evaluateMemory(next, samples, minute, limit, policy.MemoryPercent, policy.MemoryMinutes)
			next.Revision, next.From = judged.Revision, judged.From
		}
		if next == previous && (next.AlertSince == 0 || next.Notified) {
			continue
		}
		saved, changed, err := s.saveState(ctx, policy, memoryKey(server.ID), raw, next)
		if err != nil || !changed {
			log.Warn("Could not save a server's memory alert; it will be checked again")
			continue
		}
		if previous.AlertSince != 0 && next.AlertSince == 0 {
			// Like a disk alert, one that ends with its binding sends no recovery.
			if previous.Notified && previous.Revision == next.Revision {
				s.notify("memory-recovery:"+server.ID+":"+strconv.FormatInt(previous.AlertSince, 10),
					server.DisplayName+"’s memory use is no longer near its limit.")
			}
			s.changed(server.ID)
		}
		if next.AlertSince != 0 && !next.Notified {
			s.notify("memory:"+server.ID+":"+strconv.FormatInt(next.AlertSince, 10),
				fmt.Sprintf("%s has used over %d%% of its memory limit for %d minutes.", server.DisplayName, policy.MemoryPercent, policy.MemoryMinutes))
			next.Notified = true
			if _, _, err := s.saveState(ctx, policy, memoryKey(server.ID), saved, next); err != nil {
				log.Warn("Could not save a server's memory alert; it may be sent again")
			}
			s.changed(server.ID)
		}
	}
	return nil
}

// memorySamples reads the samples after a and up to b, oldest first.
func (s *Service) memorySamples(ctx context.Context, serverID string, after, minute int64) ([]memorySample, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT at,memory FROM resource_samples WHERE server_id=? AND at>? AND at<=? ORDER BY at",
		serverID, after, minute)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []memorySample
	for rows.Next() {
		var sample memorySample
		if err := rows.Scan(&sample.at, &sample.kib); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

// Resource is an active disk or memory alert.
type Resource struct {
	Kind    string // disk, memory
	Path    string // disk only
	Percent float64
	Since   int64
}

// ResourceProblems reads the active disk and memory alerts of servers whose
// monitoring is on, by server.
func (s *Service) ResourceProblems(ctx context.Context) (map[string][]Resource, error) {
	rows, err := s.rows(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.DB.QueryContext(ctx, "SELECT key,value_json FROM settings WHERE key LIKE 'monitor.%'")
	if err != nil {
		return nil, err
	}
	defer result.Close()
	problems := map[string][]Resource{}
	for result.Next() {
		var key, raw string
		if err := result.Scan(&key, &raw); err != nil {
			return nil, err
		}
		kind, serverID, _ := strings.Cut(strings.TrimPrefix(key, "monitor."), ":")
		r := rows[serverID]
		if r == nil {
			continue
		}
		policy, err := ParsePolicy([]byte(r.policy))
		if err != nil || !policy.Enabled || policy.Maintenance {
			continue
		}
		switch {
		case kind == "disk" && policy.DiskAlert:
			var disk DiskState
			if json.Unmarshal([]byte(raw), &disk) != nil {
				continue
			}
			for _, folder := range disk.Folders {
				if folder.AlertSince != 0 {
					problems[serverID] = append(problems[serverID], Resource{Kind: "disk", Path: folder.Path, Percent: folder.Percent(), Since: folder.AlertSince})
				}
			}
		case kind == "memory" && policy.MemoryAlert:
			var memory MemoryState
			if json.Unmarshal([]byte(raw), &memory) == nil && memory.AlertSince != 0 {
				problems[serverID] = append(problems[serverID], Resource{Kind: "memory", Percent: float64(policy.MemoryPercent), Since: memory.AlertSince})
			}
		}
	}
	return problems, result.Err()
}
