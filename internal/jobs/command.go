package jobs

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/schedules"
)

const (
	// MaxCommandReply is how much of a scheduled command's reply is kept, in
	// bytes, after redaction.
	MaxCommandReply = 500
	commandTimeout  = 60 * time.Second
	// commandLate is how long after queueing a command may still be sent.
	commandLate = time.Minute
)

func commandFailed(message string) error { return apperr.New("COMMAND_FAILED", 409, message) }

// skipped ends a run without sending its command, as the scheduler would.
func skipped(reason schedules.Skip) (*ops.Result, error) {
	return &ops.Result{Skipped: true, Guidance: string(reason)}, nil
}

// scheduledCommand reads the command from the schedule that queued job, at
// the revision it was queued from. Operations never store the command.
func (j *Jobs) scheduledCommand(ctx context.Context, job *ops.Context) (string, error) {
	scheduleID, _ := job.Job.Input["scheduleId"].(string)
	expected, _ := number(job.Job.Input["scheduleRevision"])
	var input string
	var revision int64
	err := j.DB.QueryRowContext(ctx, "SELECT input_json,revision FROM schedules WHERE id=? AND server_id=?",
		scheduleID, job.Job.ServerID).Scan(&input, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errScheduleRevoked
	}
	if err != nil {
		return "", err
	}
	settings, err := schedules.ParseSaved(input)
	if err != nil || float64(revision) != expected || settings.Action != "command" {
		return "", errScheduleRevoked
	}
	return settings.Command, nil
}

// reply keeps the start of redacted console output, on one line.
type reply struct {
	text strings.Builder
}

func (r *reply) push(value string) {
	if r.text.Len() <= 4*MaxCommandReply {
		r.text.WriteString(value)
	}
}

func (r *reply) String() string {
	text := strings.Join(strings.Fields(r.text.String()), " ")
	if len(text) <= MaxCommandReply {
		return text
	}
	cut := MaxCommandReply - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// runCommand sends a scheduled console command once, through the game
// console adapter. A run that can't start at once is not sent later.
func (j *Jobs) runCommand(ctx context.Context, job *ops.Context) (*ops.Result, error) {
	if _, scheduled := job.Job.Input["scheduleId"].(string); !scheduled {
		return nil, errScheduleRevoked
	}
	initial, err := j.resolve(ctx, job, authz.ConsoleExecute, true)
	if err != nil {
		return nil, err
	}
	release, err := j.Locks.AcquireYielding(ctx, initial.LockKeys)
	if errors.Is(err, locks.ErrConflict) {
		return skipped(schedules.SkipBusy)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	server, err := j.resolve(ctx, job, authz.ConsoleExecute, true)
	if err != nil {
		return nil, err
	}
	command, err := j.scheduledCommand(ctx, job)
	if err != nil {
		return nil, err
	}
	adapter := games.ResolveAdapter(server.Container.GameType, server.Container.Labels)
	if adapter == nil {
		return skipped(schedules.SkipNoConsole)
	}
	if server.Container.State != "running" {
		return skipped(schedules.SkipNotRunning)
	}
	assertAccess := func() error {
		if err := j.assertCapability(ctx, job, authz.ConsoleExecute); err != nil {
			return err
		}
		_, err := j.Identity.AssertObservedBinding(ctx, server.Logical.ID, server.Observation, job.Job.BindingRevision)
		return err
	}
	// Persisted first: an interrupted send is reported, never repeated.
	if err := job.Progress("sending_command", nil, &ops.Result{}); err != nil {
		return nil, err
	}
	if db.Now()-job.Job.CreatedAt > commandLate.Milliseconds() {
		return skipped("Skipped: it couldn't be sent at its scheduled time")
	}
	var output reply
	redactor := console.NewRedactor(server.Observation.Secrets, output.push)
	run, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	err = j.Console.GameCommand(run, console.Target{ContainerID: server.Container.ID, Labels: server.Container.Labels,
		State: server.Container.State}, adapter, command, console.Output{Stdout: redactor.Push, Stderr: redactor.Push,
		Complete: redactor.End}, assertAccess)
	if err != nil {
		if accessErr := assertAccess(); accessErr != nil {
			return nil, accessErr
		}
		message := "The game console didn't accept the command"
		if public, ok := apperr.As(err); ok {
			message = public.Message
		} else if console.IsTimeout(err) {
			message = "The game console didn't answer in time; the command may not have run"
		}
		return nil, commandFailed(message)
	}
	return &ops.Result{Sent: true, Reply: output.String()}, nil
}
