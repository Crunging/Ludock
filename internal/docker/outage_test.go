package docker_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

// Docker restarts for a second. Under RideOut a read waits for it and says
// it waited; without RideOut, as pages and polling use the client, it fails
// at once, and a mutation sent meanwhile is known never to have reached
// Docker. A Docker away for longer than the limit still fails the read.
func TestReadsRideOutDockerRestart(t *testing.T) {
	e, client := engine(t)
	client.OutageLimit = 5 * time.Second
	id := e.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	go e.Outage(time.Second)
	time.Sleep(100 * time.Millisecond)
	if _, err := client.Inspect(ctx, id); !docker.Unavailable(err) {
		t.Fatal("read without RideOut:", err)
	}
	if err := client.Stop(ctx, id, nil); !docker.NotSent(err) || !docker.Unavailable(err) {
		t.Fatal("stop during the outage:", err)
	}
	var waited atomic.Bool
	riding := docker.RideOut(ctx, func() { waited.Store(true) })
	if info, err := client.Inspect(riding, id); err != nil || !info.State.Running || !waited.Load() {
		t.Fatal("read under RideOut:", err, waited.Load())
	}

	client.OutageLimit = 300 * time.Millisecond
	go e.Outage(3 * time.Second)
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	if _, err := client.Inspect(riding, id); !docker.Unavailable(err) || time.Since(started) > 2*time.Second {
		t.Fatal("read past the limit:", err, time.Since(started))
	}
}

// A request that reached Docker before its connection was cut is never
// reported as unsent.
func TestCutRequestMayHaveBeenSent(t *testing.T) {
	e, client := engine(t)
	id := e.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	e.Hang = func(request string) bool {
		if request == "POST /v1.47/containers/"+id+"/stop" {
			go e.Outage(100 * time.Millisecond)
			return true
		}
		return false
	}
	if err := client.Stop(ctx, id, nil); !docker.Unavailable(err) || docker.NotSent(err) {
		t.Fatal(err)
	}
}
