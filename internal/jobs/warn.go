package jobs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/locks"
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

var verbs = map[string]string{ops.KindRestart: "restart", ops.KindStop: "shut down", ops.KindUpdate: "restart for an update",
	ops.KindBackup: "restart for a backup", ops.KindRestore: "restart to restore a backup", ops.KindUndoRestore: "restart to undo a restore"}

type runFunc = func(ctx context.Context, job *ops.Context) (*ops.Result, error)

// warnedFirst runs an operation after warnFirst.
func (j *Jobs) warnedFirst(capability authz.Capability, run runFunc) runFunc {
	return func(ctx context.Context, job *ops.Context) (*ops.Result, error) {
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
	// Resumed while it was still trying to send the warning, which a requeue
	// doesn't keep as the phase: nobody heard one yet, so it is sent now.
	r := job.Job.Recovery
	sending := until != 0 && !r.WarnSent && !r.WarnFailed && CanWarn(server.Container)
	// A backup that can't be taken, or an earlier restore that the stop
	// can't finish, is refused before anyone is warned, and checked again
	// before the stop. Work that holds its locks while it warns checks its
	// backup before calling this.
	if until == 0 && (backsUpFirst(job) || slices.Contains([]string{ops.KindRestore, ops.KindUndoRestore, ops.KindUpdate}, job.Job.Kind)) {
		check := func() (*ops.Result, error) {
			if err := assertAccess(); err != nil {
				return nil, err
			}
			if !locked && backsUpFirst(job) {
				if err := j.assertBackupPossible(ctx, server, job); err != nil {
					return nil, err
				}
			}
			return nil, j.assertPendingUndoable(ctx, server, job, assertAccess)
		}
		if locked {
			_, err = check()
		} else {
			_, err = job.WithLocks(append(slices.Clone(server.LockKeys), locks.StorageKey), check)
		}
		if err != nil {
			return err
		}
	}
	if until == 0 || sending {
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
		}
		// An earlier attempt, requeued behind other work, may have found nobody.
		if err := job.Progress(ops.PhaseWarningPlayers, func(r *ops.Recovery) { r.WarnUntil, r.WarnSkipped = until, false }, nil); err != nil {
			return err
		}
		// Best effort: a warning that can't be sent doesn't stop the action.
		// One that was gives players the full time from when they heard it.
		// A warning cancelled before it goes out is never sent.
		sent, err := j.sendWarning(ctx, job, server, assertAccess, locked)
		switch {
		case err != nil:
			return err
		case sent:
			until = max(until, time.Now().Add(j.warningLead()).UnixMilli())
			err := job.Progress(ops.PhaseWarning, func(r *ops.Recovery) { r.WarnSent, r.WarnUntil = true, until }, nil)
			// Cancelled while the warning went out: the canceller didn't know
			// it was sent, so the players hear it's off from here.
			if errors.Is(err, ops.ErrCancelled) {
				j.sayCalledOff(ctx, job.Job, job.Job.BindingRevision, locked)
			}
			if err != nil {
				return err
			}
		case CanWarn(server.Container):
			// Nobody heard a warning, so there is no minute to give: the
			// work goes ahead, and Activity says players weren't warned.
			if err := assertAccess(); err != nil {
				return err
			}
			result := job.Job.Result
			result.WarnFailedNote = warnFailedNote(job.Job.Kind)
			return job.Progress(phaseWarned, func(r *ops.Recovery) { r.WarnFailed = true }, &result)
		default:
			// A game that can't message its players waits out the minute.
			if err := job.Progress(ops.PhaseWarning, nil, nil); err != nil {
				return err
			}
		}
	} else if err := warnedTooLate(job); err != nil {
		return err
	} else if r.WarnFailed {
		return job.Progress(phaseWarned, nil, nil)
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

// warnFailedNote is what Activity says when a warning never went out.
func warnFailedNote(kind string) string {
	if kind == ops.KindStop {
		return "Couldn’t warn players before the shutdown."
	}
	return "Couldn’t warn players before the restart."
}

// sendWarning sends the warning, trying again for the warning's own time
// while the failure may pass, such as Docker or the game's console being
// briefly unreachable. It reports whether players were told; an error means
// the job was cancelled or its owner lost access, and no warning went out.
func (j *Jobs) sendWarning(ctx context.Context, job *ops.Context, server *servers.Context, assertAccess func() error, locked bool) (bool, error) {
	message := schedules.WarningMessage(job.Job.Kind, WarningMinutes)
	deadline := time.Now().Add(j.warningLead())
	for {
		err := j.broadcast(ctx, server, message, func() error { return job.Active(ctx) }, assertAccess, locked)
		if err == nil {
			return true, nil
		}
		if err := job.Active(ctx); err != nil {
			return false, err
		}
		if err := assertAccess(); err != nil {
			return false, err
		}
		// A refusal, such as a game without a way to message players, won't
		// pass by waiting; a password file the game hasn't written yet may.
		if public, ok := apperr.As(err); (ok && public.Code != console.PasswordUnreadable) || !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-time.After(min(j.poll(), time.Until(deadline))):
		case <-job.Cancelled():
			return false, ops.ErrCancelled
		case <-ctx.Done():
			return false, ctx.Err()
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
func warnedResult(job *ops.Context, result *ops.Result, err error) (*ops.Result, error) {
	if err != nil {
		return result, calledOff(job, err)
	}
	if job.Job.Recovery.WarnSkipped || job.Job.Recovery.WarnFailed {
		if result == nil {
			result = &ops.Result{}
		}
		if job.Job.Recovery.WarnFailed {
			result.WarnFailedNote = warnFailedNote(job.Job.Kind)
		} else {
			result.WarnSkipped = true
		}
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
	if !job.Job.Recovery.WarnSent {
		return nil
	}
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
	if job.Job.Kind == ops.KindUpdate {
		return r.InitialRunning == nil && !r.StopStarted && len(r.Swap) == 0
	}
	return job.Job.Phase == ops.PhaseWarning || job.Job.Phase == ops.PhaseWarningPlayers || job.Job.Phase == phaseWarned
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
