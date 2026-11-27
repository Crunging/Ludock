package selfupdate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
)

// recordSetting holds the latest update, saved before it hands over, so the
// Ludock that runs next, and the data copy, know of it.
const recordSetting = "ludock.update"

// States of a Record.
const (
	StateUpdating  = "updating"
	StateSucceeded = "succeeded"
	StatePutBack   = "put_back"
	// StateFailed: nothing was changed.
	StateFailed = "failed"
	// StateInterrupted: the update stopped partway, and the owner finishes
	// it from a terminal.
	StateInterrupted = "interrupted"
	StateCurrent     = "current"
)

type Record struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	ActorID string `json:"actorId"`
	// From and To are versions. Until the update succeeds, To is the version
	// the pulled image advertises, if any; after it, the new binary's own.
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	// Container, Name, and Previous are Ludock's container, its name, and
	// image ID before the update; Target is the pulled image ID.
	Container  string `json:"container"`
	Name       string `json:"name"`
	Previous   string `json:"previous"`
	Target     string `json:"target,omitempty"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	// Reason says, for people, why it failed or was put back.
	Reason string `json:"reason,omitempty"`
	Data   string `json:"data,omitempty"`
	// Leftover is the new container an interrupted update left under its
	// temporary name, for the owner to remove; Copy, that the data copy
	// from before the update is still beside the database.
	Leftover string `json:"leftover,omitempty"`
	Copy     bool   `json:"copy,omitempty"`
	// OnLeftover: the Ludock that recorded it runs in Leftover;
	// OriginalGone, that Ludock's original container no longer exists.
	OnLeftover   bool `json:"onLeftover,omitempty"`
	OriginalGone bool `json:"originalGone,omitempty"`
	// Policy is Ludock's restart policy as the update began; NoPolicy, that
	// the original couldn't get it back.
	Policy    json.RawMessage `json:"policy,omitempty"`
	NoPolicy  bool            `json:"noPolicy,omitempty"`
	Dismissed bool            `json:"dismissed,omitempty"`
	// Finished, read from Docker as an interrupted update is shown: nothing
	// it left is there any more, with Running the version that runs now.
	Finished bool   `json:"-"`
	Running  string `json:"-"`
	// Unchecked: Docker couldn't say whether the original still exists.
	Unchecked bool `json:"-"`
}

func (r *Record) InProgress() bool { return r != nil && r.State == StateUpdating }

func (r *Record) Failed() bool {
	return r != nil && (r.State == StatePutBack || r.State == StateFailed || r.State == StateInterrupted)
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
	// PutBack is what BeforeOpen did with a put-back's data as this Ludock
	// started.
	PutBack string
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

	mu   sync.Mutex
	base context.Context
	// lift ends the hold on work an update keeps.
	lift func()
	// proven are containers this process found itself in.
	proven map[string]bool
}

var (
	errUpdating = ops.ErrLudockUpdating
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
	errOwnHealthcheck = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock’s container uses a health check of its own instead of the image’s, which might pass a version that can’t open Ludock’s data. Update it with the commands below.")
	errSocketLabel = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock’s Docker socket is relabelled for its container alone (:Z), so a helper couldn’t reach it. Mount it with :z, or use the commands below.")
	errPortNotFixed = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Docker picks Ludock’s port each time its container starts, so this page wouldn’t come back at the same address. Publish a fixed port, such as 3000:3000, or use the commands below.")
	errNoDataMount = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"Ludock keeps its data inside its own container rather than in a volume or mounted folder, so recreating it would lose its data. Move its data to a volume or mounted folder first, then update.")
	errEarlier = apperr.New("LUDOCK_UPDATE_UNAVAILABLE", 409,
		"An earlier update of Ludock didn’t finish. Finish it as shown above, then update again.")
	errPull = apperr.New("PULL_FAILED", 409,
		"Docker couldn’t download Ludock’s new image. Check the machine’s internet connection; for a private registry, set LUDOCK_DOCKER_CONFIG.")
	errSave = apperr.New("LUDOCK_UPDATE_SAVE", 409,
		"Ludock couldn’t save a copy of its data before updating, so nothing was changed. Check free space where Ludock keeps its data.")
	errHelper = apperr.New("LUDOCK_UPDATE_HELPER", 409,
		"Docker couldn’t start the helper that replaces Ludock, so nothing was changed. Try again in a moment.")
	errNoLongerAdmin = apperr.New("LUDOCK_UPDATE_ACTOR", 409,
		"The administrator who asked for the update signed out or isn’t one any more, so nothing was changed.")
)

const (
	reasonUnchanged   = "Docker couldn’t swap in Ludock’s new container, so nothing was changed"
	reasonCutOff      = "the update was cut off, for example by a restart, before the new version was accepted"
	reasonNotReplaced = "the update stopped before Ludock was replaced, so nothing was changed"
	reasonInterrupted = "the update stopped partway"
)

func (s *Service) poll() time.Duration {
	if s.Poll == 0 {
		return 2 * time.Second
	}
	return s.Poll
}

// Last is the latest update, or nil.
func (s *Service) Last(ctx context.Context) *Record {
	var record Record
	if found, err := s.DB.Setting(ctx, recordSetting, &record); err != nil || !found || !identity.ValidID(record.ID) {
		return nil
	}
	return &record
}

// Failed is the latest update when it didn't succeed, until it is dismissed
// or Ludock runs another version.
func (s *Service) Failed(ctx context.Context) *Record {
	last := s.Last(ctx)
	// One that stopped partway stays until dismissed, whichever version
	// the owner then started.
	if !last.Failed() || (last.From != s.Version && last.State != StateInterrupted) {
		return nil
	}
	if last.State == StateInterrupted {
		s.present(ctx, last)
	}
	// One that left something behind shows until that is gone.
	if last.Dismissed && (last.State != StateInterrupted || last.Finished) {
		return nil
	}
	return last
}

// present drops from an interrupted update what the owner has since
// finished: a leftover container gone, as after docker rm or a rename, and
// the copy moved.
func (s *Service) present(ctx context.Context, record *Record) {
	// Nothing is left only once Docker shows this update's new container
	// gone, whatever was recorded.
	replacement, err := s.Docker.InspectSpec(ctx, servers.ReplacementName(record.Name, record.ID))
	gone := docker.IsNotFound(err)
	if gone {
		record.Leftover, record.OnLeftover, record.OriginalGone = "", false, false
	} else if err == nil && operationOf(replacement) == record.ID {
		record.Leftover = strings.TrimPrefix(replacement.Name, "/")
	}
	// Removing the container this Ludock runs in is advised only while
	// Docker shows the original, which may hold an anonymous data volume.
	// Only Docker showing it gone counts; while it can't say, no command.
	if record.OnLeftover {
		_, err := s.Docker.InspectSpec(ctx, record.Container)
		record.OriginalGone, record.Unchecked = docker.IsNotFound(err), err != nil && !docker.IsNotFound(err)
	}
	_, err = os.Lstat(filepath.Join(s.DataDir, CopyName))
	record.Copy = err == nil
	_, helper := s.Docker.InspectSpec(ctx, HelperName(record.ID))
	record.Finished, record.Running = gone && docker.IsNotFound(helper), s.Version
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
	// Pinned: Ludock's image names no tag that could move. NoCommands: the
	// commands that recreate it would lose its data or interrupt work now.
	// Refused: a click on Update Ludock failed for Reason.
	Pinned, NoCommands, Refused bool
	// CopyLeft: an earlier update's copy of the data, which this one
	// replaces, is beside the database.
	CopyLeft bool
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
	busy, err := s.busy(ctx, s.DB)
	working := err != nil || busy
	if _, _, err := s.check(ctx, false); err != nil {
		if err != errNotHere {
			offer.Reason, _ = apperr.Public(err)
		}
		offer.Pinned, offer.NoCommands = err == errPinned, noCommands(err)
		// The commands would interrupt work too, whatever else stops the
		// button.
		if working && !offer.Pinned && !offer.NoCommands {
			offer.Reason, offer.NoCommands = errBusy.Message, true
		}
		return offer
	}
	if working {
		offer.Reason, offer.NoCommands = errBusy.Message, true
		return offer
	}
	// A copy an interrupted update left is no longer needed by a Ludock
	// that opened its data and settled it, so the next update replaces it.
	_, err = os.Lstat(filepath.Join(s.DataDir, CopyName))
	offer.Available, offer.CopyLeft = true, err == nil
	return offer
}

// Refuse is the offer after err refused a click on Update Ludock.
func (o *Offer) Refuse(err error) {
	o.Reason, _ = apperr.Public(err)
	o.Available, o.Refused, o.NoCommands = false, true, o.NoCommands || noCommands(err)
}

func noCommands(err error) bool {
	return errors.Is(err, errNoDataMount) || errors.Is(err, errBusy) || errors.Is(err, errEarlier)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
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
	// First, since the commands shown otherwise would lose data kept
	// inside the container: a new one has it only from a volume or folder.
	if dataDir, err := filepath.Abs(s.DataDir); err != nil || !mounted(own, dataDir) {
		return own, nil, errNoDataMount
	}
	// Another update would replace the record of one whose leftovers
	// Settings still says how to finish.
	if last := s.Last(ctx); last != nil && last.State == StateInterrupted {
		if s.present(ctx, last); !last.Finished {
			return own, nil, errEarlier
		}
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
	test := healthTest(plan.Spec.Config)
	if len(test) == 0 || test[0] == "NONE" {
		return own, nil, errNoHealthcheck
	}
	// The image's check fails when Ludock refuses its data; one of the
	// container's own, such as of the port alone, may pass its refusal page.
	image, err := s.Docker.InspectImageConfig(ctx, own.Image)
	if err != nil {
		return own, nil, err
	}
	if !slices.Equal(test, healthTest(image.Config)) {
		return own, nil, errOwnHealthcheck
	}
	if s.socket(plan.Spec) == nil {
		return own, nil, errNoSocket
	}
	if relabelled(own, s.Socket) {
		return own, nil, errSocketLabel
	}
	var host struct {
		PublishAllPorts bool
		PortBindings    map[string][]docker.PortBinding
	}
	json.Unmarshal(encode(plan.Spec.HostConfig), &host)
	// A blank port, 0, or a range such as 3000-3005 lets Docker choose.
	for _, bindings := range host.PortBindings {
		for _, binding := range bindings {
			if port, err := strconv.Atoi(binding.HostPort); err != nil || port <= 0 {
				host.PublishAllPorts = true
			}
		}
	}
	if host.PublishAllPorts {
		return own, nil, errPortNotFixed
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

// repository is a reference without its tag or digest.
func repository(reference string) string {
	repository, _, _ := strings.Cut(reference, "@")
	if colon := strings.LastIndex(repository, ":"); colon > strings.LastIndex(repository, "/") {
		return repository[:colon]
	}
	return repository
}

func (s *Service) busy(ctx context.Context, q queryer) (bool, error) {
	var busy bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operations WHERE status IN ('queued','running'))").Scan(&busy)
	return busy, err
}

// Updating reports whether an update holds Ludock's work back.
func (s *Service) Updating() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lift != nil
}

// Start begins updating Ludock for an administrator. Nothing may run, be
// queued, or hold work, and startup recovery must have finished; from here
// until the update settles no work can start.
func (s *Service) Start(ctx context.Context, adminID, tokenHash, ip string) error {
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
	record := Record{ID: identity.NewUUID(), State: StateUpdating, ActorID: adminID, From: s.Version,
		Container: own.ID, Name: strings.TrimPrefix(own.Name, "/"), Previous: own.Image, StartedAt: db.Now(),
		Policy: plan.RestartPolicy()}
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
	s.mu.Lock()
	s.lift = lift
	s.mu.Unlock()
	go s.run(s.background(), record, own, plan, tokenHash)
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

// run pulls the image, copies the data, and hands the update to a helper
// from Ludock's own image. Ludock stopping meanwhile leaves the record for
// the next start to settle.
func (s *Service) run(ctx context.Context, record Record, own *docker.ContainerInspect, plan *update.Plan, tokenHash string) {
	// A copy an earlier update left stays until this one writes its own.
	_, copyErr := os.Lstat(filepath.Join(s.DataDir, CopyName))
	earlier := copyErr == nil
	ended := func(state, reason string) {
		kept := record
		kept.Copy = earlier
		s.settle(ctx, kept, state, reason)
	}
	failed := func(err error) {
		// A Ludock stopping meanwhile leaves the update to the next one.
		if ctx.Err() == nil {
			message, _ := apperr.Public(err)
			ended(StateFailed, message)
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
	// The new container runs what this pull fetched, whatever the tag
	// points to by now.
	// By the image's own ID, not a platform manifest's, which Docker can't
	// create a container from when the image is an index.
	target, err := s.Docker.InspectImage(ctx, repository(plan.Image)+"@"+digest)
	if err != nil {
		failed(errPull)
		return
	}
	if s.Checks != nil {
		s.Checks.ForgetSelf()
	}
	if target == record.Previous {
		ended(StateCurrent, "")
		return
	}
	image, err := s.Docker.InspectImageConfig(ctx, target)
	if err != nil {
		failed(errPull)
		return
	}
	var config struct{ Labels map[string]string }
	json.Unmarshal(encode(image.Config), &config)
	record.To, record.Target = config.Labels["org.opencontainers.image.version"], target
	// Work cancellable when the update began ends; no other can start, so
	// none touches a game server after the copy, which putting the old
	// version back would forget. An older image refuses this data, fails
	// its health check, and is put back.
	quiet, cancel := context.WithTimeout(ctx, time.Minute)
	err = s.Locks.Drain(quiet)
	cancel()
	if err != nil {
		failed(errBusy)
		return
	}
	// The pull can take minutes: the helper gets Docker's socket only for
	// the same administrator still signed in, with no work queued.
	err = s.DB.SelfUpdateMutation(ctx, record.ActorID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		if busy, err := s.busy(ctx, tx); err != nil || busy {
			return cmpErr(err, errBusy)
		}
		return s.DB.SetSetting(ctx, tx, recordSetting, record)
	})
	if errors.Is(err, db.ErrSessionGone) || errors.Is(err, db.ErrNotAdmin) {
		err = errNoLongerAdmin
	}
	if err != nil {
		failed(err)
		return
	}
	if err := s.DB.CopyBeside(ctx, CopyName); err != nil {
		failed(errSave)
		return
	}
	earlier = false
	// The copy can take a while too. The live database's record then says
	// the copy was written, and the copy's own doesn't, so a Ludock put back
	// can tell which one it opened.
	written := record
	written.Copy = true
	if err := s.DB.SelfUpdateMutation(ctx, record.ActorID, tokenHash, db.Now(), func(tx *sql.Tx) error {
		return s.DB.SetSetting(ctx, tx, recordSetting, written)
	}); errors.Is(err, db.ErrSessionGone) || errors.Is(err, db.ErrNotAdmin) {
		failed(errNoLongerAdmin)
		return
	} else if err != nil {
		failed(errSave)
		return
	}
	if err := s.startHelper(ctx, record, own); errors.Is(err, errUncertain) {
		// The helper may be swapping already; watch, or the Ludock that runs
		// next, finds out.
	} else if err != nil {
		log.Warn("Ludock's update helper didn't start, so nothing was changed", logging.Cause(err, docker.StatusCode(err))...)
		failed(errHelper)
		return
	}
	s.watch(ctx, record)
}

// HelperName names the update's helper.
func HelperName(operation string) string { return "ludock-self-update-" + operation }

// startHelper starts the helper from Ludock's own image, with only the
// Docker socket, mounted as Ludock's own is.
func (s *Service) startHelper(ctx context.Context, record Record, own *docker.ContainerInspect) error {
	spec, err := s.Docker.InspectSpec(ctx, own.ID)
	if err != nil {
		return err
	}
	socket := s.socket(spec)
	if socket == nil {
		return errNoSocket
	}
	var config struct{ User string }
	json.Unmarshal(encode(spec.Config), &config)
	var host struct{ GroupAdd, CapAdd []string }
	json.Unmarshal(encode(spec.HostConfig), &host)
	body := map[string]any{"Image": own.Image, "Entrypoint": []string{helper.Binary},
		"Cmd":  []string{"self-update", record.Container, record.Target, record.ID},
		"Env":  []string{"LUDOCK_DOCKER_SOCKET=" + socket.Destination},
		"User": config.User, "Labels": launch.HelperLabels(helperKind), "Healthcheck": map[string]any{"Test": []string{"NONE"}},
		"HostConfig": map[string]any{"NetworkMode": "none", "ReadonlyRootfs": true, "CapDrop": []string{"ALL"},
			"CapAdd": host.CapAdd, "SecurityOpt": []string{"no-new-privileges:true"}, "GroupAdd": host.GroupAdd,
			"Mounts": []map[string]any{{"Type": "bind", "Source": socket.Source, "Target": socket.Destination, "ReadOnly": !socket.RW}}}}
	call, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	id, err := s.Docker.Create(call, HelperName(record.ID), "", body)
	if err != nil {
		return err
	}
	if err := s.Docker.Start(call, id); docker.StatusCode(err) == 0 && err != nil {
		return errUncertain
	} else if err != nil {
		// Docker refused it, so it never ran.
		s.Docker.Remove(context.WithoutCancel(call), id, true, true)
		return err
	}
	return nil
}

// mounted reports whether the deepest mount holding dataDir is a volume or
// a mounted folder.
func mounted(own *docker.ContainerInspect, dataDir string) bool {
	var found *docker.MountPoint
	for index, mount := range own.Mounts {
		if helper.Within(dataDir, mount.Destination) && (found == nil || len(filepath.Clean(mount.Destination)) > len(filepath.Clean(found.Destination))) {
			found = &own.Mounts[index]
		}
	}
	return found != nil && (found.Type == "bind" || found.Type == "volume")
}

// operationOf is the update a container was created for, from its label.
func operationOf(spec *docker.ContainerSpec) string {
	var config struct{ Labels map[string]string }
	json.Unmarshal(encode(spec.Config), &config)
	return config.Labels[update.OperationLabel]
}

// errUncertain: Docker didn't answer whether it started the helper.
var errUncertain = errors.New("docker didn't answer whether it started the helper")

// watch waits for the helper to end, then records how the update did. The
// helper may stop this Ludock meanwhile; the one that runs next settles it.
func (s *Service) watch(ctx context.Context, record Record) {
	for !s.judge(ctx, record) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.poll()):
		}
	}
}

// judge records how the update ended, by the helper's exit code and what
// Docker shows, in this order; it reports false while it can't yet tell.
//
//	observed                                          concluded
//	helper running, or Docker can't say (< 30 min)    not yet
//	helper never started and Docker won't remove it   not yet
//	(< 30 min)
//	this Ludock's container unreadable                not yet; interrupted after 30 min
//	this Ludock is neither original nor new container interrupted: the owner started it; copy goes
//	original, helper exited "put back"                put back: data restored (now, or by an
//	                                                  earlier start: the record lacks Copy),
//	                                                  not restored (copy still there), or kept
//	original, helper exited "unchanged", none left    failed, nothing changed; copy goes
//	new container under Ludock's name, original gone  succeeded; copy goes
//	original never stopped (started before the        failed, stopped before Ludock was
//	update), or no helper and no new container,       replaced; copy goes
//	and none left
//	anything else                                     interrupted: finish from a terminal
//
// "None left" means Docker showed no new container of this update, after
// removing one that never ran. An exited helper is removed only once the
// outcome is saved, so a restart in between finds the same evidence; the
// next start removes one Docker refused to remove then.
func (s *Service) judge(ctx context.Context, record Record) bool {
	// A helper still running, or Docker unable to say, long after any swap
	// would have ended is given up on: the owner finishes from a terminal.
	stuck := time.Since(time.UnixMilli(record.StartedAt)) > 30*time.Minute
	// Saved, Copy says this Ludock opened the live database, not the copy.
	live := record.Copy
	// The copy stays whenever how it ended is uncertain.
	_, copyErr := os.Lstat(filepath.Join(s.DataDir, CopyName))
	record.Copy = copyErr == nil
	// Each change has its own deadline, so the one above holds.
	call := func() context.Context {
		bounded, cancel := context.WithTimeout(ctx, time.Minute)
		context.AfterFunc(bounded, cancel)
		return bounded
	}
	spec, err := s.Docker.InspectSpec(ctx, HelperName(record.ID))
	noHelper := docker.IsNotFound(err)
	if !stuck && (err != nil && !docker.IsNotFound(err) || err == nil && (spec.State.Running || spec.State.Restarting)) {
		return false
	}
	code, exited := -1, err == nil && spec.State.Status == "exited"
	if exited {
		code = spec.State.ExitCode
	} else if err == nil {
		// One still created may be starting as a lost start answer lands:
		// unforced, Docker refuses to remove it once it runs.
		if err := s.Docker.Remove(call(), spec.ID, spec.State.Status != "created", true); err != nil && !docker.IsNotFound(err) && !stuck {
			return false
		}
	}
	// An exited helper's code is the evidence, so it goes only once the
	// outcome is saved: a restart in between finds it again.
	settle := func(state, reason string) bool {
		if !s.settle(ctx, record, state, reason) {
			return false
		}
		// A Ludock stopping may not have saved it.
		if exited && ctx.Err() == nil {
			s.Docker.Remove(call(), spec.ID, false, true)
		}
		return true
	}
	own, err := s.current(ctx)
	if err != nil {
		if stuck {
			return settle(StateInterrupted, reasonInterrupted)
		}
		return false
	}
	first, gone := s.Docker.InspectSpec(ctx, record.Container)
	original, ownReplacement := own.ID == record.Container, own.Config.Labels[update.OperationLabel] == record.ID
	putBack := putBackReason(code)
	verdict := code == ExitUpdated || code == ExitUnchanged || putBack >= 0
	// An original running again with no word from the helper puts the
	// update back and stops this one first, so it waits.
	if ownReplacement && !verdict && !stuck && gone == nil && first.State.Running {
		return false
	}
	// One Docker started again after its original went, before its rename,
	// waits for its health check to settle, as the helper would have.
	if ownReplacement && own.Name != "/"+record.Name && docker.IsNotFound(gone) && own.State.Health != nil &&
		own.State.Health.Status == "starting" && !stuck {
		return false
	}
	// One under its temporary name takes Ludock's name once Docker shows its
	// original gone: the helper removes that only after the new one passed.
	renamed := ownReplacement && own.Name != "/"+record.Name && docker.IsNotFound(gone) && own.State.Health != nil &&
		own.State.Health.Status == "healthy" && s.Docker.Rename(call(), own.ID, record.Name) == nil
	if renamed {
		own.Name = "/" + record.Name
	}
	// Only a container of this update, by its label, is left over.
	replacement, err := s.Docker.InspectSpec(ctx, servers.ReplacementName(record.Name, record.ID))
	// Nothing changed only once Docker showed whether one is left.
	known, noReplacement := err == nil || docker.IsNotFound(err), docker.IsNotFound(err)
	if err == nil && replacement.ID != own.ID && operationOf(replacement) == record.ID {
		record.Leftover = strings.TrimPrefix(replacement.Name, "/")
	}
	// This Ludock may be that container, not yet under Ludock's name; its
	// removal is advised only with the original shown to exist, since it
	// may be the last container naming an anonymous data volume.
	if ownReplacement && own.Name != "/"+record.Name {
		record.Leftover, record.OnLeftover, record.OriginalGone = strings.TrimPrefix(own.Name, "/"), true, docker.IsNotFound(gone)
	}
	// An original running since before the update was never stopped, nor
	// was one restarted before the update had a helper or a new container,
	// so its data is as it was; a new container the helper left unstarted
	// goes.
	started, _ := time.Parse(time.RFC3339Nano, own.State.StartedAt)
	untouched := original && (started.Before(time.UnixMilli(record.StartedAt)) || noHelper && noReplacement)
	// An original started again with no word from the helper, as after a
	// reboot, put the update back as it started: it stopped the new one.
	cutOff := original && !verdict && !untouched
	// So does one after the helper said nothing changed, or that was cut
	// off: it never ran, or no longer does.
	if (untouched || original && code == ExitUnchanged || cutOff) && record.Leftover != "" && !replacement.State.Running {
		if err := s.Docker.Remove(call(), replacement.ID, true, true); err == nil || docker.IsNotFound(err) {
			record.Leftover = ""
		}
	}
	log.Info("Ludock's update helper ended", logging.F("exitCode", code))
	succeeded := ownReplacement && own.Name == "/"+record.Name && docker.IsNotFound(gone)
	// At most one of the two containers has a restart policy, so a reboot
	// starts one Ludock. Whichever settles an update the helper didn't
	// finish takes the one Ludock had as it began, the owner's even if
	// "no", once the other has none; if that fails it takes none.
	// While Docker can't say whether the other is there, it takes none; the
	// owner's "no" it takes at once, as that enables nothing.
	if (original || ownReplacement && (!succeeded || renamed)) && len(record.Policy) > 0 {
		var policy struct{ Name string }
		json.Unmarshal(record.Policy, &policy)
		other := ""
		if original && record.Leftover != "" {
			other = record.Leftover
		} else if ownReplacement && gone == nil {
			other = record.Container
		}
		unknown := ownReplacement && gone != nil && !docker.IsNotFound(gone) || original && !known
		if policy.Name != "no" && (unknown || other != "" && s.Docker.SetRestartPolicy(call(), other, noRestart) != nil) ||
			s.Docker.SetRestartPolicy(call(), own.ID, record.Policy) != nil {
			record.NoPolicy = true
		}
	}
	// How a put-back left the data.
	data := func() {
		switch {
		case s.PutBack == DataRestored:
			record.Data = DataRestored
		case s.MountID == "":
			// A development Ludock runs outside the container it stands
			// for, so its data never left it.
			record.Data, record.Copy = DataRestored, false
		case record.Copy:
			// It is there, but this Ludock couldn't put it back.
			record.Data = DataNotRestored
		case !live:
			// An earlier start of this Ludock put the copy back, and this
			// one opened it.
			record.Data = DataRestored
		default:
			record.Data = DataKept
		}
	}
	switch {
	case !original && !ownReplacement:
		// The owner started Ludock from a terminal, on the data as it is.
		record.Copy = false
		return settle(StateInterrupted, reasonInterrupted)
	case original && putBack >= 0:
		data()
		return settle(StatePutBack, Reasons[putBack])
	case cutOff:
		data()
		return settle(StatePutBack, reasonCutOff)
	case original && code == ExitUnchanged && known && record.Leftover == "":
		record.Copy = false
		return settle(StateFailed, reasonUnchanged)
	case succeeded:
		record.To, record.Copy = s.Version, false
		// Its name, which Ludock may find itself by, is the new container's.
		s.Own.Forget()
		return settle(StateSucceeded, "")
	case untouched && known && record.Leftover == "":
		record.Copy = false
		return settle(StateFailed, reasonNotReplaced)
	}
	return settle(StateInterrupted, reasonInterrupted)
}

// settle records the outcome until it is recorded or Ludock stops, then
// lets work start again. The data copy stays only for an update that
// didn't finish.
func (s *Service) settle(ctx context.Context, record Record, state, reason string) bool {
	for {
		settled, err := s.record(ctx, record, state, reason)
		if err == nil {
			if !record.Copy {
				os.Remove(filepath.Join(s.DataDir, CopyName))
			}
			s.release()
			if settled {
				s.announce(record, state, reason)
			}
			return true
		}
		select {
		case <-ctx.Done():
			return true
		case <-time.After(s.poll()):
		}
	}
}

// record saves how an update ended, with its audit entry, unless it is no
// longer the update in progress: whoever settled it first recorded it.
func (s *Service) record(ctx context.Context, record Record, state, reason string) (settled bool, err error) {
	ctx = context.WithoutCancel(ctx)
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

// announce alerts about an update that didn't succeed.
func (s *Service) announce(record Record, state, reason string) {
	if (state == StatePutBack || state == StateFailed || state == StateInterrupted) && s.Notify != nil {
		s.Notify(notify.Results, "ludock-update:"+record.ID, "Ludock couldn’t update itself: "+strings.TrimSuffix(reason, ".")+
			". Needs attention says what happened and what to do.")
	}
}

func (s *Service) release() {
	s.mu.Lock()
	lift := s.lift
	s.lift = nil
	s.mu.Unlock()
	if lift != nil {
		lift()
	}
}

// Recover holds Ludock's work back, when an update is in progress as it
// starts, until the update's helper has ended and the update is recorded.
func (s *Service) Recover(ctx, base context.Context) {
	s.mu.Lock()
	s.base = base
	s.mu.Unlock()
	record := s.Last(ctx)
	if !record.InProgress() {
		// One whose removal failed after its update was recorded goes now.
		if record != nil && record.ID != "" {
			go func() {
				if spec, err := s.Docker.InspectSpec(base, HelperName(record.ID)); err == nil && spec.State.Status == "exited" {
					s.Docker.Remove(base, spec.ID, false, true)
				}
			}()
		}
		return
	}
	lift := s.Locks.Hold(errUpdating)
	s.mu.Lock()
	s.lift = lift
	s.mu.Unlock()
	go s.watch(base, *record)
}

func cmpErr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
