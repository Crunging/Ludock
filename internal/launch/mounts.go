package launch

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
)

var ErrUnverifiedMount = apperr.New("UNVERIFIED_DATA_MOUNT", 409,
	"Ludock couldn’t check one of the server’s data folders, so it went no further. Each must be a real folder rather than a link to one, and a Docker volume must be a plain local one.")

// ErrHelperFailed is Docker answering with an error while Ludock creates,
// starts or reads one of its helpers.
var ErrHelperFailed = apperr.New("DOCKER_HELPER_FAILED", 502,
	"Docker reported an error when Ludock started its helper for the server’s files, so Ludock went no further. Try again in a moment; if it keeps happening, check Docker.")

// HelperFailed reports a Docker call made for a helper: a public error stays
// as it is, a request Docker never answered stays a Docker error so callers
// can wait it out, and any answer refusing it becomes ErrHelperFailed.
func HelperFailed(err error) error {
	if err == nil {
		return nil
	}
	if _, public := apperr.As(err); public || errors.Is(err, context.Canceled) {
		return err
	}
	if docker.Unavailable(err) && docker.StatusCode(err) < 500 {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrDockerTimeout
	}
	return ErrHelperFailed
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

// NoLogs keeps Docker's logging driver from storing a helper's output, such
// as a game console's raw reply, before Ludock redacts it. Attach and exec
// streams still work; only the mount validator, read through its logs,
// keeps logging.
var NoLogs = &docker.LogConfig{Type: "none"}

var digits = regexp.MustCompile(`^\d+$`)

func trimSlash(value string) string { return strings.TrimSuffix(path.Clean(value), "/") }

// MountProof verifies volume drivers and pins bind sources and volume
// subpaths through a private validator that sees only directory device and
// inode numbers. Docker follows links inside a volume to mount a subpath;
// the validator refuses links, so a helper's mount must prove it got the
// real folder at that path. It never runs user commands, reads game files,
// or exposes its host mount to a file endpoint. operationID, when set, labels the validator for recovery and
// makes cleanup retry until it is gone.
func MountProof(ctx context.Context, client *docker.Client, images *Images, mounts []identity.Mount, operationID string) (*Proof, error) {
	var sources []helper.MountSource
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
		if err != nil && !docker.IsNotFound(err) {
			return nil, HelperFailed(err)
		}
		if err != nil || info.Driver != "local" || len(info.Options) > 0 {
			return nil, ErrUnverifiedMount
		}
		if mount.Subpath != "" {
			if !strings.HasPrefix(info.Mountpoint, "/") {
				return nil, ErrUnverifiedMount
			}
			sources = append(sources, helper.MountSource{Source: trimSlash(path.Join(info.Mountpoint, mount.Subpath)),
				Destination: trimSlash(mount.Destination)})
		}
	}
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
	timedOut := creating.Err() != nil && ctx.Err() == nil
	cancel()
	if err != nil {
		// Validators only read, so removal need not hold the caller.
		removing, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		client.RemoveHelper(removing, name)
		done()
		if timedOut {
			return nil, ErrDockerTimeout
		}
		return nil, HelperFailed(err)
	}
	cleanup := func() {
		if operationID != "" {
			client.RemoveOperationHelper(id)
		} else {
			Step(context.WithoutCancel(ctx), "removing a mount validator", func(ctx context.Context) error { return client.RemoveHelper(ctx, id) })
		}
	}
	identities, err := readProof(ctx, client, id, sources)
	if err != nil {
		cleanup()
		return nil, HelperFailed(err)
	}
	return &Proof{Identities: identities, Cleanup: cleanup}, nil
}

func readProof(ctx context.Context, client *docker.Client, id string, sources []helper.MountSource) (map[string]helper.MountIdentity, error) {
	if err := Step(ctx, "starting a mount validator", func(ctx context.Context) error { return client.Start(ctx, id) }); err != nil {
		return nil, err
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	logs, err := client.Logs(deadline, id, docker.LogsOptions{Follow: true, Stdout: true, Tail: 10})
	// A validator that refuses exits at once, and Docker may remove it first.
	if docker.IsNotFound(err) {
		return nil, ErrUnverifiedMount
	}
	if err != nil && deadline.Err() != nil && ctx.Err() == nil {
		return nil, ErrDockerTimeout
	}
	if err != nil {
		return nil, HelperFailed(err)
	}
	defer logs.Close()
	stdout := docker.Stdout(logs)
	defer stdout.Close()
	reader := bufio.NewReaderSize(stdout, 16384)
	line, err := reader.ReadSlice('\n')
	if err != nil && deadline.Err() != nil && ctx.Err() == nil {
		return nil, ErrDockerTimeout
	}
	// A validator always prints a line, so an end without one means it died;
	// any other read error is Docker's.
	if errors.Is(err, io.EOF) {
		return nil, ErrUnverifiedMount
	}
	if err != nil {
		return nil, HelperFailed(err)
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
	_, err := Exec(deadline, client, container, ExecOptions{Cmd: helper.Command("mount-identities", identities), Errors: identityErrors})
	return HelperFailed(err)
}

// identityErrors keep a mismatch, exit 1, a mount refusal.
var identityErrors = &ExecErrors{Timeout: ErrDockerTimeout, Failed: ErrHelperFailed, Unsafe: ErrHelperFailed, OutputLimit: ErrHelperFailed,
	Exits: map[int]*apperr.Error{1: ErrUnverifiedMount}}

// createLimit bounds a validator's create; tests shorten it.
var createLimit = 2 * time.Minute

func randomSuffix() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
