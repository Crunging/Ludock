// Package selfupdate replaces Ludock's own container with one running the
// newer image its tag points to. Ludock can't replace itself from inside,
// since the process doing it dies with its container: Ludock pulls the
// image, records the update, saves its data, and starts a helper from the
// new image, which does the swap and removes itself.
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
)

// Version is the interface between Ludock versions, which 1.0 froze: the
// Ludock that starts an update is older than the helper that does it, and
// either older or newer than the Ludock that comes up after it. A helper
// accepts every version an older Ludock may pass, and keeps everything below
// as it is; testdata/1.0 holds what a 1.0 Ludock writes.
//
// The older Ludock starts the helper from the image it pulled, by its ID,
// with entrypoint /usr/local/bin/ludock, the Docker socket mounted at the
// path in LUDOCK_DOCKER_SOCKET and nothing else, restart policy
// unless-stopped, and these arguments:
//
//	self-update 1 <container> <image> <operation>
//
// The arguments are:
//   - container: the full ID of Ludock's own container, the one replaced.
//   - image: the pulled image's ID (sha256:…), which the helper runs from
//     and the replacement must run.
//   - operation: the update's UUID, which names everything below.
//
// Before starting the helper, the older Ludock saved its data beside its
// database as SaveName, an archive `ludock restore-data` reads, and recorded
// the update in the ludock.update setting (Record) as handed_over. It ends
// a helper that hasn't created its state marker within two minutes.
//
// Every container the update creates carries ludock.enable=false, the
// ludock.internal kind below, and ludock.update.operation; each is named
// after HelperName and, but the helper, never started:
//   - HelperName, kind self-update: the helper.
//   - -state, kind self-update-state: the original's name (ludock.update.name)
//     and restart policy (ludock.update.policy), written before anything
//     changes.
//   - -healthy, kind self-update-healthy: the replacement that passed its
//     check (ludock.update.container), written before the original is
//     removed.
//   - -undo, kind self-update-undo: the decision to put the original back,
//     with a Reason code (ReasonLabel) and whether its data is restored
//     (ludock.update.restore).
//   - -ready-<container ID>, kind self-update-ready: written
//     by a Ludock that opened its data and settles this update, naming its
//     container (ludock.update.container) and that container's start time
//     (ludock.update.started). The helper leaves only once the Ludock
//     running under the original's name has written one for its run.
//   - -data, kind self-update-data: a data step, run from the new image
//     (self-update-data) or the original's (restore-data).
//
// Putting the original back with its data restores the save through the
// original's own `ludock restore-data <save>`, whose exit code says how it
// went: 0 restored, 3 the save can't be restored by that version (damaged,
// from a newer version, or from a beta), 4 the data folder already holds
// data, anything else an error worth trying again. The aside folder's
// settled file then holds "restored" or "kept" (the save couldn't be
// restored, so the newer data is back in place).
//
// The Ludock that comes up removes these containers and the aside folder
// once it has recorded the outcome.
const Version = "1"

// Reason codes a helper leaves when it puts the previous container back.
// Codes are never reused; a Ludock reports an unknown one generically.
const (
	ReasonImage     = "image"
	ReasonCreate    = "create"
	ReasonStart     = "start"
	ReasonStopped   = "stopped"
	ReasonUnhealthy = "unhealthy"
	ReasonTimeout   = "timeout"
	ReasonNoHealth  = "nohealth"
)

// Exit codes of `ludock restore-data`.
const (
	RestoreFinal  = 3
	RestoreExists = 4
)

const (
	ReasonLabel    = "ludock.update.reason"
	restoreLabel   = "ludock.update.restore"
	nameLabel      = "ludock.update.name"
	policyLabel    = "ludock.update.policy"
	containerLabel = "ludock.update.container"
	startedLabel   = "ludock.update.started"
	operationLabel = "ludock.update.operation"
)

// Kinds of InternalLabel this package's containers carry.
const (
	helperKind  = "self-update"
	stateKind   = "self-update-state"
	healthyKind = "self-update-healthy"
	undoKind    = "self-update-undo"
	readyKind   = "self-update-ready"
	dataKind    = "self-update-data"
)

// HelperName is the helper's container name for an update.
func HelperName(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return "ludock-self-update-" + hex.EncodeToString(sum[:8])
}

func stateName(operation string) string   { return HelperName(operation) + "-state" }
func healthyName(operation string) string { return HelperName(operation) + "-healthy" }
func undoName(operation string) string    { return HelperName(operation) + "-undo" }
func dataName(operation string) string    { return HelperName(operation) + "-data" }

func readyName(operation, container string) string {
	return HelperName(operation) + "-ready-" + container
}

// SaveName is the data save an update writes beside the database.
func SaveName(operation string) string { return "ludock-update-" + operation + ".tar" }

// asideName is the folder beside the database where putting the previous
// version back moves the newer version's data, and records that it did.
func asideName(operation string) string { return ".ludock-update-" + operation }
