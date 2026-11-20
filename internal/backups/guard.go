package backups

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// folderGuard gives up on calls into backup folders that stop answering, as
// a network share can. A call blocked in the kernel can't be interrupted, so
// each runs on a goroutine of its own, and the guard keeps one record per
// folder of the calls still holding it.
type folderGuard struct {
	mu sync.Mutex
	// busy counts, per folder and lane, the claims held and the calls given
	// up on that haven't returned.
	busy map[string]*[laneCount]int
}

// lane is which earlier calls make a call fail at once: those in its own
// lane. The lanes were separate mechanisms before the guard, and merging any
// would change which calls fail fast.
type lane int

const (
	laneWork    lane = iota // file work: copies, saves, removing copies
	laneMeasure             // checks of a second folder's free space
	laneLook                // looks into a folder, validation, removal
	laneArchive             // archive reads; no call consults it
	laneCount
)

type guardOptions struct {
	lane lane
	// claim holds the lane for the whole call rather than only once it is
	// given up on, so a second call fails at once even when nothing is stuck.
	claim bool
	// limit is how long the call may go without progress. check is how
	// often progress is looked at; zero makes limit a fixed time limit.
	limit, check time.Duration
	// stalled is returned when the call is given up on, and busy (stalled
	// when nil) when it is refused.
	stalled, busy error
	// hook names the testHook point the call's goroutine reaches first.
	hook string
}

// pending is one call into a folder; its flags are guarded by folderGuard.mu.
type pending struct {
	folder              string
	lane                lane
	claimed             bool
	returned, abandoned bool
}

// folderCall is what a guarded function sees of its call.
type folderCall struct {
	// ctx ends when the call does, and carries progress (progressKey).
	ctx  context.Context
	last atomic.Int64
	// gate is held by unlessGivenUp and by giving the call up.
	gate   sync.Mutex
	gaveUp bool
}

func (c *folderCall) progress() { c.last.Store(time.Now().UnixNano()) }

// unlessGivenUp runs fn unless the call was given up on, in which case it
// returns givenUp; giving up waits for a running fn, so none runs after the
// call has returned.
func (c *folderCall) unlessGivenUp(fn func() error, givenUp error) error {
	c.gate.Lock()
	defer c.gate.Unlock()
	if c.gaveUp {
		return givenUp
	}
	return fn()
}

func (g *folderGuard) counts(folder string) *[laneCount]int {
	if g.busy == nil {
		g.busy = map[string]*[laneCount]int{}
	}
	if g.busy[folder] == nil {
		g.busy[folder] = &[laneCount]int{}
	}
	return g.busy[folder]
}

// release is called with mu held.
func (g *folderGuard) release(p *pending) {
	counts := g.counts(p.folder)
	counts[p.lane]--
	if *counts == ([laneCount]int{}) {
		delete(g.busy, p.folder)
	}
}

// finish records that p's call returned; whichever of finish and abandon
// comes second settles the record.
func (g *folderGuard) finish(p *pending) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p.returned = true
	if p.claimed || p.abandoned {
		g.release(p)
	}
}

// abandon records p's call as given up on; false when it already returned.
func (g *folderGuard) abandon(p *pending) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.returned {
		return false
	}
	p.abandoned = true
	if !p.claimed {
		g.counts(p.folder)[p.lane]++
	}
	return true
}

// run calls fn, I/O in folder, on a goroutine of its own. It refuses while
// the folder's lane is busy, and gives fn up when ctx ends (returning ctx's
// error) or at the time limit (o.stalled), cancelling fn's context. Until a
// call given up on returns, the record keeps it.
func (g *folderGuard) run(ctx context.Context, folder string, o guardOptions, fn func(*folderCall) error) error {
	p := &pending{folder: folder, lane: o.lane, claimed: o.claim}
	g.mu.Lock()
	if counts := g.busy[folder]; counts != nil && counts[o.lane] > 0 {
		g.mu.Unlock()
		if o.busy != nil {
			return o.busy
		}
		return o.stalled
	}
	if o.claim {
		g.counts(folder)[o.lane]++
	}
	g.mu.Unlock()
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	c := &folderCall{}
	c.progress()
	c.ctx = context.WithValue(work, progressKey{}, c.progress)
	done := make(chan error, 1)
	go func() {
		if o.hook != "" && testHook != nil {
			testHook(o.hook)
		}
		err := fn(c)
		// Settled before the result is reported, so the caller's next call
		// in this folder doesn't find it still held.
		g.finish(p)
		done <- err
	}()
	var expired <-chan time.Time
	if o.check == 0 {
		timer := time.NewTimer(o.limit)
		defer timer.Stop()
		expired = timer.C
	} else {
		ticker := time.NewTicker(o.check)
		defer ticker.Stop()
		expired = ticker.C
	}
	for {
		givenUp := o.stalled
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			givenUp = ctx.Err()
		case <-expired:
			if o.check != 0 && time.Since(time.Unix(0, c.last.Load())) <= o.limit {
				continue
			}
		}
		// A look that returned meanwhile reports its result; a claimed call
		// reports the limit or cancellation it reached.
		if !g.abandon(p) && !o.claim {
			return <-done
		}
		c.gate.Lock()
		c.gaveUp = true
		c.gate.Unlock()
		return givenUp
	}
}
