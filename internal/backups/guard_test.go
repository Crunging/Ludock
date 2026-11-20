package backups

import (
	"context"
	"errors"
	"testing"
	"time"
)

// One folder is busy in each lane in turn. A claim refuses a second claim
// even before anything is stuck; a look refuses later looks only once it is
// given up on; no lane refuses calls in another; and each record settles
// once its call returns.
func TestFolderGuardLanes(t *testing.T) {
	var g folderGuard
	stalled, busy := errors.New("stalled"), errors.New("busy")
	quick := func(l lane, claim bool) error {
		return g.run(context.Background(), "/f", guardOptions{lane: l, claim: claim, limit: time.Second, stalled: stalled, busy: busy},
			func(*folderCall) error { return nil })
	}
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- g.run(context.Background(), "/f", guardOptions{lane: laneWork, claim: true, limit: time.Hour, stalled: stalled},
			func(*folderCall) error { <-release; return nil })
	}()
	for deadline := time.Now().Add(5 * time.Second); g.busyIn("/f", laneWork) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the claim never started")
		}
	}
	if err := quick(laneWork, true); err != busy {
		t.Fatal("a second claim started", err)
	}
	if quick(laneLook, false) != nil || quick(laneMeasure, true) != nil {
		t.Fatal("a claim refused another lane")
	}
	close(release)
	if <-held != nil || g.busyIn("/f", laneWork) != 0 {
		t.Fatal("the claim didn't settle")
	}

	stuck := make(chan struct{})
	err := g.run(context.Background(), "/f", guardOptions{lane: laneLook, limit: 20 * time.Millisecond, check: 5 * time.Millisecond, stalled: stalled},
		func(*folderCall) error { <-stuck; return nil })
	if err != stalled || g.busyIn("/f", laneLook) != 1 {
		t.Fatal("a stuck look wasn't given up on", err)
	}
	if quick(laneLook, false) != busy || quick(laneWork, true) != nil {
		t.Fatal("an abandoned look refused the wrong calls")
	}
	close(stuck)
	for deadline := time.Now().Add(5 * time.Second); g.busyIn("/f", laneLook) != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned look never settled")
		}
	}
	if quick(laneLook, false) != nil || len(g.busy) != 0 {
		t.Fatal("the record outlived its calls", g.busy)
	}
}
