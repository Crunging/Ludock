package helper

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// MountSource is a bind source to prove, keyed by its helper destination.
type MountSource struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// MountIdentity is a directory's device and inode, as decimal strings.
type MountIdentity struct {
	Dev string `json:"dev"`
	Ino string `json:"ino"`
}

// HostRoot is where the mount validator sees the host's root directory.
var HostRoot = "/host"

// RunMountProof pins every bind source below HostRoot without following
// symbolic links, prints their identities, and keeps the descriptors open so
// the inodes cannot be reused until the validator is removed. It never reads
// file contents. timeoutMs 0 waits until killed.
func RunMountProof(argument, timeoutMs string, stdout io.Writer) int {
	var sources []MountSource
	var t tracker
	fail := func() int {
		io.WriteString(stdout, `{"error":"unsafe-source"}`+"\n")
		t.closeAll()
		return 1
	}
	if json.Unmarshal([]byte(argument), &sources) != nil {
		return fail()
	}
	identities := map[string]MountIdentity{}
	for _, source := range sources {
		if !strings.HasPrefix(source.Source, "/") || strings.Contains(source.Source, "\x00") {
			return fail()
		}
		parts, err := components(source.Source)
		if err != nil {
			return fail()
		}
		directory, err := t.walk(HostRoot, parts)
		if err != nil {
			return fail()
		}
		info, err := fstat(directory)
		if err != nil {
			return fail()
		}
		identities[source.Destination] = MountIdentity{strconv.FormatUint(info.Dev, 10), strconv.FormatUint(info.Ino, 10)}
	}
	encoded, _ := json.Marshal(map[string]any{"identities": identities})
	stdout.Write(append(encoded, '\n'))
	if ms, err := strconv.Atoi(timeoutMs); err == nil && ms > 0 {
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return 0
	}
	for {
		time.Sleep(time.Hour)
	}
}

// RunMountIdentities checks that each helper mount is the directory the
// validator pinned. It exits 1 on any mismatch.
func RunMountIdentities(argument string) int {
	var expected map[string]MountIdentity
	if json.Unmarshal([]byte(argument), &expected) != nil {
		return 1
	}
	var t tracker
	defer t.closeAll()
	for name, identity := range expected {
		parts, err := components(name)
		if err != nil {
			return 1
		}
		directory, err := t.walk("/", parts)
		if err != nil {
			return 1
		}
		info, err := fstat(directory)
		t.close(directory)
		if err != nil || strconv.FormatUint(info.Dev, 10) != identity.Dev || strconv.FormatUint(info.Ino, 10) != identity.Ino {
			return 1
		}
	}
	return 0
}

// RunIdle keeps a helper container alive for its execs, then exits.
func RunIdle(timeoutMs string) int {
	ms, err := strconv.Atoi(timeoutMs)
	if err != nil || ms <= 0 {
		ms = 35 * 60 * 1000
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	return 0
}
