// Package resources keeps a day of CPU and memory history for each server:
// one sample a minute while it runs, removed as it ages out.
package resources

import (
	"context"
	"database/sql"
	"math"
	"sync"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
)

var log = logging.New("resources")

const (
	// Interval is the time between samples of a server, in milliseconds.
	Interval = 60_000
	// parallel bounds concurrent Docker stats reads, which take about a
	// second each.
	parallel = 4
)

// Range is a span of history and the bucket its samples are averaged into.
type Range struct {
	Key, Label string
	Span, Step int64 // milliseconds
}

// Ranges are the spans the server page offers; the first is the default.
var Ranges = []Range{
	{Key: "hour", Label: "Hour", Span: 3_600_000, Step: Interval},
	{Key: "6h", Label: "6 hours", Span: 6 * 3_600_000, Step: 2 * Interval},
	{Key: "day", Label: "Day", Span: 24 * 3_600_000, Step: 5 * Interval},
}

// RangeFor finds a range by key, or returns the default.
func RangeFor(key string) Range {
	for _, item := range Ranges {
		if item.Key == key {
			return item
		}
	}
	return Ranges[0]
}

// retained is how far back samples are kept: the longest range plus its
// first, partly covered bucket.
func retained() int64 {
	longest := Ranges[len(Ranges)-1]
	return longest.Span + longest.Step
}

// Point is one bucket of history. CPU is a percentage of one core; memory
// is in bytes.
type Point struct {
	At                 int64
	CPU, CPUPeak       float64
	Memory, MemoryPeak int64
}

// History is a server's samples over a range. Points are in time order and
// lie in [Start, End]; End is the start of the current bucket.
type History struct {
	ServerID   string
	Range      Range
	Start, End int64
	Points     []Point
	// Averages and peaks over the whole range.
	CPU, CPUPeak       float64
	Memory, MemoryPeak int64
}

// Service records and reads resource history.
type Service struct {
	DB       *db.DB
	Identity *identity.Store
	Authz    *authz.Service
	// Sample reads a server's statistics after confirming its binding.
	Sample func(context.Context, identity.Server) (*servers.Stats, error)

	mu   sync.Mutex
	last int64 // the minute last recorded
}

type sample struct {
	serverID    string
	cpu, memory int64
}

// Record samples every running server once per minute and removes samples
// older than the longest range. current is the discovery result of the same
// tick, or nil when Docker is unreachable; then nothing is sampled and the
// gap shows in the history.
func (s *Service) Record(ctx context.Context, current map[string]servers.Container, now int64) error {
	minute := now - now%Interval
	s.mu.Lock()
	due := minute != s.last
	s.last = minute
	s.mu.Unlock()
	if !due {
		return nil
	}
	var samples []sample
	if current != nil {
		logical, err := s.Identity.List(ctx, "")
		if err != nil {
			return err
		}
		samples = s.sampleAll(ctx, logical, current)
	}
	return s.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, item := range samples {
			if _, err := tx.ExecContext(ctx, "INSERT INTO resource_samples(server_id,at,cpu,memory) VALUES(?,?,?,?) ON CONFLICT DO NOTHING",
				item.serverID, minute, item.cpu, item.memory); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM resource_samples WHERE at < ?", minute-retained())
		return err
	})
}

// sampleAll reads the servers whose bound container is running. A server
// that cannot be sampled, or whose binding no longer matches, is skipped.
func (s *Service) sampleAll(ctx context.Context, logical []identity.Server, current map[string]servers.Container) []sample {
	var (
		mu      sync.Mutex
		wait    sync.WaitGroup
		samples []sample
	)
	slots := make(chan struct{}, parallel)
	for _, server := range logical {
		if server.Status != identity.StatusActive || server.ReviewRequired || server.ContainerID == "" ||
			current[server.ContainerID].State != "running" {
			continue
		}
		wait.Add(1)
		slots <- struct{}{}
		go func(server identity.Server) {
			defer wait.Done()
			defer func() { <-slots }()
			stats, err := s.Sample(ctx, server)
			if err != nil {
				log.Debug("Could not sample a server's resources; it will be sampled again next minute")
				return
			}
			mu.Lock()
			samples = append(samples, sample{serverID: server.ID, cpu: int64(math.Round(max(0, stats.CPUPercent) * 100)),
				memory: int64(math.Round(max(0, stats.MemUsageMB) * 1024))})
			mu.Unlock()
		}(server)
	}
	wait.Wait()
	return samples
}

// History reads a server's samples over a range, averaged into its buckets.
func (s *Service) History(ctx context.Context, actor *db.SessionUser, serverID string, span Range, now int64) (*History, error) {
	if _, err := s.Authz.Assert(ctx, actor, serverID, authz.ServerView); err != nil {
		return nil, err
	}
	end := now - now%span.Step
	h := &History{ServerID: serverID, Range: span, Start: end - span.Span, End: end}
	rows, err := s.DB.QueryContext(ctx, `SELECT at/?, COUNT(*), SUM(cpu), MAX(cpu), SUM(memory), MAX(memory)
    FROM resource_samples WHERE server_id=? AND at>=? AND at<? GROUP BY 1 ORDER BY 1`, span.Step, serverID, h.Start, end+span.Step)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var count, cpuSum, memorySum int64
	for rows.Next() {
		var slot, n, cpu, cpuPeak, memory, memoryPeak int64
		if err := rows.Scan(&slot, &n, &cpu, &cpuPeak, &memory, &memoryPeak); err != nil {
			return nil, err
		}
		h.Points = append(h.Points, Point{At: slot * span.Step, CPU: float64(cpu) / float64(n) / 100, CPUPeak: float64(cpuPeak) / 100,
			Memory: memory * 1024 / n, MemoryPeak: memoryPeak * 1024})
		count, cpuSum, memorySum = count+n, cpuSum+cpu, memorySum+memory
		h.CPUPeak, h.MemoryPeak = max(h.CPUPeak, float64(cpuPeak)/100), max(h.MemoryPeak, memoryPeak*1024)
	}
	if count > 0 {
		h.CPU, h.Memory = float64(cpuSum)/float64(count)/100, memorySum*1024/count
	}
	return h, rows.Err()
}
