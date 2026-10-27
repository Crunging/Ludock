package resources

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/servers"
)

var ctx = context.Background()

var (
	admin  = &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	viewer = &db.SessionUser{ID: "viewer", Username: "viewer", Role: db.RoleViewer}
)

const day = 24 * 3_600_000

func setup(t *testing.T) (*Service, []identity.Server, *atomic.Int32) {
	t.Helper()
	d := dbtest.Open(t)
	store := &identity.Store{DB: d}
	list, err := store.Reconcile(ctx, []identity.Observation{
		{ContainerID: "mc", Name: "mc", DisplayName: "Minecraft", GameType: "minecraft"},
		{ContainerID: "vh", Name: "vh", DisplayName: "Valheim", GameType: "valheim"},
	}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []*db.SessionUser{admin, viewer} {
		if err := d.CreateUser(ctx, db.UserRecord{User: db.User{ID: user.ID, Username: user.Username, Role: user.Role, CreatedAt: 1},
			PasswordHash: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	samples := &atomic.Int32{}
	s := &Service{DB: d, Identity: store, Authz: &authz.Service{DB: d, Servers: store},
		Sample: func(_ context.Context, server identity.Server) (*servers.Stats, error) {
			samples.Add(1)
			if server.ContainerID == "vh" {
				return nil, errors.New("binding changed")
			}
			return &servers.Stats{CPUPercent: 12.345, MemUsageMB: 1536}, nil
		}}
	return s, list, samples
}

func byName(list []identity.Server, name string) identity.Server {
	for _, server := range list {
		if server.DisplayName == name {
			return server
		}
	}
	panic(name)
}

func insert(t *testing.T, s *Service, serverID string, at, cpu, memory int64) {
	t.Helper()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO resource_samples(server_id,at,cpu,memory) VALUES(?,?,?,?)", serverID, at, cpu, memory); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM resource_samples").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Ticks every 15 seconds record each running server once a minute, at the
// minute's start. A stopped server, one that fails its binding check, and a
// tick without Docker record nothing.
func TestRecordOncePerMinute(t *testing.T) {
	s, _, samples := setup(t)
	running := map[string]servers.Container{"mc": {ID: "mc", State: "running"}, "vh": {ID: "vh", State: "running"}}
	base := int64(1000 * Interval)
	for _, now := range []int64{base + 5_000, base + 20_000, base + 35_000, base + 50_000} {
		if err := s.Record(ctx, running, now); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s) != 1 || samples.Load() != 2 {
		t.Fatalf("samples = %d, reads = %d", count(t, s), samples.Load())
	}
	var at, cpu, memory int64
	s.DB.QueryRowContext(ctx, "SELECT at,cpu,memory FROM resource_samples").Scan(&at, &cpu, &memory)
	if at != base || cpu != 1235 || memory != 1536*1024 {
		t.Fatal(at, cpu, memory)
	}
	stopped := map[string]servers.Container{"mc": {ID: "mc", State: "exited"}, "vh": {ID: "vh", State: "exited"}}
	s.Record(ctx, stopped, base+Interval)
	s.Record(ctx, nil, base+2*Interval)
	if count(t, s) != 1 || samples.Load() != 2 {
		t.Fatalf("samples = %d, reads = %d", count(t, s), samples.Load())
	}
}

// Samples older than a day and one bucket are removed by the next minute's
// round, even while Docker is unreachable.
func TestRecordPrunesOldSamples(t *testing.T) {
	s, list, _ := setup(t)
	id := byName(list, "Minecraft").ID
	now := int64(10 * day)
	insert(t, s, id, now-day-5*Interval-Interval, 100, 1)
	insert(t, s, id, now-day-5*Interval, 100, 1)
	insert(t, s, id, now-Interval, 100, 1)
	if err := s.Record(ctx, nil, now); err != nil {
		t.Fatal(err)
	}
	if count(t, s) != 2 {
		t.Fatal(count(t, s))
	}
}

// The day averages into 5-minute buckets and keeps each bucket's peak;
// samples before the range are left out, and the range totals weigh every
// sample alike.
func TestHistoryBuckets(t *testing.T) {
	s, list, _ := setup(t)
	id := byName(list, "Minecraft").ID
	now := int64(10*day) + 7*Interval + 30_000
	end := int64(10*day) + 5*Interval
	insert(t, s, id, end-day-Interval, 99_00, 99)       // before the range
	insert(t, s, id, end-day, 10_00, 1024)              // first bucket
	insert(t, s, id, end-5*Interval, 20_00, 1024)       // a bucket of three
	insert(t, s, id, end-4*Interval, 40_00, 2048)       //
	insert(t, s, id, end-3*Interval, 30_00, 3072)       //
	insert(t, s, id, end+2*Interval, 50_00, 4096)       // the current bucket
	insert(t, s, byName(list, "Valheim").ID, end, 1, 1) // another server

	h, err := s.History(ctx, admin, id, RangeFor("day"), now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Start != end-day || h.End != end || len(h.Points) != 3 {
		t.Fatalf("%+v", h)
	}
	middle := h.Points[1]
	if h.Points[0].At != end-day || middle.At != end-5*Interval || h.Points[2].At != end ||
		middle.CPU != 30 || middle.CPUPeak != 40 || middle.Memory != 2048*1024 || middle.MemoryPeak != 3072*1024 {
		t.Fatalf("%+v", h.Points)
	}
	if h.CPU != 30 || h.CPUPeak != 50 || h.Memory != 11264*1024/5 || h.MemoryPeak != 4096*1024 {
		t.Fatalf("%+v", h)
	}
	if RangeFor("unknown").Key != "hour" {
		t.Fatal("default range")
	}
}

// A viewer without a grant can't read a server's history.
func TestHistoryNeedsView(t *testing.T) {
	s, list, _ := setup(t)
	if _, err := s.History(ctx, viewer, byName(list, "Minecraft").ID, RangeFor("hour"), day); err == nil {
		t.Fatal("history without a grant")
	}
}
