// Package files exposes a server's data directories. Paths stay inside
// approved roots; host system paths are never offered.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/textutil"
)

type Root struct {
	ID   string
	Name string
	Path string
}

func normalizedMountPath(value string) string {
	normalized := strings.TrimSuffix(path.Clean(value), "/")
	if normalized == "" {
		return "/"
	}
	return normalized
}

// Within reports whether value is root or inside it.
func Within(value, root string) bool {
	return value == root || strings.HasPrefix(value, root+"/")
}

var socketSuffix = regexp.MustCompile(`(?i)\.sock$`)

// SafeWritableDataMount reports a writable bind or volume that is not a
// system path or socket.
func SafeWritableDataMount(mount identity.Mount) bool {
	if !mount.Writable || (mount.Type != "bind" && mount.Type != "volume") {
		return false
	}
	destination := normalizedMountPath(mount.Destination)
	if !strings.HasPrefix(destination, "/") || textutil.Length(destination) > 512 ||
		strings.Contains(mount.Destination, "\x00") || SensitiveSystemPath(destination) || socketSuffix.MatchString(destination) {
		return false
	}
	if mount.Type == "bind" {
		source := path.Clean(mount.Source)
		if !strings.HasPrefix(source, "/") || strings.Contains(mount.Source, "\x00") ||
			SensitiveSystemPath(source) || socketSuffix.MatchString(source) {
			return false
		}
	}
	return !sharesOwnMount(mount)
}

var ownMounts atomic.Pointer[[]identity.Mount]

// SetOwnMounts records the mounts of Ludock's own container. Its data and
// backup folders hold password hashes and keys, so no game folder that
// overlaps them is offered.
func SetOwnMounts(mounts []identity.Mount) {
	mounts = slices.Clone(mounts)
	ownMounts.Store(&mounts)
}

func sharesOwnMount(mount identity.Mount) bool {
	own := ownMounts.Load()
	if own == nil {
		return false
	}
	source := path.Clean(mount.Source)
	for _, other := range *own {
		switch {
		case mount.Type == "volume" && other.Type == "volume" && mount.Name != "" && mount.Name == other.Name:
			return true
		case mount.Type == "bind" && other.Type == "bind" && strings.HasPrefix(other.Source, "/"):
			if otherSource := path.Clean(other.Source); Within(source, otherSource) || Within(otherSource, source) {
				return true
			}
		}
	}
	return false
}

var protectedPaths = []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/var/lib/docker", "/var/lib/containerd",
	"/etc", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/usr", "/root/.ssh", "/root/.aws", "/root/.docker"}

var broadPaths = []string{"/", "/home", "/root", "/srv", "/opt", "/mnt", "/media", "/Users"}

var extraSensitive atomic.Pointer[[]string]

// SetSensitivePaths adds configured protected paths (absolute, cleaned).
// Startup sets them once.
func SetSensitivePaths(paths []string) {
	paths = slices.Clone(paths)
	extraSensitive.Store(&paths)
}

// SensitiveSystemPath reports a host or container system path, a broad
// parent directory, or a configured sensitive path.
func SensitiveSystemPath(value string) bool {
	roots := slices.Clone(protectedPaths)
	if extra := extraSensitive.Load(); extra != nil {
		roots = append(roots, *extra...)
	}
	if slices.Contains(broadPaths, value) {
		return true
	}
	for _, root := range roots {
		if Within(value, root) || Within(root, value) {
			return true
		}
	}
	return false
}

// Roots lists up to eight browsable data roots. ludock.files may name them;
// otherwise every safe writable mount is a root, except the game's own
// server files. The deepest actual mount
// owns each path, so a label cannot bypass a read-only or sensitive nested
// mount by naming its parent.
func Roots(gameType string, labels map[string]string, mounts []identity.Mount) []Root {
	var candidates []string
	if configured, ok := labels[games.LabelFiles]; ok {
		for _, value := range strings.Split(configured, ",") {
			candidates = append(candidates, strings.TrimSpace(value))
		}
	} else {
		var serverFiles, saves []string
		if integration := games.Lookup(gameType); integration != nil {
			serverFiles, saves = integration.ServerFiles, integration.Saves
		}
		for _, mount := range mounts {
			destination := normalizedMountPath(mount.Destination)
			if !SafeWritableDataMount(mount) {
				continue
			}
			if !slices.Contains(serverFiles, destination) {
				candidates = append(candidates, mount.Destination)
				continue
			}
			for _, save := range saves {
				if Within(save, destination) {
					candidates = append(candidates, save)
				}
			}
		}
	}
	var paths []string
	for _, value := range candidates {
		if !strings.HasPrefix(value, "/") || strings.Contains(value, "\x00") || slices.Contains(strings.Split(value, "/"), "..") {
			continue
		}
		normalized := strings.TrimSuffix(path.Clean(value), "/")
		if normalized == "" || textutil.Length(normalized) > 512 || slices.Contains(paths, normalized) || SensitiveSystemPath(normalized) {
			continue
		}
		if owner, ok := RootMount(normalized, mounts); !ok || !SafeWritableDataMount(owner) {
			continue
		}
		paths = append(paths, normalized)
	}
	var roots []Root
	for _, candidate := range paths {
		nested := false
		for _, other := range paths {
			if other != candidate && Within(candidate, other) {
				nested = true
			}
		}
		if nested || len(roots) >= 8 {
			continue
		}
		roots = append(roots, Root{ID: RootID(candidate), Name: rootName(gameType, candidate), Path: candidate})
	}
	return roots
}

// RootMount is the deepest mount that holds rootPath.
func RootMount(rootPath string, mounts []identity.Mount) (identity.Mount, bool) {
	var owners []identity.Mount
	for _, mount := range mounts {
		if Within(rootPath, normalizedMountPath(mount.Destination)) {
			owners = append(owners, mount)
		}
	}
	sort.SliceStable(owners, func(i, j int) bool {
		return textutil.Length(owners[i].Destination) > textutil.Length(owners[j].Destination)
	})
	if len(owners) == 0 {
		return identity.Mount{}, false
	}
	return owners[0], true
}

// RootID derives a root's ID from its container path, so an ID kept by a
// page or a backup can only ever name the folder it was given for.
func RootID(rootPath string) string {
	sum := sha256.Sum256([]byte(rootPath))
	return "root-" + hex.EncodeToString(sum[:6])
}

func rootName(gameType, rootPath string) string {
	switch {
	case strings.EqualFold(gameType, "minecraft") && rootPath == "/data":
		return "Minecraft data"
	case strings.EqualFold(gameType, "valheim") && rootPath == "/config":
		return "Valheim config"
	case strings.EqualFold(gameType, "terraria") && rootPath == "/root/.local/share/Terraria/Worlds":
		return "Terraria worlds"
	case strings.EqualFold(gameType, "palworld") && rootPath == "/palworld/Pal/Saved":
		return "Palworld saves"
	}
	if base := path.Base(rootPath); base != "/" && base != "." {
		return base
	}
	return rootPath
}
