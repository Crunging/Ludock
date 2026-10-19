package ops

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
)

var ctx = context.Background()

func setup(t *testing.T, servers int) (*Runner, []string) {
	t.Helper()
	d := dbtest.Open(t)
	d.CreateUser(ctx, db.UserRecord{User: db.User{ID: "admin", Username: "admin", Role: db.RoleAdmin, CreatedAt: 1}, PasswordHash: "x"})
	store := &identity.Store{DB: d}
	var observations []identity.Observation
	for index := range servers {
		name := string(rune('a' + index))
		observations = append(observations, identity.Observation{ContainerID: "c" + name, Name: name, DisplayName: name, GameType: "minecraft"})
	}
	list, _ := store.Reconcile(ctx, observations, "", 0)
	var ids []string
	for _, server := range list {
		ids = append(ids, server.ID)
	}
	r := &Runner{DB: d, Locks: locks.New()}
	t.Cleanup(r.Stop)
	return r, ids
}

func waitStatus(t *testing.T, r *Runner, id, status string) *Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if job, _ := r.Get(ctx, id); job != nil && job.Status == status {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, _ := r.Get(ctx, id)
	t.Fatalf("job %s: %+v, want %s", id, job, status)
	return nil
}

func TestEnqueueIdempotencyAndConflicts(t *testing.T) {
	r, servers := setup(t, 1)
	first, err := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start", IdempotencyKey: "k"})
	if err != nil || first.Status != Queued {
		t.Fatal(first, err)
	}
	again, err := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start", IdempotencyKey: "k"})
	if err != nil || again.ID != first.ID {
		t.Fatal("idempotent request created another operation")
	}
	if _, err := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start", IdempotencyKey: "k",
		Input: map[string]any{"x": 1}}); !apperr.Is(err, "IDEMPOTENCY_CONFLICT") {
		t.Fatal(err)
	}
	if _, err := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "stop"}); !apperr.Is(err, "OPERATION_CONFLICT") {
		t.Fatal(err)
	}
}

func TestRunsHandlersAndRecordsOutcomes(t *testing.T) {
	r, servers := setup(t, 3)
	r.Register("start", Handler{Run: func(_ context.Context, c *Context) (map[string]any, error) {
		c.Progress("starting", map[string]any{"containerId": "x"}, nil)
		return map[string]any{"action": "start"}, nil
	}})
	r.Register("update", Handler{Run: func(context.Context, *Context) (map[string]any, error) {
		return map[string]any{"alreadyCurrent": true}, nil
	}})
	r.Register("stop", Handler{Run: func(context.Context, *Context) (map[string]any, error) {
		return nil, errors.New("internal detail /srv/secret")
	}})
	r.Start(ctx)
	a, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start"})
	b, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[1], ActorID: "admin", Kind: "update"})
	c, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[2], ActorID: "admin", Kind: "stop"})
	job := waitStatus(t, r, a.ID, Succeeded)
	if job.Recovery["containerId"] != "x" || job.Result["action"] != "start" {
		t.Fatal(job)
	}
	waitStatus(t, r, b.ID, AlreadyCurrent)
	failed := waitStatus(t, r, c.ID, Failed)
	if failed.Error != "The operation failed. Check administrator diagnostics." {
		t.Fatal("internal error text leaked:", failed.Error)
	}
	var audits int
	r.DB.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action IN ('server.start.succeeded','server.stop.failed') AND user_id='admin'").Scan(&audits)
	if audits != 2 {
		t.Fatal(audits)
	}
}

func TestDataLaneIsSerialAndLifecycleParallel(t *testing.T) {
	r, servers := setup(t, 4)
	var mu sync.Mutex
	active := map[string]int{}
	peak := map[string]int{}
	gate := make(chan struct{})
	track := func(lane string) func(context.Context, *Context) (map[string]any, error) {
		return func(context.Context, *Context) (map[string]any, error) {
			mu.Lock()
			active[lane]++
			peak[lane] = max(peak[lane], active[lane])
			mu.Unlock()
			<-gate
			mu.Lock()
			active[lane]--
			mu.Unlock()
			return nil, nil
		}
	}
	r.Register("backup", Handler{Run: track("data")})
	r.Register("start", Handler{Run: track("lifecycle")})
	r.Start(ctx)
	var ids []string
	for index, kind := range []string{"backup", "backup", "start", "start"} {
		job, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[index], ActorID: "admin", Kind: kind})
		ids = append(ids, job.ID)
	}
	// Wait for both lifecycle jobs to run at once (a fixed sleep flakes on
	// slow runners), then give a second data job time to start wrongly.
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		lifecycle := active["lifecycle"]
		mu.Unlock()
		if lifecycle == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	for _, id := range ids {
		waitStatus(t, r, id, Succeeded)
	}
	mu.Lock()
	defer mu.Unlock()
	if peak["data"] != 1 || peak["lifecycle"] != 2 {
		t.Fatal(peak)
	}
}

func TestJobsYieldHeldResourcesAndRetry(t *testing.T) {
	r, servers := setup(t, 2)
	gate := make(chan struct{})
	order := make(chan string, 2)
	r.Register("start", Handler{Run: func(_ context.Context, c *Context) (map[string]any, error) {
		return c.WithLocks([]string{"path:/srv/shared"}, func() (map[string]any, error) {
			order <- c.Job.ServerID
			if c.Job.ServerID == servers[0] {
				<-gate
			}
			return nil, nil
		})
	}})
	r.Start(ctx)
	first, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start"})
	if got := <-order; got != servers[0] {
		t.Fatal(got)
	}
	second, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[1], ActorID: "admin", Kind: "start"})
	waitStatus(t, r, second.ID, Queued)
	close(gate)
	waitStatus(t, r, first.ID, Succeeded)
	waitStatus(t, r, second.ID, Succeeded)
	// A conflict with a request outside the runner fails instead of waiting.
	release, _ := r.Locks.Acquire([]string{"path:/srv/shared"}, nil)
	defer release()
	third, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start"})
	if job := waitStatus(t, r, third.ID, Failed); job.Error != "A conflicting operation is already running" {
		t.Fatal(job.Error)
	}
}

func TestBackgroundHoldersAreCancelled(t *testing.T) {
	r, servers := setup(t, 1)
	cancelled := make(chan struct{})
	var release func()
	release, _ = r.Locks.Acquire([]string{"server:" + servers[0]}, func() { close(cancelled); go release() })
	r.Register("stop", Handler{Run: func(_ context.Context, c *Context) (map[string]any, error) {
		return c.WithLocks([]string{"server:" + servers[0]}, func() (map[string]any, error) { return nil, nil })
	}})
	r.Start(ctx)
	job, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "stop"})
	<-cancelled
	waitStatus(t, r, job.ID, Succeeded)
}

func TestInterruptedOperationsAreReconciledNotReplayed(t *testing.T) {
	r, servers := setup(t, 2)
	ran := false
	r.Register("stop", Handler{
		Run: func(context.Context, *Context) (map[string]any, error) { ran = true; return nil, nil },
		Recover: func(_ context.Context, c *Context) (map[string]any, error) {
			return map[string]any{"reconciled": c.Job.Recovery["stopStarted"]}, nil
		},
	})
	r.DB.Exec(`INSERT INTO operations (id,server_id,actor_id,kind,status,phase,input_json,recovery_json,binding_revision,created_at,updated_at)
    VALUES ('00000000-0000-4000-8000-000000000001',?,'admin','stop','running','stopping','{}','{"stopStarted":true}',1,1,1),
           ('00000000-0000-4000-8000-000000000002',?,'admin','stop','running','stopping','not json','{}',1,1,1)`, servers[0], servers[1])
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	job, _ := r.Get(ctx, "00000000-0000-4000-8000-000000000001")
	if job.Status != Interrupted || job.Result["reconciled"] != true || ran {
		t.Fatalf("%+v ran=%v", job, ran)
	}
	var damaged string
	r.DB.QueryRow("SELECT error FROM operations WHERE id='00000000-0000-4000-8000-000000000002'").Scan(&damaged)
	if damaged != "Recovery requires administrator attention. The server has been left stopped where possible." {
		t.Fatal(damaged)
	}
}

func TestStopLeavesQueuedWork(t *testing.T) {
	r, servers := setup(t, 1)
	r.Register("start", Handler{Run: func(context.Context, *Context) (map[string]any, error) { return nil, nil }})
	job, _ := r.Enqueue(ctx, nil, EnqueueOptions{ServerID: servers[0], ActorID: "admin", Kind: "start"})
	r.Start(ctx)
	r.Stop()
	r.Start(ctx)
	waitStatus(t, r, job.ID, Succeeded)
}
