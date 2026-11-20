package backups

import (
	"slices"
	"strings"

	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/servers"
)

// Fit is whether a backup can be restored onto its server as it is now.
type Fit struct {
	// Moved: the same game and data folders inside the server, but kept
	// elsewhere on the host (another volume or bind source) than when the
	// backup was taken. Restoring it needs the administrator to confirm that.
	Moved bool
	// Refusal says in plain words why it can't be restored; empty when it can.
	Refusal string
}

// FitFor compares a backup with the server as Docker shows it now.
func FitFor(backup *Backup, server *servers.Context) Fit {
	return CompareFit(backup, server.Logical.BindingFingerprint, server.Container.GameType, CurrentRoots(server.Container.FileRoots))
}

// CompareFit compares a backup with a server's binding fingerprint, game, and
// data folders. A restore writes each recorded folder's files to that same
// path inside the server, so the game and those paths are all that must
// agree; what holds them on the host may differ.
func CompareFit(backup *Backup, fingerprint, gameType string, roots []Root) Fit {
	same := rootsEqual(backup.Roots, roots)
	if same && fingerprint != "" && backup.fingerprint == fingerprint {
		return Fit{}
	}
	// Unrecognized images are all "unknown", which can't show the game is
	// the same.
	if backup.GameType == "" || backup.GameType == "unknown" || gameType == "unknown" {
		return Fit{Refusal: "Ludock can’t tell which game this backup holds, so it restores only onto the data folders it was taken from."}
	}
	if !strings.EqualFold(backup.GameType, gameType) {
		return Fit{Refusal: "This backup holds " + games.DisplayName(backup.GameType) + " data, but the server now runs " +
			games.DisplayName(gameType) + "."}
	}
	if !same {
		var reasons []string
		if missing := rootPaths(backup.Roots, roots); len(missing) > 0 {
			reasons = append(reasons, "This backup has "+folders(missing)+", which the server no longer has.")
		}
		if extra := rootPaths(roots, backup.Roots); len(extra) > 0 {
			reasons = append(reasons, "The server now has "+folders(extra)+", which this backup doesn’t.")
		}
		if len(reasons) == 0 {
			reasons = append(reasons, "This backup’s data folders don’t match the server’s.")
		}
		return Fit{Refusal: strings.Join(reasons, " ")}
	}
	return Fit{Moved: true}
}

// CurrentRoots are a server's data folders as a backup records them.
func CurrentRoots(fileRoots []files.Root) []Root {
	var roots []Root
	for _, root := range fileRoots {
		roots = append(roots, Root{ID: root.ID, Path: root.Path})
	}
	return roots
}

// rootsEqual ignores order: a root's ID is derived from its path, and Docker
// or the ludock.files label may list the same roots in a new order.
func rootsEqual(left, right []Root) bool {
	return len(left) == len(right) && len(rootPaths(left, right)) == 0 && len(rootPaths(right, left)) == 0
}

// rootPaths lists the paths of roots that others lacks.
func rootPaths(roots, others []Root) []string {
	var paths []string
	for _, root := range roots {
		if !slices.Contains(others, root) {
			paths = append(paths, root.Path)
		}
	}
	return paths
}

func folders(paths []string) string {
	if len(paths) == 1 {
		return "the folder " + paths[0]
	}
	return "the folders " + strings.Join(paths[:len(paths)-1], ", ") + " and " + paths[len(paths)-1]
}
