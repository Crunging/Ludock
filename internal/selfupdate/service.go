package selfupdate

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/notify"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

// recordSetting holds the latest update. It is saved before anything runs
// and read by whichever Ludock comes up, so a Ludock reads every form an
// older one wrote.
const recordSetting = db.UpdateSetting

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

func (r *Record) Failed() bool {
	return r != nil && (r.State == StatePutBack || r.State == StateFailed)
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
	// Snapshot copies Ludock's data; the update calls it only once Quiesce
	// holds back Ludock's own saves.
	Snapshot func(ctx context.Context) (*db.Snapshot, error)
	// Quiesce stops Ludock's background work, once what runs ends, until
	// resume; it is nil when there is none.
	Quiesce func(ctx context.Context) (resume func(), err error)
	// DataDir and Socket are Ludock's data folder and Docker socket.
	DataDir, Socket string
	// MountID is the container ID read from Ludock's own mounts, or "":
	// the only proof of which container this process runs in.
	MountID string
	// DevSelfProof accepts reading back a file in the data folder as that
	// proof instead, for a Ludock outside the fake container it stands for.
	DevSelfProof bool
	Version      string
	Poll         time.Duration

	// admit serializes starting an update with settling one, whose cleanup
	// prunes data saves.
	admit sync.Mutex
	mu    sync.Mutex
	base  context.Context
	// holds keep work back for each update, and for startup until it knows
	// whether one is in progress.
	holds map[string]*hold
	// proven are containers this process found itself in.
	proven map[string]bool
}

// hold is what keeps work back while Ludock updates itself: lock
// acquisitions, background passes, and writes.
type hold struct {
	lift, resume, reopen func()
}

const startupHold = "startup"

var (
	errUpdating = db.ErrUpdating
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
		"Ludock couldn’t confirm which container it runs in, so it won’t replace one. Use the commands below.")
	errNoDataMount = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock can’t reach its data folder the same way from a helper, so it couldn’t put its data back after a failed update. Update it with the commands below.")
	errPortNotFixed = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Docker picks Ludock’s port each time its container starts, so this page wouldn’t come back at the same address. Publish a fixed port, such as 3000:3000, or use the commands below.")
	errPull = apperr.New("PULL_FAILED", 409,
		"Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG.")
	errSave = apperr.New("LUDOCK_UPDATE_SAVE", 409,
		"Ludock couldn’t save a copy of its data before updating, so nothing was changed. Check free space where Ludock keeps its data.")
	errHelper = apperr.New("LUDOCK_UPDATE_HELPER", 409,
		"Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment.")
	errDrain = apperr.New("LUDOCK_UPDATE_BUSY", 409,
		"Ludock was still saving other changes, so nothing was changed. Try again in a moment.")
	errNoLongerAdmin = apperr.New("LUDOCK_UPDATE_ACTOR", 409,
		"The administrator who asked for the update signed out or isn’t one any more, so nothing was changed.")
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

// errUnusable is a stored record that names no update this version knows,
// such as one edited or imported by hand: nothing is done with its fields.
var errUnusable = errors.New("the stored update record is unusable")

func (s *Service) last(ctx context.Context) (*Record, error) {
	var record Record
	found, err := s.DB.Setting(ctx, recordSetting, &record)
	if err != nil || !found {
		return nil, err
	}
	if !identity.ValidID(record.ID) || !slices.Contains([]string{StatePreparing, StateHandedOver, StateSucceeded, StatePutBack,
		StateFailed, StateCurrent}, record.State) {
		return nil, errUnusable
	}
	return &record, nil
}

// Last is the latest update, or nil.
func (s *Service) Last(ctx context.Context) *Record {
	record, err := s.last(ctx)
	if err != nil {
		return nil
	}
	return record
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
	if offer.Last.InProgress() || s.Updating() {
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

// current reads Ludock's own container afresh: the one its mounts name,
// whatever its name is meanwhile, or, for development, the one it is called.
func (s *Service) current(ctx context.Context) (*docker.ContainerInspect, error) {
	reference := s.MountID
	if reference == "" && s.DevSelfProof && s.Own != nil {
		reference = s.Own.Name()
	}
	if reference == "" {
		return nil, errNotHere
	}
	info, err := s.Docker.Inspect(ctx, reference)
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
	if err := s.proveSelf(ctx, own); err != nil {
		return own, nil, err
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
	if s.socket(plan.Spec) == nil {
		return own, nil, errNoSocket
	}
	if dataDir, err := filepath.Abs(s.DataDir); err != nil {
		return own, nil, errNoDataMount
	} else if _, err := DataMount(own, dataDir); err != nil {
		return own, nil, errNoDataMount
	}
	var host struct {
		PublishAllPorts bool
		PortBindings    map[string][]docker.PortBinding
	}
	json.Unmarshal(encode(plan.Spec.HostConfig), &host)
	for _, bindings := range host.PortBindings {
		for _, binding := range bindings {
			if binding.HostPort == "" || binding.HostPort == "0" {
				host.PublishAllPorts = true
			}
		}
	}
	if host.PublishAllPorts {
		return own, nil, errPortNotFixed
	}
	// The temporary name must keep the original's whole, so a helper run
	// again can tell them apart.
	if len(strings.TrimPrefix(plan.Spec.Name, "/")) > 128-len(servers.ReplacementName("", "")) {
		return own, nil, errUnsupported
	}
	return own, plan, nil
}

// socket is the container's mount of Docker's socket, or nil.
func (s *Service) socket(spec *docker.ContainerSpec) *docker.MountPoint {
	for index, mount := range spec.Mounts {
		if mount.Type == "bind" && filepath.Clean(mount.Destination) == filepath.Clean(s.Socket) {
			return &spec.Mounts[index]
		}
	}
	return nil
}

// proveSelf confirms this process runs in own: by the ID its mounts show.
// A name or hostname could name another container, and a file only proves
// access to a folder another container may share, so that is accepted only
// for development.
func (s *Service) proveSelf(ctx context.Context, own *docker.ContainerInspect) error {
	if s.MountID != "" {
		if s.MountID != own.ID {
			return errSelfUnsure
		}
		return nil
	}
	if !s.DevSelfProof {
		return errSelfUnsure
	}
	s.mu.Lock()
	proven := s.proven[own.ID]
	s.mu.Unlock()
	if proven {
		return nil
	}
	dataDir, err := filepath.Abs(s.DataDir)
	if err != nil {
		return errSelfUnsure
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	file := filepath.Join(dataDir, ".ludock-self-"+hex.EncodeToString(nonce[:]))
	if err := os.WriteFile(file, []byte(hex.EncodeToString(nonce[:])), 0o600); err != nil {
		return errSelfUnsure
	}
	defer os.Remove(file)
	content, err := s.Docker.ReadFile(ctx, own.ID, file, 64)
	if err != nil || string(content) != hex.EncodeToString(nonce[:]) {
		return errSelfUnsure
	}
	s.mu.Lock()
	if s.proven == nil {
		s.proven = map[string]bool{}
	}
	s.proven[own.ID] = true
	s.mu.Unlock()
	return nil
}

// Start begins updating Ludock for an administrator. Nothing may run, be
// queued, or hold work, and startup recovery must have finished; from here
// until the update settles no work can start.
func (s *Service) Start(ctx context.Context, adminID, tokenHash, ip string) error {
	s.admit.Lock()
	defer s.admit.Unlock()
	if s.Updating() {
		return errUpdating
	}
	own, plan, err := s.check(ctx, true)
	if err != nil {
		return err
	}
	lift, err := s.Locks.Maintain(errUpdating)
	if errors.Is(err, locks.ErrConflict) {
		return errBusy
	}
	if err != nil {
		return err
	}
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
	s.keep(record.ID, &hold{lift: lift})
	go s.run(s.background(), record, plan, tokenHash)
	return nil
}

func (s *Service) background() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.base == nil {
		return context.Background()
	}
	return s.base
}

// run pulls the image, saves the data, and hands the update to a helper
// from the new image. Ludock stopping meanwhile leaves the record for the
// next start to settle.
func (s *Service) run(ctx context.Context, record Record, plan *update.Plan, tokenHash string) {
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
	digest, err := s.Docker.PullDigest(pull, plan.Image, auth, plan.Platform())
	cancel()
	if err != nil || digest == "" {
		failed(errPull)
		return
	}
	// The helper runs from what this pull fetched, whatever the tag points
	// to by now.
	target, err := resolveImage(ctx, s.Docker, repository(plan.Image)+"@"+digest, plan)
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
	// An older version can't open data a newer one wrote, so it would only
	// be put back.
	if older(record.To, record.From) {
		failed(apperr.New("LUDOCK_UPDATE_OLDER", 409, "The image Ludock’s tag points to is version "+record.To+
			", older than this one, which can’t use the data this version keeps. Change the image in its compose.yaml to update."))
		return
	}
	// Nothing is admitted after the save, since putting the old version back
	// restores it; what was admitted finishes first.
	if err := s.closeWrites(ctx, record.ID); err != nil {
		failed(errDrain)
		return
	}
	maintenance := db.Maintenance(ctx)
	// The pull can take minutes: the handover needs the same administrator's
	// session still, and no work queued meanwhile, which the save would
	// leave interrupted. It is saved before the data save, so the save
	// restored after a failed update shows the update handed over.
	record.State, record.Target = StateHandedOver, target
	err = s.DB.AdminMutation(maintenance, record.ActorID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if busy, err := s.busy(maintenance, tx); err != nil || busy {
			return cmpErr(err, errBusy)
		}
		return s.DB.SetSetting(maintenance, tx, recordSetting, record)
	})
	if err != nil {
		if public, ok := apperr.As(err); ok && public == errBusy {
			failed(errBusy)
		} else if errors.Is(err, db.ErrSessionGone) || errors.Is(err, db.ErrNotAdmin) {
			failed(errNoLongerAdmin)
		} else {
			failed(err)
		}
		return
	}
	if err := s.save(maintenance, record.ID); err != nil {
		failed(errSave)
		return
	}
	if step, err := s.startHelper(ctx, record); err != nil {
		log.Warn("Ludock's update helper didn't get going, so nothing was changed",
			append(logging.Cause(err, docker.StatusCode(err)), logging.F("step", step), logging.F("docker", docker.Said(err)))...)
		failed(errHelper)
		return
	}
	s.watch(ctx, record)
}

// repository is a reference without its tag or digest.
func repository(reference string) string {
	repository, _, _ := strings.Cut(reference, "@")
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		return repository[:colon]
	}
	return repository
}

// older reports whether version a is older than b, when both are releases.
func older(a, b string) bool {
	parse := func(version string) ([3]int, bool) {
		var parts [3]int
		fields := strings.Split(strings.TrimPrefix(version, "v"), ".")
		if len(fields) != 3 {
			return parts, false
		}
		for index, field := range fields {
			number, err := strconv.Atoi(field)
			if err != nil || number < 0 {
				return parts, false
			}
			parts[index] = number
		}
		return parts, true
	}
	left, ok := parse(a)
	right, ok2 := parse(b)
	if !ok || !ok2 {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
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
	saved := false
	defer func() {
		if !saved {
			file.Close()
			os.Remove(partial)
		}
	}()
	if written, err := io.Copy(file, snapshot); err != nil || written != snapshot.Size {
		return cmpErr(err, errors.New("data save was cut short"))
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	saved = true
	return syncDir(s.DataDir)
}

// startHelper starts the helper from the pulled image, by ID, with only the
// Docker socket, mounted as Ludock's own is, and the interface's arguments.
// Docker runs it again when it exits and after a daemon restart, even one
// stopped through Docker meanwhile; it removes itself.
func (s *Service) startHelper(ctx context.Context, record Record) (step string, err error) {
	spec, err := s.Docker.InspectSpec(ctx, record.Container)
	if docker.IsNotFound(err) {
		// Resumed once the original is gone: the replacement has its
		// definition.
		if own, ownErr := s.current(ctx); ownErr == nil {
			spec, err = s.Docker.InspectSpec(ctx, own.ID)
		}
	}
	if err != nil {
		return "inspect Ludock", err
	}
	socket := s.socket(spec)
	if socket == nil {
		return "find the socket", errNoSocket
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
			"RestartPolicy": map[string]any{"Name": "always"},
			"Mounts":        []map[string]any{{"Type": "bind", "Source": socket.Source, "Target": socket.Destination, "ReadOnly": !socket.RW}}}}
	id, err := s.Docker.Create(ctx, HelperName(record.ID), "", body)
	if err != nil {
		// A create whose answer was lost may have made it; it never started.
		if spec, inspectErr := s.Docker.InspectSpec(ctx, HelperName(record.ID)); inspectErr == nil && ours(spec, helperKind, record.ID) {
			s.Docker.Remove(context.WithoutCancel(ctx), spec.ID, true, true)
		}
		return "create", err
	}
	if err := s.Docker.Start(ctx, id); err != nil && !docker.Unavailable(err) {
		// It never ran, so nothing changed.
		s.Docker.Remove(context.WithoutCancel(ctx), id, true, true)
		return "start", err
	}
	return "", nil
}

func ours(spec *docker.ContainerSpec, kind, operation string) bool {
	found := labels(spec)
	return found[launch.InternalLabel] == kind && found[operationLabel] == operation
}

// marker inspects the update's marker of kind called name; known is false
// when Docker couldn't say. Another container under the name is no marker.
func (s *Service) marker(ctx context.Context, name, kind, operation string) (spec *docker.ContainerSpec, known bool) {
	spec, err := s.Docker.InspectSpec(ctx, name)
	if docker.IsNotFound(err) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	if !ours(spec, kind, operation) {
		return nil, true
	}
	return spec, true
}

// handshake bounds how long a helper may run without recording its marker,
// which it does before it changes anything.
const handshake = 2 * time.Minute

// watch waits for the helper to settle the update and remove itself, then
// records how it ended, saying meanwhile that this Ludock opened its data.
// Ludock may be stopped by the helper meanwhile. A helper that never
// started, or never got as far as its marker, is ended.
func (s *Service) watch(ctx context.Context, record Record) {
	began := time.Now()
	for {
		spec, err := s.Docker.InspectSpec(ctx, HelperName(record.ID))
		switch {
		case docker.IsNotFound(err), err == nil && !ours(spec, helperKind, record.ID):
			// Another container under the helper's name was never ours.
			if s.judge(ctx, record) == nil {
				return
			}
		case err == nil:
			s.signalReady(ctx, record)
			marked, known := s.marker(ctx, stateName(record.ID), stateKind, record.ID)
			switch {
			case !known:
			case spec.State.Status == "exited" && marked != nil:
				// Stopped by abandon as it recorded its marker, it goes on.
				s.Docker.Start(ctx, spec.ID)
			case marked == nil && (spec.State.Status == "created" || spec.State.Status == "exited" || time.Since(began) >= handshake):
				if s.abandon(ctx, record, spec) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll()):
		}
	}
}

// signalReady writes, for this Ludock's current run, that it opened its data
// and settles the update, which the helper waits for before it leaves.
func (s *Service) signalReady(ctx context.Context, record Record) {
	own, err := s.current(ctx)
	if err != nil {
		return
	}
	name := readyName(record.ID, own.ID)
	if spec, known := s.marker(ctx, name, readyKind, record.ID); !known {
		return
	} else if spec != nil {
		if found := labels(spec); found[containerLabel] == own.ID && found[startedLabel] == own.State.StartedAt {
			return
		}
		if err := s.Docker.Remove(ctx, spec.ID, true, true); err != nil && !docker.IsNotFound(err) {
			return
		}
	}
	name, body := ReadyMarker(record.ID, own.ID, own.State.StartedAt, own.Image)
	if _, err := s.Docker.Create(ctx, name, "", body); err != nil && docker.StatusCode(err) != 409 {
		log.Warn("Could not tell Ludock's update helper that Ludock opened its data; retrying")
	}
}

// ReadyMarker is the readiness marker of the Ludock in container, for its
// run that started at started, created from image.
func ReadyMarker(operation, container, started, image string) (name string, body map[string]any) {
	readyLabels := launch.HelperLabels(readyKind)
	readyLabels[operationLabel] = operation
	readyLabels[containerLabel] = container
	readyLabels[startedLabel] = started
	return readyName(operation, container), markerBody(image, readyLabels)
}

// abandon ends an update whose helper changed nothing: with no marker once
// it is stopped, it never got that far. It reports whether it ended it.
func (s *Service) abandon(ctx context.Context, record Record, spec *docker.ContainerSpec) bool {
	helper := spec.ID
	if marked, known := s.marker(ctx, stateName(record.ID), stateKind, record.ID); marked != nil || !known {
		return false
	}
	zero := 0
	if err := s.Docker.Stop(ctx, helper, &zero); err != nil && docker.StatusCode(err) != 304 {
		return false
	}
	if marked, known := s.marker(ctx, stateName(record.ID), stateKind, record.ID); !known {
		return false
	} else if marked != nil {
		// It recorded its marker as it was stopped, so it goes on; watch
		// starts it again if this start fails.
		s.Docker.Start(ctx, helper)
		return false
	}
	s.describe(ctx, spec)
	if err := s.Docker.Remove(ctx, helper, true, true); err != nil && !docker.IsNotFound(err) {
		return false
	}
	return s.conclude(ctx, record, StateFailed, errHelper.Message) == nil
}

// describe logs the state a helper was found in, before it is removed for
// never recording its marker, and its last log lines. Its log is Ludock's
// own, which redacts itself; Docker's words and every line are scrubbed again
// all the same.
func (s *Service) describe(ctx context.Context, spec *docker.ContainerSpec) {
	why := "it ran for 2 minutes without recording that it began"
	switch spec.State.Status {
	case "created":
		why = "it never started"
	case "exited":
		why = "it exited"
	}
	fields := []logging.Field{logging.F("why", why), logging.F("status", spec.State.Status),
		logging.F("exitCode", spec.State.ExitCode), logging.F("oomKilled", spec.State.OOMKilled),
		logging.F("docker", logging.Scrub(spec.State.Error))}
	logs, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if stream, err := s.Docker.Logs(logs, spec.ID, docker.LogsOptions{Stdout: true, Stderr: true, Tail: 10}); err == nil {
		var output bytes.Buffer
		docker.Demux(io.LimitReader(stream, 64*1024), &output, &output)
		stream.Close()
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, logging.Scrub(line))
			}
		}
		fields = append(fields, logging.F("helperLog", strings.Join(lines, " | ")))
	}
	log.Warn("Ludock's update helper didn't get going, so nothing was changed", fields...)
}

// errResumed: the update's helper was gone with nothing showing how the
// update ended, so it was started again to finish it.
var errResumed = errors.New("the update's helper was started again")

// judge records an update whose helper is gone, and removes what the helper
// left, only once Docker shows how it ended: the new container passed and
// took the name and policy, or the original runs again as before with its
// data settled. Anything else, such as a helper removed by hand mid-way,
// starts the helper again, which carries on from its markers.
func (s *Service) judge(ctx context.Context, record Record) error {
	own, err := s.current(ctx)
	if err != nil && err != errNotHere {
		return err
	}
	if own == nil {
		own = &docker.ContainerInspect{}
	}
	marked, known := s.marker(ctx, stateName(record.ID), stateKind, record.ID)
	if !known {
		return errors.New("docker couldn't say whether the helper began")
	}
	if marked == nil {
		if own.Image == record.Target {
			// Only a marker removed by hand gets here: nothing says what
			// the original's name and policy were.
			log.Warn("Ludock's update can't tell how it ended because a container it left was removed; Ludock stays read-only. Run docker compose up -d in the folder with its compose.yaml")
			return errors.New("the update's marker is gone")
		}
		// The helper records its marker before it changes anything.
		log.Warn("Ludock's update helper was gone before it recorded that it began, so nothing was changed")
		return s.conclude(ctx, record, StateFailed, errHelper.Message)
	}
	found := labels(marked)
	named := false
	if own.ID != "" {
		spec, err := s.Docker.InspectSpec(ctx, own.ID)
		if err != nil {
			return err
		}
		named = own.Name == "/"+found[nameLabel] && samePolicy(spec.HostConfig["RestartPolicy"], json.RawMessage(found[policyLabel]))
	}
	healthy, known := s.marker(ctx, healthyName(record.ID), healthyKind, record.ID)
	if !known {
		return errors.New("docker couldn't say whether the new version passed")
	}
	if own.Image == record.Target && named && healthy != nil && labels(healthy)[containerLabel] == own.ID {
		if _, err := s.Docker.InspectSpec(ctx, record.Container); docker.IsNotFound(err) {
			record.To = s.Version
			// Its name, which Ludock may find itself by, is the new container's.
			s.Own.Forget()
			return s.conclude(ctx, record, StateSucceeded, "")
		} else if err != nil {
			return err
		}
	}
	undone, known := s.marker(ctx, undoName(record.ID), undoKind, record.ID)
	if !known {
		return errors.New("docker couldn't say why the update was undone")
	}
	if own.ID == record.Container && named && undone != nil {
		settled := readSettled(s.DataDir, record.ID)
		restore, valid := restoreFlag(undone)
		_, err := s.Docker.InspectSpec(ctx, servers.ReplacementName(found[nameLabel], record.ID))
		if err != nil && !docker.IsNotFound(err) {
			return err
		}
		if valid && (!restore || settled == "restored" || settled == "kept") && docker.IsNotFound(err) {
			record.Code = labels(undone)[ReasonLabel]
			reason := reasons[record.Code]
			if reason == "" {
				reason = "the new version didn’t come up"
			}
			if restore && settled == "kept" {
				reason += ", and Ludock couldn’t put its data back as it was saved, so it kept the newer data; the save " + SaveName(record.ID) +
					" is beside it"
			}
			return s.conclude(ctx, record, StatePutBack, reason)
		}
	}
	log.Warn("Ludock's update helper was removed before the update finished; starting it again to finish it")
	if step, err := s.startHelper(ctx, record); err != nil {
		log.Warn("Ludock's update helper couldn't be started again; retrying",
			append(logging.Cause(err, docker.StatusCode(err)), logging.F("step", step), logging.F("docker", docker.Said(err)))...)
		return err
	}
	return errResumed
}

// samePolicy reports whether two restart policies, as Docker reports them,
// are the same.
func samePolicy(a, b json.RawMessage) bool {
	type restart struct {
		Name              string
		MaximumRetryCount int
	}
	var left, right restart
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	return cmp.Or(left.Name, "no") == cmp.Or(right.Name, "no") && left.MaximumRetryCount == right.MaximumRetryCount
}

// conclude records the outcome, then removes the update's markers and
// prunes data saves, and only then lets work start again: a restart between
// the two records it the same way, and the next start removes what is left.
func (s *Service) conclude(ctx context.Context, record Record, state, reason string) error {
	s.admit.Lock()
	defer s.admit.Unlock()
	settled, err := s.record(ctx, record, state, reason)
	if err != nil {
		return err
	}
	// Whoever recorded it removes what is left.
	if settled {
		if err := s.removeMarkers(ctx, record.ID); err != nil {
			log.Warn("Could not remove a container Ludock's update left; remove it with docker rm")
		}
		s.removeLeftovers(record.ID)
	}
	s.release(record.ID)
	if settled {
		s.announce(record, state, reason)
	}
	return nil
}

// readSettled reads how the data step settled, never following a link or
// waiting on anything but a regular file.
func readSettled(dataDir, operation string) string {
	dir, err := unix.Open(filepath.Join(dataDir, asideName(operation)), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, "settled", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	file := os.NewFile(uintptr(fd), "settled")
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	content, _ := io.ReadAll(io.LimitReader(file, 64))
	return strings.TrimSpace(string(content))
}

// removeLeftovers removes the newer data a put-back moved aside and every
// partial save, and, once this update wrote its save, every other.
// Every removal stays inside the data folder, following no link.
func (s *Service) removeLeftovers(operation string) {
	if !identity.ValidID(operation) {
		return
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return
	}
	defer root.Close()
	if err := root.RemoveAll(asideName(operation)); err != nil {
		log.Warn("Could not remove the newer data an update moved aside")
	}
	info, err := root.Lstat(SaveName(operation))
	complete := err == nil && info.Mode().IsRegular()
	entries, err := os.ReadDir(s.DataDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		id, partial := strings.CutSuffix(strings.TrimPrefix(name, "ludock-update-"), ".tar.partial")
		if !partial {
			id = strings.TrimSuffix(strings.TrimPrefix(name, "ludock-update-"), ".tar")
		}
		// Only saves an update named; an owner's own archive is left alone.
		if !identity.ValidID(id) || SaveName(id) != strings.TrimSuffix(name, ".partial") || !entry.Type().IsRegular() || name == SaveName(operation) {
			continue
		}
		if partial || complete {
			root.Remove(name)
		}
	}
}

// removeMarkers removes the update's markers, data step, and a helper that
// never started, each only if it is the update's of its kind, with the
// volumes the image gave them.
func (s *Service) removeMarkers(ctx context.Context, operation string) error {
	names := map[string]string{stateName(operation): stateKind, healthyName(operation): healthyKind, undoName(operation): undoKind,
		dataName(operation): dataKind, HelperName(operation): helperKind}
	containers, err := s.Docker.ListContainers(ctx, true, docker.Filters{"label": {operationLabel + "=" + operation}})
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.Labels[launch.InternalLabel] == readyKind && container.Labels[operationLabel] == operation {
			for _, name := range container.Names {
				names[strings.TrimPrefix(name, "/")] = readyKind
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(names)) {
		kind := names[name]
		spec, known := s.marker(ctx, name, kind, operation)
		if !known {
			return errors.New("docker couldn't say what the update left")
		}
		// A helper is left only by a create whose answer was lost, never
		// started.
		if spec == nil || (kind == helperKind && spec.State.Status != "created") {
			continue
		}
		if err := s.Docker.Remove(ctx, spec.ID, true, true); err != nil && !docker.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// finish records how an update ended and lets work start again.
func (s *Service) finish(ctx context.Context, record Record, state, reason string) error {
	settled, err := s.record(ctx, record, state, reason)
	if err != nil {
		return err
	}
	s.release(record.ID)
	if settled {
		s.announce(record, state, reason)
	}
	return nil
}

// record saves how an update ended, with its audit entry, unless it is no
// longer the update in progress: whoever settled it first recorded it.
func (s *Service) record(ctx context.Context, record Record, state, reason string) (settled bool, err error) {
	ctx = db.Maintenance(context.WithoutCancel(ctx))
	record.State, record.Reason, record.FinishedAt = state, reason, db.Now()
	outcome := map[string]string{StateSucceeded: "succeeded", StateCurrent: "already_current"}[state]
	if outcome == "" {
		outcome = "failed"
	}
	details := map[string]string{"from": record.From, "to": record.To, "reason": reason, "outcome": state}
	err = s.DB.Tx(ctx, func(tx *sql.Tx) error {
		var current Record
		if _, err := db.ReadSetting(ctx, tx, recordSetting, &current); err != nil {
			return err
		}
		if current.ID != record.ID || !current.InProgress() {
			return nil
		}
		settled = true
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
		return false, err
	}
	return settled, nil
}

// announce alerts about a failed update, once writes are admitted again.
func (s *Service) announce(record Record, state, reason string) {
	if (state == StatePutBack || state == StateFailed) && s.Notify != nil {
		s.Notify(notify.FailedWork, "ludock-update:"+record.ID, "Ludock couldn’t update itself: "+strings.TrimSuffix(reason, ".")+
			". It is still on version "+record.From+"; Needs attention says what to do.")
	}
}

func (s *Service) keep(operation string, held *hold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]*hold{}
	}
	s.holds[operation] = held
}

// closeWrites holds back background work and closes writes for an update,
// once those admitted end. It doesn't wait longer than a moment.
func (s *Service) closeWrites(ctx context.Context, operation string) error {
	s.mu.Lock()
	held := s.holds[operation]
	s.mu.Unlock()
	if held == nil {
		return errors.New("the update holds no work back")
	}
	drain, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.Quiesce != nil && held.resume == nil {
		resume, err := s.Quiesce(drain)
		if err != nil {
			return err
		}
		held.resume = resume
	}
	reopen, err := s.DB.CloseWrites(drain)
	if err != nil {
		return err
	}
	held.reopen = reopen
	return nil
}

// release lets work start again after an update. A Ludock that handed
// itself over never gets here: it is stopped with work held back.
func (s *Service) release(operation string) {
	s.mu.Lock()
	held := s.holds[operation]
	delete(s.holds, operation)
	s.mu.Unlock()
	if held == nil {
		return
	}
	for _, step := range []func(){held.reopen, held.resume, held.lift} {
		if step != nil {
			step()
		}
	}
}

// Updating reports that an update holds work back.
func (s *Service) Updating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.holds) > 0
}

// holdAll holds every kind of work back at once, as at startup, when
// nothing has run yet to wait for.
func (s *Service) holdAll(ctx context.Context) *hold {
	held := &hold{lift: s.Locks.Hold(errUpdating)}
	if s.Quiesce != nil {
		held.resume, _ = s.Quiesce(ctx)
	}
	held.reopen, _ = s.DB.CloseWrites(ctx)
	return held
}

// Prepare holds work back from the start, before Ludock serves anything,
// while an update may be in progress: one whose data save is put back would
// undo whatever was written meanwhile. Recover settles or lifts it.
func (s *Service) Prepare(ctx context.Context) {
	if record, err := s.last(ctx); err == nil && !record.InProgress() {
		return
	}
	s.keep(startupHold, s.holdAll(ctx))
}

// Recover settles an update a restart interrupted, before work can start:
// one whose helper still runs holds work back until it is gone. base is
// the context later updates run in.
func (s *Service) Recover(ctx, base context.Context) error {
	s.admit.Lock()
	s.mu.Lock()
	s.base = base
	s.mu.Unlock()
	record, err := s.last(ctx)
	if errors.Is(err, errUnusable) {
		// Its state may still close writes in the database, so it goes.
		log.Warn("Ludock's stored update record is unusable, so it was removed; nothing of an update was cleaned up")
		err = s.DB.DeleteSetting(db.Maintenance(ctx), recordSetting)
	}
	if err != nil {
		s.admit.Unlock()
		return err
	}
	if !record.InProgress() {
		defer s.admit.Unlock()
		if record != nil {
			// What an update that ended left, if removing it failed then.
			s.removeLeftovers(record.ID)
			if err := s.removeMarkers(ctx, record.ID); err != nil {
				return err
			}
		}
		s.release(startupHold)
		return nil
	}
	s.mu.Lock()
	_, watched := s.holds[record.ID]
	startup := s.holds[startupHold]
	if !watched {
		delete(s.holds, startupHold)
	}
	s.mu.Unlock()
	// Startup retries recovery; a watcher already settles this update.
	if watched {
		s.admit.Unlock()
		return nil
	}
	if startup == nil {
		startup = s.holdAll(ctx)
	}
	s.keep(record.ID, startup)
	s.admit.Unlock()
	// Settled here or not at all: a failure hands the hold back to startup,
	// which tries again.
	retry := func(err error) error {
		if err != nil {
			s.mu.Lock()
			if s.holds[record.ID] == startup {
				delete(s.holds, record.ID)
				s.holds[startupHold] = startup
			}
			s.mu.Unlock()
		}
		return err
	}
	if record.State == StatePreparing {
		return retry(s.finish(ctx, *record, StateFailed, "Ludock restarted before the update began, so nothing changed."))
	}
	spec, err := s.Docker.InspectSpec(ctx, HelperName(record.ID))
	if docker.IsNotFound(err) || (err == nil && !ours(spec, helperKind, record.ID)) {
		if err = s.judge(ctx, *record); !errors.Is(err, errResumed) {
			return retry(err)
		}
	} else if err != nil {
		return retry(err)
	}
	go s.watch(base, *record)
	return nil
}
