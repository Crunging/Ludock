package selfupdate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

// recordSetting holds the latest update. It is saved before anything runs
// and read by whichever Ludock comes up, so a Ludock reads every form an
// older one wrote.
const recordSetting = "ludock.update"

// States of a Record.
const (
	StatePreparing  = "preparing"
	StateHandedOver = "handed_over"
	StateSucceeded  = "succeeded"
	StatePutBack    = "put_back"
	StateFailed     = "failed"
	StateCurrent    = "current"
)

type Record struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	ActorID string `json:"actorId"`
	// From and To are versions; To is known once the new version runs.
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	// Container and Previous are Ludock's container and image ID before the
	// update; Target is the pulled image ID.
	Container  string `json:"container"`
	Previous   string `json:"previous"`
	Target     string `json:"target,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	// Reason says, for people, why it failed or was put back; Code is the
	// helper's reason code.
	Reason string `json:"reason,omitempty"`
	Code   string `json:"code,omitempty"`
	// Dismissed: an administrator has seen that it failed.
	Dismissed bool `json:"dismissed,omitempty"`
}

func (r *Record) InProgress() bool {
	return r != nil && (r.State == StatePreparing || r.State == StateHandedOver)
}

// Service updates Ludock's own container.
type Service struct {
	DB      *db.DB
	Docker  *docker.Client
	Own     *self.Container
	Updates *update.Service
	Checks  *update.Checks
	Locks   *locks.Table
	Notify  func(alert notify.Alert, key, message string)
	// Snapshot copies Ludock's data, one at a time with its other saves.
	Snapshot func(ctx context.Context) (*db.Snapshot, error)
	// DataDir and Socket are Ludock's data folder and Docker socket.
	DataDir, Socket string
	// SelfFound says how Ludock found its own container: "mounts" (its own
	// ID), "env" (LUDOCK_SELF_CONTAINER), or "hostname".
	SelfFound string
	// MountID is the container ID read from Ludock's own mounts, or "".
	MountID string
	Version string
	Poll    time.Duration

	mu   sync.Mutex
	base context.Context
	// lifts release each update's hold on work, by update.
	lifts map[string]func()
}

var (
	errUpdating = apperr.New("LUDOCK_UPDATING", 409, "Ludock is updating itself. Try again in a minute, once it is back.")
	errBusy     = apperr.New("OPERATION_CONFLICT", 409, "Wait for the work in progress to finish, then update Ludock.")
	errNotHere  = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock isn’t running in a container it can identify, so it can’t update itself. Use the commands below.")
	errPinned = apperr.New("UNSUPPORTED_IMAGE", 409,
		"Ludock’s image is pinned to a digest or image ID, so there is no newer version to update to. Change the image in its compose.yaml to move to another version.")
	errAutoRemove = apperr.New("SERVER_DELETED_ON_STOP", 409,
		"Ludock was started with --rm, so Docker would delete it as soon as it stopped. Update it with the commands below.")
	errUnsupported = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock’s container is set up in a way it can’t safely replace itself, so update it with the commands below.")
	errNoHealthcheck = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock’s container has its health check turned off, so an update couldn’t tell whether the new version works. Update it with the commands below.")
	errNoSocket = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock doesn’t reach Docker through a mounted socket, so it can’t hand its update to a helper. Update it with the commands below.")
	errSelfUnsure = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock can’t be sure which container is its own, so it won’t replace one. Unset LUDOCK_SELF_CONTAINER if it holds an ID, or set it to the container’s name, then use the commands below.")
	errNoDataMount = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock’s data folder isn’t on a volume or a mounted folder, so it would be lost with the container. Update it with the commands below.")
	errPull = apperr.New("PULL_FAILED", 409,
		"Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG.")
	errSave = apperr.New("LUDOCK_UPDATE_SAVE", 409,
		"Ludock couldn’t save a copy of its data before updating, so nothing was changed. Check free space where Ludock keeps its data.")
	errHelper = apperr.New("LUDOCK_UPDATE_HELPER", 409,
		"Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment.")
)

// reasons are the words for the helper's reason codes.
var reasons = map[string]string{
	ReasonImage:     "the image Ludock’s tag points to changed during the update",
	ReasonCreate:    "Docker couldn’t create the new container",
	ReasonStart:     "Docker couldn’t start the new container, for example because its port was still in use",
	ReasonStopped:   "the new version stopped right after it started",
	ReasonUnhealthy: "the new version reported itself unhealthy",
	ReasonTimeout:   "the new version didn’t report healthy within 3 minutes",
	ReasonNoHealth:  "the new image has no health check, so Ludock couldn’t confirm it works",
}

func (s *Service) poll() time.Duration {
	if s.Poll == 0 {
		return 2 * time.Second
	}
	return s.Poll
}

// Last is the latest update, or nil.
func (s *Service) Last(ctx context.Context) *Record {
	var record Record
	if found, err := s.DB.Setting(ctx, recordSetting, &record); err != nil || !found {
		return nil
	}
	return &record
}

// Failed is the latest update when it failed or was put back, until it is
// dismissed or Ludock runs another version.
func (s *Service) Failed(ctx context.Context) *Record {
	last := s.Last(ctx)
	if !last.Failed() || last.Dismissed || last.From != s.Version {
		return nil
	}
	return last
}

func (r *Record) Failed() bool {
	return r != nil && (r.State == StatePutBack || r.State == StateFailed)
}

var errNothingToDismiss = apperr.New("NOT_FOUND", 404, "There is no failed update of Ludock to dismiss.")

// Dismiss records that an administrator has seen the failed update id.
func (s *Service) Dismiss(ctx context.Context, adminID, tokenHash, id string) error {
	return authz.AdminMutationError(s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var record Record
		if found, err := db.ReadSetting(ctx, tx, recordSetting, &record); err != nil || !found || record.ID != id || !record.Failed() {
			return cmpErr(err, errNothingToDismiss)
		}
		record.Dismissed = true
		return s.DB.SetSetting(ctx, tx, recordSetting, record)
	}))
}

// Offer is whether an administrator can update Ludock from Settings.
type Offer struct {
	Available bool
	// Reason says why not, when there is one to give.
	Reason string
	// Pinned: Ludock's image names no tag that could move.
	Pinned   bool
	Updating bool
	Last     *Record
	// Failed is a failed update an administrator hasn't dismissed.
	Failed *Record
}

// Offer reports whether Update Ludock can be offered now.
func (s *Service) Offer(ctx context.Context) Offer {
	offer := Offer{Last: s.Last(ctx), Failed: s.Failed(ctx)}
	if offer.Last.InProgress() {
		offer.Updating = true
		return offer
	}
	if _, _, err := s.check(ctx, false); err != nil {
		if err != errNotHere {
			offer.Reason, _ = apperr.Public(err)
		}
		offer.Pinned = err == errPinned
		return offer
	}
	if busy, err := s.busy(ctx, s.DB); err != nil || busy {
		offer.Reason = errBusy.Message
		return offer
	}
	offer.Available = true
	return offer
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Service) busy(ctx context.Context, q queryer) (bool, error) {
	var busy bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE status IN ('queued','running'))").Scan(&busy)
	return busy, err
}

// current reads Ludock's own container afresh: an update gives its name to
// a new container.
func (s *Service) current(ctx context.Context) (*docker.ContainerInspect, error) {
	if s.Own == nil || s.Own.Name() == "" {
		return nil, errNotHere
	}
	info, err := s.Docker.Inspect(ctx, s.Own.Name())
	if status := docker.StatusCode(err); status == 404 || status == 403 {
		return nil, errNotHere
	}
	return info, err
}

// check confirms Ludock can replace its own container. dependents also
// scans the host for containers sharing its namespaces.
func (s *Service) check(ctx context.Context, dependents bool) (*docker.ContainerInspect, *update.Plan, error) {
	own, err := s.current(ctx)
	if err != nil {
		return nil, nil, err
	}
	// The new container must find itself as the old one did: by its own
	// ID, read from its mounts, or by the name it takes over. A hostname
	// may be stale, and an ID in the environment would name the old one.
	switch {
	case s.SelfFound == "mounts" && s.Own.Name() == own.ID:
	case s.SelfFound == "env" && "/"+s.Own.Name() == own.Name && (s.MountID == "" || s.MountID == own.ID):
	default:
		return own, nil, errSelfUnsure
	}
	plan, err := s.Updates.CheckContainer(ctx, own.ID, dependents)
	switch {
	case apperr.Is(err, "UNSUPPORTED_IMAGE"):
		return own, nil, errPinned
	case apperr.Is(err, servers.ErrAutoRemove.Code):
		return own, nil, errAutoRemove
	case err != nil:
		if _, public := apperr.As(err); public {
			return own, nil, errUnsupported
		}
		return own, nil, err
	}
	var config struct{ Healthcheck *struct{ Test []string } }
	if json.Unmarshal(encode(plan.Spec.Config), &config) != nil || config.Healthcheck == nil ||
		len(config.Healthcheck.Test) == 0 || config.Healthcheck.Test[0] == "NONE" {
		return own, nil, errNoHealthcheck
	}
	socket := false
	for _, mount := range plan.Spec.Mounts {
		socket = socket || (mount.Type == "bind" && filepath.Clean(mount.Destination) == filepath.Clean(s.Socket))
	}
	if !socket {
		return own, nil, errNoSocket
	}
	if dataDir, err := filepath.Abs(s.DataDir); err != nil || DataMount(plan.Spec.Mounts, dataDir) == nil {
		return own, nil, errNoDataMount
	}
	// The temporary name must keep the original's whole, so a helper run
	// again can tell them apart.
	if len(strings.TrimPrefix(plan.Spec.Name, "/")) > 128-len(servers.ReplacementName("", "")) {
		return own, nil, errUnsupported
	}
	return own, plan, nil
}

// Start begins updating Ludock for an administrator. Nothing may run or be
// queued; from here until the update settles no work can start.
func (s *Service) Start(ctx context.Context, adminID, tokenHash, ip string) error {
	own, plan, err := s.check(ctx, true)
	if err != nil {
		return err
	}
	lift := s.Locks.Hold(errUpdating)
	record := Record{ID: identity.NewUUID(), State: StatePreparing, ActorID: adminID, From: s.Version,
		Container: own.ID, Previous: own.Image, StartedAt: db.Now()}
	err = s.DB.AdminMutation(ctx, adminID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		var current Record
		if _, err := db.ReadSetting(ctx, tx, recordSetting, &current); err != nil {
			return err
		}
		if current.InProgress() {
			return errUpdating
		}
		if busy, err := s.busy(ctx, tx); err != nil || busy {
			return cmpErr(err, errBusy)
		}
		if err := s.DB.SetSetting(ctx, tx, recordSetting, record); err != nil {
			return err
		}
		return s.DB.WriteAudit(ctx, tx, db.AuditEntry{UserID: adminID, Action: "ludock.update.started", TargetType: "ludock",
			TargetID: record.ID, Details: map[string]string{"from": record.From}, IPAddress: ip})
	})
	if err != nil {
		lift()
		return authz.AdminMutationError(err)
	}
	s.hold(record.ID, lift)
	s.mu.Lock()
	base := s.base
	s.mu.Unlock()
	if base == nil {
		base = context.Background()
	}
	go s.run(base, record, plan)
	return nil
}

// run pulls the image, saves the data, and hands the update to a helper
// from the new image. Ludock stopping meanwhile leaves the record for the
// next start to settle.
func (s *Service) run(ctx context.Context, record Record, plan *update.Plan) {
	failed := func(err error) {
		if ctx.Err() == nil {
			message, _ := apperr.Public(err)
			s.settle(ctx, record, StateFailed, message)
		}
	}
	auth, err := s.Updates.RegistryAuth(plan)
	if err != nil {
		failed(err)
		return
	}
	pull, cancel := context.WithTimeout(ctx, 15*time.Minute)
	err = s.Docker.Pull(pull, plan.Image, auth, plan.Platform())
	cancel()
	if err != nil {
		failed(errPull)
		return
	}
	target, err := s.Docker.InspectImage(ctx, plan.Image)
	if err != nil {
		failed(errPull)
		return
	}
	if s.Checks != nil {
		s.Checks.ForgetSelf()
	}
	if target == record.Previous {
		s.settle(ctx, record, StateCurrent, "")
		return
	}
	// Release images name their version in a label; a build without one is
	// named once it runs.
	if image, err := s.Docker.InspectImageConfig(ctx, target); err == nil {
		var config struct{ Labels map[string]string }
		json.Unmarshal(encode(image.Config), &config)
		record.To = config.Labels["org.opencontainers.image.version"]
	}
	// Saved before the data save, so the save restored after a failed update
	// shows the update handed over.
	record.State, record.Target = StateHandedOver, target
	if err := s.DB.SetSetting(ctx, nil, recordSetting, record); err != nil {
		failed(err)
		return
	}
	if err := s.save(ctx, record.ID); err != nil {
		failed(errSave)
		return
	}
	if err := s.startHelper(ctx, record); err != nil {
		failed(errHelper)
		return
	}
	s.watch(ctx, record)
}

// settle is finish, retried until it is recorded or Ludock stops.
func (s *Service) settle(ctx context.Context, record Record, state, reason string) {
	for s.finish(ctx, record, state, reason) != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll()):
		}
	}
}

// save writes the data save beside the database.
func (s *Service) save(ctx context.Context, operation string) error {
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	final := filepath.Join(s.DataDir, SaveName(operation))
	partial := final + ".partial"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	if written, err := io.Copy(file, snapshot); err != nil || written != snapshot.Size {
		file.Close()
		os.Remove(partial)
		return cmpErr(err, errors.New("data save was cut short"))
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	return syncDir(s.DataDir)
}

// startHelper starts the helper from the pulled image, by ID, with only the
// Docker socket, mounted as Ludock's own is, and the interface's arguments.
// Docker runs it again after a host restart; it removes itself.
func (s *Service) startHelper(ctx context.Context, record Record) error {
	var socket *docker.MountPoint
	spec, err := s.Docker.InspectSpec(ctx, record.Container)
	if err != nil {
		return err
	}
	for index, mount := range spec.Mounts {
		if mount.Type == "bind" && filepath.Clean(mount.Destination) == filepath.Clean(s.Socket) {
			socket = &spec.Mounts[index]
		}
	}
	if socket == nil {
		return errNoSocket
	}
	var config struct{ User string }
	json.Unmarshal(encode(spec.Config), &config)
	var host struct{ GroupAdd, CapAdd []string }
	json.Unmarshal(encode(spec.HostConfig), &host)
	helperLabels := launch.HelperLabels(helperKind)
	helperLabels[operationLabel] = record.ID
	body := map[string]any{"Image": record.Target, "Entrypoint": []string{helper.Binary},
		"Cmd":  []string{"self-update", Version, record.Container, record.Target, record.ID},
		"Env":  []string{"LUDOCK_DOCKER_SOCKET=" + socket.Destination},
		"User": config.User, "Labels": helperLabels, "Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"},
			"CapAdd": host.CapAdd, "SecurityOpt": []string{"no-new-privileges:true"}, "GroupAdd": host.GroupAdd,
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			"Mounts":        []map[string]any{{"Type": "bind", "Source": socket.Source, "Target": socket.Destination, "ReadOnly": !socket.RW}}}}
	id, err := s.Docker.Create(ctx, HelperName(record.ID), "", body)
	if err != nil {
		// A create whose answer was lost may have made it; it never started.
		if spec, inspectErr := s.Docker.InspectSpec(ctx, HelperName(record.ID)); inspectErr == nil && labels(spec)[operationLabel] == record.ID {
			s.Docker.Remove(context.WithoutCancel(ctx), spec.ID, true, true)
		}
		return err
	}
	if err := s.Docker.Start(ctx, id); err != nil && !docker.Unavailable(err) {
		// It never ran, so nothing changed.
		s.Docker.Remove(context.WithoutCancel(ctx), id, true, true)
		return err
	}
	return nil
}

// handshake bounds how long a helper may run without recording its marker,
// which it does before it changes anything.
const handshake = 2 * time.Minute

// watch waits for the helper to settle the update and remove itself, then
// records how it ended. Ludock may be stopped by the helper meanwhile. A
// helper that never started, or never got as far as its marker, is ended.
func (s *Service) watch(ctx context.Context, record Record) {
	began := time.Now()
	for {
		spec, err := s.Docker.InspectSpec(ctx, HelperName(record.ID))
		switch {
		case docker.IsNotFound(err):
			if s.judge(ctx, record) == nil {
				return
			}
		case err == nil && labels(spec)[operationLabel] == record.ID && spec.State.Status == "exited":
			// Stopped by abandon as it recorded its marker, it goes on.
			s.Docker.Start(ctx, spec.ID)
		case err == nil && labels(spec)[operationLabel] == record.ID && (spec.State.Status == "created" || time.Since(began) >= handshake):
			if s.abandon(ctx, record, spec.ID) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll()):
		}
	}
}

// abandon ends an update whose helper changed nothing: with no marker once
// it is stopped, it never got that far. It reports whether it ended it.
func (s *Service) abandon(ctx context.Context, record Record, helper string) bool {
	marked := func() bool {
		_, err := s.Docker.InspectSpec(ctx, stateName(record.ID))
		return !docker.IsNotFound(err)
	}
	if marked() {
		return false
	}
	zero := 0
	if err := s.Docker.Stop(ctx, helper, &zero); err != nil && docker.StatusCode(err) != 304 {
		return false
	}
	if marked() {
		// It recorded its marker as it was stopped, so it goes on; watch
		// starts it again if this start fails.
		s.Docker.Start(ctx, helper)
		return false
	}
	if err := s.Docker.Remove(ctx, helper, true, true); err != nil && !docker.IsNotFound(err) {
		return false
	}
	s.removeLeftovers(record.ID)
	return s.finish(ctx, record, StateFailed, errHelper.Message) == nil
}

// judge records an update whose helper is gone, by whether this Ludock runs
// the pulled image, and removes what the helper left.
func (s *Service) judge(ctx context.Context, record Record) error {
	own, err := s.current(ctx)
	if err != nil && err != errNotHere {
		return err
	}
	state, reason := StateSucceeded, ""
	if own == nil {
		own = &docker.ContainerInspect{}
	}
	_, err = s.Docker.InspectSpec(ctx, stateName(record.ID))
	if err != nil && !docker.IsNotFound(err) {
		return err
	}
	marked := err == nil
	if own.Image != record.Target && !marked {
		// The helper records its marker before it changes anything.
		state, reason = StateFailed, errHelper.Message
	} else if own.Image != record.Target {
		state, reason = StatePutBack, "the update stopped before the new version started"
		spec, err := s.Docker.InspectSpec(ctx, undoName(record.ID))
		if err != nil && !docker.IsNotFound(err) {
			return err
		}
		if err == nil {
			var config struct{ Labels map[string]string }
			json.Unmarshal(encode(spec.Config), &config)
			record.Code = config.Labels[ReasonLabel]
			if reason = reasons[record.Code]; reason == "" {
				reason = "the new version didn’t come up"
			}
		}
	} else {
		record.To = s.Version
		// Its name, which Ludock may find itself by, is the new container's.
		s.Own.Forget()
	}
	if err := s.removeMarkers(ctx, record.ID); err != nil {
		log.Warn("Could not remove a container Ludock's update left; remove it with docker rm")
	}
	s.removeLeftovers(record.ID)
	return s.finish(ctx, record, state, reason)
}

// removeLeftovers removes the newer data a put-back moved aside, and every
// data save but this update's.
func (s *Service) removeLeftovers(operation string) {
	if err := os.RemoveAll(filepath.Join(s.DataDir, asideName(operation))); err != nil {
		log.Warn("Could not remove the newer data an update moved aside")
	}
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ludock-update-") && (strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.partial")) &&
			name != SaveName(operation) && entry.Type().IsRegular() {
			os.Remove(filepath.Join(s.DataDir, name))
		}
	}
}

// finish records how an update ended, with its audit entry, and lets work
// start again.
func (s *Service) finish(ctx context.Context, record Record, state, reason string) error {
	ctx = context.WithoutCancel(ctx)
	record.State, record.Reason, record.FinishedAt = state, reason, db.Now()
	outcome := map[string]string{StateSucceeded: "succeeded", StateCurrent: "already_current"}[state]
	if outcome == "" {
		outcome = "failed"
	}
	details := map[string]string{"from": record.From, "to": record.To, "reason": reason, "outcome": state}
	err := s.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := s.DB.SetSetting(ctx, tx, recordSetting, record); err != nil {
			return err
		}
		encoded, _ := json.Marshal(details)
		updated, err := tx.ExecContext(ctx, "UPDATE audit_log SET action=?, details_json=? WHERE target_type='ludock' AND target_id=? AND action='ludock.update.started'",
			"ludock.update."+outcome, string(encoded), record.ID)
		if err != nil {
			return err
		}
		if changed, err := updated.RowsAffected(); err != nil || changed > 0 {
			return err
		}
		entry := db.AuditEntry{Action: "ludock.update." + outcome, TargetType: "ludock", TargetID: record.ID, Details: details}
		if user, _ := db.FindUserIn(ctx, tx, record.ActorID); user != nil {
			entry.UserID = record.ActorID
		}
		return s.DB.WriteAudit(ctx, tx, entry)
	})
	if err != nil {
		log.Warn("Could not record how Ludock's update ended; retrying")
		return err
	}
	if (state == StatePutBack || state == StateFailed) && s.Notify != nil {
		s.Notify(notify.FailedWork, "ludock-update:"+record.ID, "Ludock couldn’t update itself: "+strings.TrimSuffix(reason, ".")+
			". It is still on version "+record.From+"; Needs attention says what to do.")
	}
	s.mu.Lock()
	lift := s.lifts[record.ID]
	delete(s.lifts, record.ID)
	s.mu.Unlock()
	if lift != nil {
		lift()
	}
	return nil
}

func (s *Service) hold(operation string, lift func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifts == nil {
		s.lifts = map[string]func(){}
	}
	s.lifts[operation] = lift
}

// Updating reports that an update holds work back. Changes people make
// meanwhile are refused: putting the old version back restores the data
// saved before, which would undo them.
func (s *Service) Updating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lifts) > 0
}

// ErrUpdating refuses a change while Ludock updates itself.
var ErrUpdating = errUpdating

// removeMarkers removes the update's markers, data step, and a helper that
// never started, each only if it carries the update's label, with the
// volumes the image gave them.
func (s *Service) removeMarkers(ctx context.Context, operation string) error {
	for _, name := range []string{stateName(operation), undoName(operation), dataName(operation), HelperName(operation)} {
		spec, err := s.Docker.InspectSpec(ctx, name)
		if docker.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		// A helper is left only by a create whose answer was lost, never
		// started.
		if labels(spec)[operationLabel] != operation || (name == HelperName(operation) && spec.State.Status != "created") {
			continue
		}
		if err := s.Docker.Remove(ctx, spec.ID, true, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Recover settles an update a restart interrupted, before work can start:
// one whose helper still runs holds work back until it is gone. base is
// the context later updates run in.
func (s *Service) Recover(ctx, base context.Context) error {
	s.mu.Lock()
	s.base = base
	s.mu.Unlock()
	record := s.Last(ctx)
	if record == nil {
		return nil
	}
	// What an update that ended left, if removing it failed then.
	if !record.InProgress() {
		return s.removeMarkers(ctx, record.ID)
	}
	if record.State == StatePreparing {
		return s.finish(ctx, *record, StateFailed, "Ludock restarted before the update began, so nothing changed.")
	}
	_, err := s.Docker.InspectSpec(ctx, HelperName(record.ID))
	if docker.IsNotFound(err) {
		return s.judge(ctx, *record)
	}
	if err != nil {
		return err
	}
	// Startup retries recovery; the update is watched once.
	s.mu.Lock()
	_, watched := s.lifts[record.ID]
	s.mu.Unlock()
	if !watched {
		s.hold(record.ID, s.Locks.Hold(errUpdating))
		go s.watch(base, *record)
	}
	return nil
}
