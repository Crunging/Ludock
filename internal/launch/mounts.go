package launch

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
)

var ErrUnverifiedMount = apperr.New("UNVERIFIED_DATA_MOUNT", 409,
	"A data mount could not be verified. Bind source paths must contain no symbolic links and must be accessible to the Docker host validator; named volumes must use the local driver without host remapping options.")

type Mount struct {
	Type        string
	Source      string
	Destination string
	Name        string
}

// Proof keeps a validator alive. Its pinned descriptors prevent source
// inode reuse until Cleanup.
type Proof struct {
	Identities map[string]helper.MountIdentity
	Cleanup    func()
}

// HelperLimits are the resource limits of helper containers. Go runtimes
// start several threads, so the PID limit leaves room for them.
const (
	helperPids    = 64
	helperCPUNano = 1_000_000_000
)

// HelperEnv keeps helper Go runtimes small.
var HelperEnv = []string{"GOMAXPROCS=1", "GOMEMLIMIT=96MiB"}

var digits = regexp.MustCompile(`^\d+$`)

func trimSlash(value string) string { return strings.TrimSuffix(path.Clean(value), "/") }

// MountProof verifies volume drivers and pins bind sources through a private
// validator that sees only directory device and inode numbers. It never runs
// user commands, reads game files, or exposes its host mount to a file
// endpoint. operationID, when set, labels the validator for recovery and
// makes cleanup retry until it is gone.
func MountProof(ctx context.Context, client *docker.Client, images *Images, mounts []Mount, operationID string) (*Proof, error) {
	for _, mount := range mounts {
		if mount.Type != "volume" {
			continue
		}
		name := mount.Name
		if name == "" {
			name = mount.Source
		}
		if name == "" || strings.Contains(name, "/") {
			return nil, ErrUnverifiedMount
		}
		info, err := client.InspectVolume(ctx, name)
		if err != nil || info.Driver != "local" || len(info.Options) > 0 {
			return nil, ErrUnverifiedMount
		}
	}
	var sources []helper.MountSource
	for _, mount := range mounts {
		if mount.Type == "bind" {
			sources = append(sources, helper.MountSource{Source: trimSlash(mount.Source), Destination: trimSlash(mount.Destination)})
		}
	}
	if len(sources) == 0 {
		return &Proof{Identities: map[string]helper.MountIdentity{}, Cleanup: func() {}}, nil
	}
	image, err := images.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	timeout := strconv.Itoa(35 * 60 * 1000)
	labels := map[string]string{"ludock.enable": "false", "ludock.internal": "mount-validator"}
	if operationID != "" {
		timeout = "0"
		labels["ludock.operation"] = operationID
	}
	// The create is bounded on its own: a backup or restore may already have
	// stopped the game and hold its locks. The name finds a validator Docker
	// created without answering.
	name := "ludock-validator-" + randomSuffix()
	creating, cancel := context.WithTimeout(context.WithoutCancel(ctx), createLimit)
	id, err := client.CreateHelper(creating, docker.ContainerCreate{Name: name,
		Image: image, Entrypoint: []string{helper.Binary}, Cmd: helper.Command("mount-proof", sources, timeout)[1:],
		User: "0", Labels: labels, Env: HelperEnv,
		HostConfig: &docker.HostConfig{
			Mounts:      []docker.MountSettings{{Type: "bind", Source: "/", Target: helper.HostRoot, ReadOnly: true, BindOptions: &docker.BindOptions{ReadOnlyForceRecursive: true}}},
			NetworkMode: "none", ReadonlyRootfs: true, AutoRemove: true, CapDrop: []string{"ALL"},
			CapAdd: []string{"DAC_READ_SEARCH"}, SecurityOpt: []string{"no-new-privileges"},
			PidsLimit: helperPids, Memory: 128 * 1024 * 1024, NanoCpus: helperCPUNano,
		},
	})
	cancel()
	if err != nil {
		// Validators only read, so removal need not hold the caller.
		removing, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		client.RemoveHelper(removing, name)
		done()
		return nil, ErrUnverifiedMount
	}
	cleanup := func() {
		if operationID != "" {
			client.RemoveOperationHelper(id)
		} else {
			client.RemoveHelper(context.WithoutCancel(ctx), id)
		}
	}
	identities, err := readProof(ctx, client, id, sources)
	if err != nil {
		cleanup()
		return nil, ErrUnverifiedMount
	}
	return &Proof{Identities: identities, Cleanup: cleanup}, nil
}

func readProof(ctx context.Context, client *docker.Client, id string, sources []helper.MountSource) (map[string]helper.MountIdentity, error) {
	if err := client.Start(ctx, id); err != nil {
		return nil, err
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	logs, err := client.Logs(deadline, id, docker.LogsOptions{Follow: true, Stdout: true, Tail: 10})
	if err != nil {
		return nil, err
	}
	defer logs.Close()
	stdout := docker.Stdout(logs)
	defer stdout.Close()
	reader := bufio.NewReaderSize(stdout, 16384)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, ErrUnverifiedMount
	}
	var result struct {
		Identities map[string]helper.MountIdentity `json:"identities"`
	}
	if json.Unmarshal(line, &result) != nil || result.Identities == nil {
		return nil, ErrUnverifiedMount
	}
	for _, source := range sources {
		identity, ok := result.Identities[source.Destination]
		if !ok || !digits.MatchString(identity.Dev) || !digits.MatchString(identity.Ino) {
			return nil, ErrUnverifiedMount
		}
	}
	return result.Identities, nil
}

// AssertMountIdentities checks, inside a data helper, that each projected
// mount is the directory the validator pinned. Keep the proof alive until the
// data helper is removed.
func AssertMountIdentities(ctx context.Context, client *docker.Client, container string, identities map[string]helper.MountIdentity) error {
	if len(identities) == 0 {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := Exec(deadline, client, container, ExecOptions{Cmd: helper.Command("mount-identities", identities)}); err != nil {
		return ErrUnverifiedMount
	}
	return nil
}

// createLimit bounds a validator's create; tests shorten it.
var createLimit = 2 * time.Minute

func randomSuffix() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
