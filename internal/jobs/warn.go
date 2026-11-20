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
		_, err := j.resolve(ctx, job, capability, true)
		return err
	}
	until := job.Job.Recovery.WarnUntil
	if until == 0 {
		until = time.Now().Add(j.warningLead()).UnixMilli()
		if err := job.Progress("warning", func(r *ops.Recovery) { r.WarnUntil = until }, nil); err != nil {
			return err
		}
		// Best effort: a warning that can't be sent doesn't stop the action.
		if j.broadcast(ctx, server, job.Job.Kind, assertAccess, locked) == nil {
			if err := job.Progress("warning", func(r *ops.Recovery) { r.WarnSent = true }, nil); err != nil {
				return err
			}
		} else if err := assertAccess(); err != nil {
			return err
		}
	} else if time.Now().UnixMilli() > until+schedtime.OneTimeGraceMs {
		return apperr.New("WARNED_ACTION_MISSED", 409, "Ludock wasn’t running when the server was due to "+verbs[job.Job.Kind]+
			", so it didn’t. Players had been told it would.")
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

var errNoWarning = apperr.New("WARNING_UNAVAILABLE", 409, "This server's game can't message its players")

// resumable: a warned operation interrupted before it acted on the server,
// so it can wait out its warning and act after a restart of Ludock.
func resumable(job *ops.Context) bool {
	r := job.Job.Recovery
	return warnRequested(job) && r.WarnUntil != 0 && !r.StopStarted && r.InitialRunning == nil && !r.StartRequested &&
		r.BackupID == "" && r.DataHelperID == "" && len(r.Swap) == 0 && len(r.Restore) == 0
}

// calledOff says so when players were warned of something that then
// didn't happen.
func calledOff(job *ops.Context, err error) error {
	r := job.Job.Recovery
	if err == nil || !r.WarnSent || r.StopStarted || r.InitialRunning != nil || len(r.Swap) != 0 || apperr.Is(err, "WARNED_ACTION_MISSED") {
		return err
	}
	suffix := " Players were told the server would " + verbs[job.Job.Kind] + ", but it didn’t."
	if public, ok := apperr.As(err); ok {
		return apperr.New(public.Code, public.Status, strings.TrimSuffix(public.Message, ".")+"."+suffix)
	}
	return apperr.New("WARNED_ACTION_FAILED", 409, "Something went wrong."+suffix+" Check the server, then try again.")
}
