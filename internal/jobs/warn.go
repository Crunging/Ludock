package jobs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedtime"
	"github.com/Crunging/Ludock/internal/schedules"
	"github.com/Crunging/Ludock/internal/servers"
)

// WarningMinutes is how long an operation asked for with a warning waits
// after telling players.
const WarningMinutes = 1

// WarnInput is the operation input that asks for a warning first.
const WarnInput = "warnPlayers"

func warnRequested(job *ops.Context) bool { return job.Job.Input[WarnInput] == true }

// CanWarn reports whether a server's game can message every player.
func CanWarn(container servers.Container) bool {
	return container.Console != nil && container.Console.SupportsBroadcast &&
		games.BroadcastTemplate(container.GameType, container.Labels) != ""
}

func (j *Jobs) warningLead() time.Duration {
	if j.WarningLead > 0 {
		return j.WarningLead
	}
	return WarningMinutes * time.Minute
}

var verbs = map[string]string{"restart": "restart", "stop": "shut down", "update": "restart for an update",
	"backup": "restart for a backup", "restore": "restart to restore a backup", UndoRestore: "restart to undo a restore"}

type runFunc = func(ctx context.Context, job *ops.Context) (map[string]any, error)

// warnedFirst runs an operation after warnFirst.
func (j *Jobs) warnedFirst(capability authz.Capability, run runFunc) runFunc {
	return func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		if err := j.warnFirst(ctx, job, capability, false); err != nil {
			return nil, calledOff(job, err)
		}
		// Past the wait, an interruption is recovered like any other, which
		// removes helpers the checks before the stop may leave.
		if warnRequested(job) {
			if err := job.Progress("validating", nil, nil); err != nil {
				return nil, calledOff(job, err)
			}
		}
		result, err := run(ctx, job)
		return warnedResult(job, result, err)
	}
}

// warnFirst, for an operation that asked for it, tells players the server
// is going down and waits until the time it gave. The time is saved before
// the warning is sent, so a restart of Ludock resumes the wait without
// warning again. Locks are taken only while the warning is sent, unless
// locked says the caller holds them.
func (j *Jobs) warnFirst(ctx context.Context, job *ops.Context, capability authz.Capability, locked bool) error {
	if !warnRequested(job) {
		return nil
	}
	server, err := j.resolve(ctx, job, capability, true)
	if err != nil {
		return err
	}
	assertAccess := func() error {
		if _, err := j.resolve(ctx, job, capability, true); err != nil {
			return err
		}
		if job.Job.Input["createBackup"] == true {
			return j.assertCapability(ctx, job, authz.BackupsCreate)
		}
		return nil
	}
	until := job.Job.Recovery.WarnUntil
	if until == 0 {
		if err := job.Progress(phaseCounting, nil, nil); err != nil {
			return err
		}
		if j.nobodyOnline(ctx, server, locked) {
			if err := assertAccess(); err != nil {
				return err
			}
			return job.Progress(phaseCounting, func(r *ops.Recovery) { r.WarnSkipped = true }, nil)
		}
		until = time.Now().Add(j.warningLead()).UnixMilli()
		// An earlier attempt, requeued behind other work, may have found nobody.
		if err := job.Progress("warning", func(r *ops.Recovery) { r.WarnUntil, r.WarnSkipped = until, false }, nil); err != nil {
			return err
		}
		// Best effort: a warning that can't be sent doesn't stop the action.
		// One that was gives players the full time from when they heard it.
		// A warning cancelled before it goes out is never sent.
		if j.broadcast(ctx, server, schedules.WarningMessage(job.Job.Kind, WarningMinutes), func() error { return job.Active(ctx) },
			assertAccess, locked) == nil {
			until = max(until, time.Now().Add(j.warningLead()).UnixMilli())
			err := job.Progress(ops.PhaseWarning, func(r *ops.Recovery) { r.WarnSent, r.WarnUntil = true, until }, nil)
			// Cancelled while the warning went out: the canceller didn't know
			// it was sent, so the players hear it's off from here.
			if errors.Is(err, ops.ErrCancelled) {
				j.broadcast(ctx, server, schedules.CalledOffMessage(job.Job.Kind), nil, func() error {
					_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, job.Job.BindingRevision)
					return err
				}, locked)
			}
			if err != nil {
				return err
			}
		} else if err := job.Active(ctx); err != nil {
			return err
		} else if err := assertAccess(); err != nil {
			return err
		}
	} else if err := warnedTooLate(job); err != nil {
		return err
	} else if err := job.Progress(ops.PhaseWarning, nil, nil); err != nil {
		return err
	}
	for {
		remaining := time.Until(time.UnixMilli(until))
		if remaining <= 0 {
			if err := assertAccess(); err != nil {
				return err
			}
			// The end of the wait: from here the operation can't be cancelled.
			return job.Progress(phaseWarned, nil, nil)
		}
		select {
		case <-time.After(min(j.poll(), remaining)):
		case <-job.Cancelled():
			return ops.ErrCancelled
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := assertAccess(); err != nil {
			return err
		}
	}
}

// phaseWarned is a warned operation past its wait that hasn't acted yet.
const phaseWarned = "warned"

// phaseCounting is an operation reading who is online before it warns.
const phaseCounting = "checking_players"

// recentReading is how old the panel's own reading of players online may be
// to warn them without reading again.
const recentReading = 15 * time.Second

// nobodyOnline reports whether a reading taken now finds nobody on server,
// since someone may have joined since the panel's last. A count that can't
// be read isn't nobody: the warning goes out.
func (j *Jobs) nobodyOnline(ctx context.Context, server *servers.Context, locked bool) bool {
	switch server.Container.State {
	case "running", "paused", "restarting":
	default:
		return true
	}
	if j.Players == nil || !j.Players.Source.Lists(server.Container) {
		return false
	}
	if online, at, known := j.Players.OnlineAt(server.Logical.ID, server.Container); known && online > 0 && time.Since(time.UnixMilli(at)) < recentReading {
		return false
	}
	if !locked {
		// Preempts a background reading, which this one replaces.
		release, err := j.Locks.AcquirePreempting(ctx, server.LockKeys, nil)
		if err != nil {
			return false
		}
		defer release()
	}
	reading := j.Players.ReadHeld(ctx, *server.Logical)
	return reading != nil && !reading.Failed && reading.Online == 0
}

// warnedResult ends an operation that may have warned first: a success notes
// that nobody was online to warn, and a failure is calledOff.
func warnedResult(job *ops.Context, result map[string]any, err error) (map[string]any, error) {
	if err != nil {
		return result, calledOff(job, err)
	}
	if job.Job.Recovery.WarnSkipped {
		if result == nil {
			result = map[string]any{}
		}
		result["warnSkipped"] = true
	}
	return result, nil
}

// broadcast sends message to players; ready, when set, runs once the
// server's locks are held, right before sending.
func (j *Jobs) broadcast(ctx context.Context, server *servers.Context, message string, ready, assertAccess func() error, locked bool) error {
	if !CanWarn(server.Container) || j.Console == nil {
		return errNoWarning
	}
	if !locked {
		release, err := j.Locks.AcquirePreempting(ctx, server.LockKeys, nil)
		if err != nil {
			return err
		}
		defer release()
	}
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, j.warningLead()/2)
	defer cancel()
	adapter := games.ResolveAdapter(server.Container.GameType, server.Container.Labels)
	template := games.BroadcastTemplate(server.Container.GameType, server.Container.Labels)
	command := strings.ReplaceAll(template, "{message}", message)
	return j.Console.GameCommand(ctx, console.Target{ContainerID: server.Container.ID, Labels: server.Container.Labels,
		State: server.Container.State}, adapter, command, console.Discard, assertAccess)
}

// warnedTooLate refuses to act more than a few minutes after the time
// players were given, as after Ludock was down.
func warnedTooLate(job *ops.Context) error {
	if until := job.Job.Recovery.WarnUntil; until != 0 && time.Now().UnixMilli() > until+schedtime.OneTimeGraceMs {
		return apperr.New("WARNED_ACTION_MISSED", 409, "Ludock wasn’t running when the server was due to "+verbs[job.Job.Kind]+
			", so it didn’t. Players had been told it would.")
	}
	return nil
}

var errNoWarning = apperr.New("WARNING_UNAVAILABLE", 409, "This server's game can't message its players")

// resumable: a warned operation interrupted while it waited, before any
// later step was saved, so it can wait out its warning and act after a
// restart of Ludock. An update's pull and checks change nothing, so it
// stays resumable until it saves its stop.
func resumable(job *ops.Context) bool {
	r := job.Job.Recovery
	if !warnRequested(job) || r.WarnUntil == 0 {
		return false
	}
	if job.Job.Kind == "update" {
		return r.InitialRunning == nil && !r.StopStarted && len(r.Swap) == 0
	}
	return job.Job.Phase == ops.PhaseWarning || job.Job.Phase == phaseWarned
}

// calledOff tells whoever reads the failure that players had been warned.
// Other errors keep their chain, which the runner may act on.
func calledOff(job *ops.Context, err error) error {
	r := job.Job.Recovery
	public, ok := apperr.As(err)
	if !ok || !r.WarnSent || public.Code == "WARNED_ACTION_MISSED" {
		return err
	}
	suffix := " Players were told the server would " + verbs[job.Job.Kind] + ", but it didn’t."
	if r.StopStarted || r.InitialRunning != nil || len(r.Swap) != 0 {
		suffix = " Players had been told the server would " + verbs[job.Job.Kind] + "."
	}
	return apperr.New(public.Code, public.Status, strings.TrimSuffix(public.Message, ".")+"."+suffix)
}
