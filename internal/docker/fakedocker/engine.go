// Package fakedocker is an in-memory Docker Engine API on a unix socket. It
// implements the endpoints Ludock uses so tests and browser journeys can run
// the real binary without a daemon. Never point it at real game servers.
package fakedocker

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
)

type Container struct {
	docker.ContainerInspect
	CreateRequest docker.ContainerCreate
	// ExtraConfig holds configuration fields ContainerInspect doesn't model,
	// such as Cmd and Volumes; inspection reports them inside Config.
	ExtraConfig             map[string]any
	HostConfig              map[string]any
	Networks                map[string]docker.EndpointSettings
	ImageManifestDescriptor *docker.ManifestDescriptor
	Platform                string
	// Logs are emitted on /logs as stdout lines.
	Logs  []string
	Stats docker.ContainerStats
	Stdin []byte
	// Files are served by /archive: regular files by absolute path, and
	// symbolic links whose value starts with "->", such as "-> /etc".
	Files map[string]string
	// IgnoresStop is a game that never gets the stop signal, as under a
	// shell entrypoint: a stop waits its whole timeout and kills it, unless
	// the game exits on its own first (Exit).
	IgnoresStop bool
}

type ExecHandler func(container *Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int

type Engine struct {
	mu         sync.Mutex
	containers map[string]*Container
	order      []string
	volumes    map[string]docker.VolumeInspect
	images     map[string]string
	// imageConfigs are the container defaults of each image ID.
	imageConfigs map[string]map[string]any
	// registry is the image ID each published tag points to; registryFail
	// is the status its registry lookups fail with.
	registry     map[string]string
	registryFail map[string]int
	// disks are the helper "space" results of host paths that journeys set,
	// or "" for a failed measurement.
	disks map[string]string
	// startAfter names, by restore helper step, the container to start
	// once that step has run.
	startAfter map[string]string
	execs      map[string]*exec
	events     []chan []byte
	// Exec handles exec commands; nil exits 0 without output.
	Exec ExecHandler
	// OnStart runs when a container starts, under no lock, e.g. to fill in
	// the logs a helper process would print; OnStop when one stops.
	OnStart func(c *Container)
	OnStop  func(c *Container)
	// OnPull runs, under no lock, before an image pull answers.
	OnPull func(reference string)
	// AddGame adds a fake game server for POST /fake/game, or reports false
	// for a game it doesn't know.
	AddGame func(game, name string) bool
	// Attach runs a container's process on an attached stream, under no
	// lock; nil collects stdin in Container.Stdin.
	Attach     func(c *Container, stdin *bufio.Reader, stdout func([]byte))
	APIVersion string
	// Fail makes every API request return this status when non-zero.
	Fail int
	// FailRemoves makes the next N container removals fail with status 500.
	FailRemoves int
	// HangExecStarts makes the next N exec starts accept the request and never
	// answer, as a stalled daemon can. The exec ends when the client hangs up.
	HangExecStarts int
	// FailExecStart reports whether Docker fails to run an exec of cmd once
	// its stream is open, as a daemon whose containerd times out does: Docker
	// ends the exec with code 128 without running it, then writes its error
	// on the stream's stdout.
	FailExecStart func(cmd []string) bool
	// ExitDelay records each exec's exit this long after its stream closes,
	// as Docker sometimes does.
	ExitDelay time.Duration
	// LoseUploads makes the next N file-helper uploads finish and then fail
	// their exec inspection, as a daemon that loses the result can.
	LoseUploads int
	// HangCreates makes the next N container creates make the container and
	// never answer, as a daemon whose response is lost can.
	HangCreates int
	// Hang, when it returns true for a request's "METHOD path", makes the
	// request do nothing and never answer, as a stalled daemon can.
	Hang func(request string) bool
	// Requests records "METHOD path" for each request.
	Requests []string
	// Creates records each container create body.
	Creates []docker.ContainerCreate
	// RegistryAuth records the X-Registry-Auth header of each registry lookup.
	RegistryAuth []string
	// Delay holds every Engine API request this long before it is handled,
	// as a slow daemon would; faults fail or stall chosen requests.
	Delay  time.Duration
	faults []*fault
	// streams are the hijacked connections an outage cuts.
	streams map[net.Conn]bool

	listener net.Listener
	server   *http.Server
	Socket   string
}

func Start(dir string) (*Engine, error) {
	socket := filepath.Join(dir, "docker.sock")
	os.Remove(socket)
	return StartSocket(socket)
}

// StartSocket serves on the unix socket at path, which must not exist.
func StartSocket(socket string) (*Engine, error) {
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	engine := &Engine{containers: map[string]*Container{}, volumes: map[string]docker.VolumeInspect{},
		images: map[string]string{}, imageConfigs: map[string]map[string]any{},
		registry: map[string]string{}, registryFail: map[string]int{}, disks: map[string]string{}, execs: map[string]*exec{}, APIVersion: "1.47", listener: listener, Socket: socket}
	engine.server = &http.Server{Handler: http.HandlerFunc(engine.serve)}
	go engine.server.Serve(listener)
	return engine, nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	for _, events := range e.events {
		close(events)
	}
	e.events = nil
	server := e.server
	e.mu.Unlock()
	return server.Close()
}

var idCounter atomic.Int64

func newID() string {
	return fmt.Sprintf("%064x", time.Now().UnixNano()+idCounter.Add(1))
}
