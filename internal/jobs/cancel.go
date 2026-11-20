package jobs

import (
	"context"
	"database/sql"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
)

// StartCapabilities are what starting an operation like job takes. Whoever
// holds them on its server may cancel it while it waits; nil means only
// administrators may.
func StartCapabilities(job *ops.Job) []authz.Capability {
	var capabilities []authz.Capability
	switch job.Kind {
	case "start", "stop", "restart":
		capabilities = []authz.Capability{authz.Capability("server." + job.Kind)}
		if job.Input["createBackup"] == true {
			capabilities = append(capabilities, authz.BackupsCreate)
		}
	case "backup":
		capabilities = []authz.Capability{authz.BackupsCreate}
	case "restore":
		capabilities = []authz.Capability{authz.BackupsRestore}
	case "update":
		capabilities = []authz.Capability{authz.ServerUpdate}
		if request, _ := job.Input["request"].(map[string]any); request["forceRecreate"] == true {
			capabilities = append(capabilities, authz.ServerRecreate)
		}
	case "command":
		capabilities = []authz.Capability{authz.ConsoleExecute}
	}
	return capabilities
}

// CanCancel reports whether user may cancel job, which must be waiting.
func (j *Jobs) CanCancel(ctx context.Context, user *db.SessionUser, job *ops.Job) bool {
	return ops.Waiting(job) && j.assertCanCancel(ctx, user, job, false) == nil
}

func (j *Jobs) assertCanCancel(ctx context.Context, user *db.SessionUser, job *ops.Job, audited bool) error {
	if user.Role == db.RoleAdmin {
		return nil
	}
	capabilities := StartCapabilities(job)
	if len(capabilities) == 0 {
		return errCannotCancel
	}
	for _, capability := range append([]authz.Capability{authz.ServerView}, capabilities...) {
		if audited {
			if _, err := j.Authz.Assert(ctx, user, job.ServerID, capability); err != nil {
				return err
			}
			continue
		}
		if ok, err := j.Authz.Has(ctx, user, job.ServerID, capability); err != nil || !ok {
			return errCannotCancel
		}
	}
	return nil
}

var errCannotCancel = apperr.New("FORBIDDEN", 403, "You can’t cancel this")

// Cancel calls off an operation on serverID that is still waiting, on
// behalf of user, whose access the caller has just rechecked; an
// administrator's session and role are checked again in the write, by
// tokenHash. Players who were warned hear that it's off.
func (j *Jobs) Cancel(ctx context.Context, user *db.SessionUser, tokenHash, serverID, id string) error {
	if _, err := j.Authz.Assert(ctx, user, serverID, authz.ServerView); err != nil {
		return err
	}
	job, err := j.runner.Get(ctx, id)
	if err != nil {
		return err
	}
	if job == nil || job.ServerID != serverID {
		return apperr.New("NOT_FOUND", 404, "Not found")
	}
	if err := j.assertCanCancel(ctx, user, job, true); err != nil {
		return err
	}
	var within func(func(*sql.Tx) error) error
	if user.Role == db.RoleAdmin {
		within = func(fn func(*sql.Tx) error) error { return j.DB.AdminMutation(ctx, user.ID, tokenHash, db.Now(), fn) }
	}
	before, err := j.runner.Cancel(ctx, id, user.ID, within)
	if err != nil {
		return err
	}
	// A warning sent and saved before the cancel is answered here; one still
	// going out is answered by the operation itself.
	if before.Recovery.WarnSent {
		j.sayCalledOff(context.WithoutCancel(ctx), before, before.BindingRevision)
	} else if heard := j.countedDown(ctx, before); heard != 0 {
		j.sayCalledOff(context.WithoutCancel(ctx), before, heard)
	}
	return nil
}

// countedDown spends the countdown a schedule sent for the run that queued
// job, reporting the binding revision players heard it on, or 0; schedules
// record their warnings by run, not on the operation. Whichever call-off
// spends it tells players.
func (j *Jobs) countedDown(ctx context.Context, job *ops.Job) int64 {
	scheduleID, scheduled := job.Input["scheduleId"].(string)
	if !scheduled {
		return 0
	}
	heard, err := schedules.CallOffQueued(ctx, j.DB, scheduleID, job.ID)
	if err != nil {
		return 0
	}
	return heard
}

// registrar registers handlers so that players counted down to a scheduled
// operation hear it's off when its turn comes and it is refused, because
// its schedule changed or its owner lost access, before it acts.
type registrar struct {
	runner *ops.Runner
	jobs   *Jobs
}

func (r registrar) Register(kind string, handler ops.Handler) {
	run := handler.Run
	handler.Run = func(ctx context.Context, job *ops.Context) (map[string]any, error) {
		if _, scheduled := job.Job.Input["scheduleId"].(string); scheduled {
			// A refusal is final: the handler never runs after players hear
			// it's off, even if access returns meanwhile.
			if err := r.refusal(ctx, job); err != nil {
				if _, public := apperr.As(err); public {
					if heard := r.jobs.countedDown(ctx, job.Job); heard != 0 {
						r.jobs.sayCalledOff(ctx, job.Job, heard)
					}
				}
				return nil, err
			}
		}
		return run(ctx, job)
	}
	r.runner.Register(kind, handler)
}

// refusal is why the owner of scheduled job can't start it now, if they
// can't.
func (r registrar) refusal(ctx context.Context, job *ops.Context) error {
	actor, err := r.jobs.Actor(ctx, job)
	if err != nil {
		return err
	}
	for _, capability := range StartCapabilities(job.Job) {
		if _, err := r.jobs.Authz.Assert(ctx, actor, job.Job.ServerID, capability); err != nil {
			return err
		}
	}
	return nil
}

// sayCalledOff tells players a warned operation is off, best effort, on
// binding, the revision they were warned on. Like the warning, it runs on
// Ludock's own authority: the operation's owner may be the one who lost
// access.
func (j *Jobs) sayCalledOff(ctx context.Context, job *ops.Job, binding int64) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	server, err := j.Servers.ResolveChecked(ctx, job.ServerID, binding, func(context.Context) error { return nil })
	if err != nil || server.Container.State != "running" {
		return
	}
	assertAccess := func() error {
		_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, binding)
		return err
	}
	if err := j.broadcast(ctx, server, schedules.CalledOffMessage(job.Kind), nil, assertAccess, false); err != nil {
		log.Debug("Could not tell players a warned operation was cancelled")
	}
}
