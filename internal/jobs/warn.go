package jobs

import (
	"context"
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

// WarningMinutes is how long a restart, stop, or update asked for with a
// warning waits after telling players.
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

var verbs = map[string]string{"restart": "restart", "stop": "shut down", "update": "restart for an update"}

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
		until = time.Now().Add(j.warningLead()).UnixMilli()
		if err := job.Progress("warning", func(r *ops.Recovery) { r.WarnUntil = until }, nil); err != nil {
			return err
		}
		// Best effort: a warning that can't be sent doesn't stop the action.
		// One that was gives players the full time from when they heard it.
		if j.broadcast(ctx, server, job.Job.Kind, assertAccess, locked) == nil {
			until = max(until, time.Now().Add(j.warningLead()).UnixMilli())
			if err := job.Progress("warning", func(r *ops.Recovery) { r.WarnSent, r.WarnUntil = true, until }, nil); err != nil {
				return err
			}
		} else if err := assertAccess(); err != nil {
			return err
		}
	} else if err := warnedTooLate(job); err != nil {
		return err
	} else if err := job.Progress("warning", nil, nil); err != nil {
		return err
	}
	for {
		remaining := time.Until(time.UnixMilli(until))
		if remaining <= 0 {
			return assertAccess()
		}
		select {
		case <-time.After(min(j.poll(), remaining)):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := assertAccess(); err != nil {
			return err
		}
	}
}

func (j *Jobs) broadcast(ctx context.Context, server *servers.Context, kind string, assertAccess func() error, locked bool) error {
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
	ctx, cancel := context.WithTimeout(ctx, j.warningLead()/2)
	defer cancel()
	adapter := games.ResolveAdapter(server.Container.GameType, server.Container.Labels)
	template := games.BroadcastTemplate(server.Container.GameType, server.Container.Labels)
	command := strings.ReplaceAll(template, "{message}", schedules.WarningMessage(kind, WarningMinutes))
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
	return job.Job.Phase == "warning"
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
