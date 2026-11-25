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

// Version is the helper interface, which 1.0 froze. An older Ludock starts
// the helper from the image it just pulled, with only the Docker socket
// mounted, and these arguments after the image's entrypoint
// (/usr/local/bin/ludock):
//
//	self-update 1 <container> <image> <operation>
//
// The arguments are:
//   - container: the full ID of Ludock's own container, the one replaced.
//   - image: the pulled image's ID (sha256:…), which the helper runs from
//     and the replacement must run.
//   - operation: the update's UUID. HelperName derives the helper's name
//     from it, and the older Ludock saved its data beside its database as
//     SaveName before starting the helper.
//
// Until a Ludock that comes up removes it, a helper that put the previous
// container back leaves a never-started container named HelperName+"-undo"
// labelled ReasonLabel with one of the Reason codes, which that Ludock
// reports. A helper accepts every version an older Ludock may pass.
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

const (
	ReasonLabel = "ludock.update.reason"
	// restoreLabel marks an undo that must restore the data save, since the
	// previous container was stopped and the new version may have changed
	// the data.
	restoreLabel = "ludock.update.restore"
	nameLabel    = "ludock.update.name"
	policyLabel  = "ludock.update.policy"
	// operationLabel names the update on the helper and its markers.
	operationLabel = "ludock.update.operation"
)

// Kinds of InternalLabel this package's containers carry.
const (
	helperKind = "self-update"
	markerKind = "self-update-marker"
	dataKind   = "self-update-data"
)

// HelperName is the helper's container name for an update.
func HelperName(operation string) string {
	sum := sha256.Sum256([]byte(operation))
	return "ludock-self-update-" + hex.EncodeToString(sum[:8])
}

func stateName(operation string) string { return HelperName(operation) + "-state" }
func undoName(operation string) string  { return HelperName(operation) + "-undo" }
func dataName(operation string) string  { return HelperName(operation) + "-data" }

// SaveName is the data save an update writes beside the database.
func SaveName(operation string) string { return "ludock-update-" + operation + ".tar" }

// asideName is the folder beside the database where putting the previous
// version back moves the newer version's data, and records that it did.
func asideName(operation string) string { return ".ludock-update-" + operation }
