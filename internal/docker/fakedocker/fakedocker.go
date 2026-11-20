// Package fakedocker is an in-memory Docker Engine API on a unix socket. It
// implements the endpoints Ludock uses so tests and browser journeys can run
// the real binary without a daemon. Never point it at real game servers.
package fakedocker

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	disks  map[string]string
	execs  map[string]*exec
	events []chan []byte
	// Exec handles exec commands; nil exits 0 without output.
	Exec ExecHandler
	// OnStart runs when a container starts, under no lock, e.g. to fill in
	// the logs a helper process would print; OnStop when one stops.
	OnStart func(c *Container)
	OnStop  func(c *Container)
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

// fault fails (status) or stalls the next count requests whose
// "METHOD path" matches.
type fault struct {
	match  *regexp.Regexp
	status int
	stall  bool
	count  int
}

// AddFault fails the next count Engine API requests whose "METHOD path"
// matches pattern with status, or with stall leaves them unanswered until the
// client hangs up. A count below one means every match.
func (e *Engine) AddFault(pattern string, status int, stall bool, count int) error {
	match, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.faults = append(e.faults, &fault{match: match, status: status, stall: stall, count: count})
	return nil
}

// ClearFaults removes the delay and every fault.
func (e *Engine) ClearFaults() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Delay, e.faults = 0, nil
}

// faultLocked uses up and returns the first fault matching request, if any.
func (e *Engine) faultLocked(request string) *fault {
	for index, f := range e.faults {
		if !f.match.MatchString(request) {
			continue
		}
		if f.count > 0 {
			if f.count--; f.count == 0 {
				e.faults = slices.Delete(e.faults, index, index+1)
			}
		}
		return f
	}
	return nil
}

// Outage stops answering on the socket for duration, as a daemon that
// exits and is started again would: connections and streams close, helpers
// started with AutoRemove and every exec are gone, and new connections are refused until it
// is back. Other containers keep their state.
func (e *Engine) Outage(duration time.Duration) error {
	e.mu.Lock()
	for _, events := range e.events {
		close(events)
	}
	e.events = nil
	// Without live restore, Docker stops every container as it exits; those
	// created with AutoRemove are gone when it is back.
	for conn := range e.streams {
		conn.Close()
	}
	// A restarted daemon knows none of its earlier execs.
	e.execs = map[string]*exec{}
	for id, c := range e.containers {
		if autoRemove, _ := c.HostConfig["AutoRemove"].(bool); autoRemove {
			e.removeLocked(id)
		}
	}
	server := e.server
	e.mu.Unlock()
	server.Close()
	os.Remove(e.Socket)
	time.Sleep(duration)
	listener, err := net.Listen("unix", e.Socket)
	if err != nil {
		return err
	}
	e.mu.Lock()
	e.listener = listener
	e.server = &http.Server{Handler: http.HandlerFunc(e.serve)}
	server = e.server
	e.mu.Unlock()
	go server.Serve(listener)
	return nil
}

// Refuse stops accepting connections for duration, as a daemon going down
// does just before it closes the ones it has: a request already being
// answered finishes, but its connection is not kept. It returns at once.
func (e *Engine) Refuse(duration time.Duration) {
	e.mu.Lock()
	listener, server := e.listener, e.server
	e.mu.Unlock()
	server.SetKeepAlivesEnabled(false)
	listener.Close()
	os.Remove(e.Socket)
	go func() {
		time.Sleep(duration)
		listener, err := net.Listen("unix", e.Socket)
		if err != nil {
			return
		}
		e.mu.Lock()
		e.listener = listener
		e.server = &http.Server{Handler: http.HandlerFunc(e.serve)}
		server := e.server
		e.mu.Unlock()
		server.Serve(listener)
	}()
}

type exec struct {
	id        string
	container string
	options   docker.ExecCreate
	running   bool
	exitCode  *int
	lost      bool
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

type GameOptions struct {
	ID, Name, Image string
	Labels          map[string]string
	Env             []string
	Running         bool
	Mounts          []docker.MountPoint
	Ports           map[string][]docker.PortBinding
	// OpenStdin is stdin_open: true.
	OpenStdin bool
}

func (e *Engine) AddContainer(options GameOptions) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := options.ID
	if id == "" {
		id = newID()
	}
	status := "exited"
	if options.Running {
		status = "running"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	labels := options.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	container := &Container{ContainerInspect: docker.ContainerInspect{
		ID: id, Image: "sha256:" + strings.Repeat("0", 64), Name: "/" + options.Name, Created: now,
		Config: docker.ContainerConfig{Image: options.Image, Labels: labels, Env: options.Env, OpenStdin: options.OpenStdin},
		State: docker.ContainerState{Status: status, Running: options.Running, StartedAt: now,
			FinishedAt: "0001-01-01T00:00:00Z"},
		Mounts: options.Mounts, NetworkSettings: docker.NetworkSettings{Ports: options.Ports},
	}}
	e.containers[id] = container
	e.order = append(e.order, id)
	e.emitLocked("create", id)
	return id
}

// Update changes a container under the engine lock.
func (e *Engine) Update(id string, change func(*Container)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if container := e.containers[id]; container != nil {
		change(container)
	}
}

// Container returns a copy of a container's inspection.
func (e *Engine) Container(id string) *docker.ContainerInspect {
	e.mu.Lock()
	defer e.mu.Unlock()
	if container := e.containers[id]; container != nil {
		copy := container.ContainerInspect
		return &copy
	}
	return nil
}

func (e *Engine) RemoveContainer(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removeLocked(id)
}

// Exit ends a running container's process with code, as a game that quits
// on its own command.
func (e *Engine) Exit(id string, code int) {
	e.mu.Lock()
	c := e.containers[id]
	if c == nil || !c.State.Running {
		e.mu.Unlock()
		return
	}
	hook := e.exitLocked(c, code)
	e.mu.Unlock()
	if hook != nil {
		hook(c)
	}
}

// exitLocked returns OnStop, to run once the lock is released.
func (e *Engine) exitLocked(c *Container, code int) func(*Container) {
	c.State.Status, c.State.Running, c.State.ExitCode = "exited", false, code
	c.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	// Docker releases a stopped container's addresses.
	for name, endpoint := range c.Networks {
		endpoint.IPAddress, endpoint.MacAddress = "", ""
		c.Networks[name] = endpoint
	}
	e.emitLocked("stop", c.ID)
	e.emitLocked("die", c.ID)
	// Docker removes an AutoRemove container once it stops.
	if autoRemove, _ := c.HostConfig["AutoRemove"].(bool); autoRemove {
		e.removeLocked(c.ID)
	}
	return e.OnStop
}

func (e *Engine) removeLocked(id string) {
	// The event carries the labels, so it is built before the container goes;
	// subscribers act on it only after the lock is released.
	e.emitLocked("destroy", id)
	delete(e.containers, id)
	for index, existing := range e.order {
		if existing == id {
			e.order = append(e.order[:index], e.order[index+1:]...)
			break
		}
	}
}

func (e *Engine) emitLocked(action, id string) {
	// Docker sends the container's labels and name as actor attributes.
	attributes := map[string]string{}
	if c := e.containers[id]; c != nil {
		for key, value := range c.Config.Labels {
			attributes[key] = value
		}
		attributes["name"] = strings.TrimPrefix(c.Name, "/")
	}
	event, _ := json.Marshal(map[string]any{"Type": "container", "Action": action,
		"Actor": map[string]any{"ID": id, "Attributes": attributes}, "id": id, "time": time.Now().Unix()})
	event = append(event, '\n')
	for _, events := range e.events {
		select {
		case events <- event:
		default:
		}
	}
}

var (
	versioned    = regexp.MustCompile(`^/v1\.\d+(/.*)$`)
	container    = regexp.MustCompile(`^/containers/([^/]+)(/[a-z]+)?$`)
	execPath     = regexp.MustCompile(`^/exec/([^/]+)/(start|json)$`)
	volume       = regexp.MustCompile(`^/volumes/([^/]+)$`)
	image        = regexp.MustCompile(`^/images/(.+)/json$`)
	distribution = regexp.MustCompile(`^/distribution/(.+)/json$`)
)

// control serves the /fake/ endpoints that journeys and tests use to change
// the daemon; faults and delays never apply to them.
func (e *Engine) control(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/fake/fault":
		// Injects faults: POST /fake/fault?delay=MS holds every request;
		// &match=REGEXP with &status=CODE fails, or &stall=1 stalls, the next
		// &count=N requests whose "METHOD path" matches (every one without a
		// count); &clear=1 removes all of them first.
		query := r.URL.Query()
		if query.Get("clear") != "" {
			e.ClearFaults()
		}
		if delay := query.Get("delay"); delay != "" {
			ms, _ := strconv.Atoi(delay)
			e.mu.Lock()
			e.Delay = time.Duration(ms) * time.Millisecond
			e.mu.Unlock()
		}
		if pattern := query.Get("match"); pattern != "" {
			status, _ := strconv.Atoi(query.Get("status"))
			count, _ := strconv.Atoi(query.Get("count"))
			if err := e.AddFault(pattern, status, query.Get("stall") != "", count); err != nil {
				fail(w, 400)
				return
			}
		}
		w.WriteHeader(204)
		return
	case "/fake/outage":
		// Goes away for a while, as a restarted daemon does:
		// POST /fake/outage?seconds=N answers, then the socket closes.
		seconds, _ := strconv.ParseFloat(r.URL.Query().Get("seconds"), 64)
		w.WriteHeader(204)
		go func() {
			time.Sleep(50 * time.Millisecond)
			e.Outage(time.Duration(seconds * float64(time.Second)))
		}()
		return
	case "/fake/requests":
		// Lists every Engine API request so far, one "METHOD path" a line.
		e.mu.Lock()
		requests := strings.Join(e.Requests, "\n")
		e.mu.Unlock()
		w.Write([]byte(requests))
		return
	case "/fake/rcon":
		// Counts the connections the fake game consoles have taken.
		fmt.Fprint(w, rconConnections.Load())
		return
	case "/fake/registry":
		// Lets journeys publish a new version of a tag or break its registry:
		// POST /fake/registry?image=REF&id=IMAGE-ID or &status=CODE.
		query := r.URL.Query()
		if id := query.Get("id"); id != "" {
			e.PublishImage(query.Get("image"), id)
		}
		if status := query.Get("status"); status != "" {
			code, _ := strconv.Atoi(status)
			e.FailRegistry(query.Get("image"), code)
		}
		w.WriteHeader(204)
		return
	case "/fake/disk":
		// Sets what measuring the disk under a host path reports:
		// POST /fake/disk?path=PATH&used=BYTES&available=BYTES, &fail=1 for a
		// failed measurement, or neither to measure for real.
		query := r.URL.Query()
		used, _ := strconv.ParseInt(query.Get("used"), 10, 64)
		available, _ := strconv.ParseInt(query.Get("available"), 10, 64)
		e.mu.Lock()
		switch {
		case query.Get("fail") != "":
			e.disks[query.Get("path")] = ""
		case query.Get("used") != "":
			encoded, _ := json.Marshal(map[string]int64{"total": used + available, "used": used, "available": available})
			e.disks[query.Get("path")] = string(encoded)
		default:
			delete(e.disks, query.Get("path"))
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/lose-upload":
		// Makes the next file write finish and then report an unknown result.
		e.mu.Lock()
		e.LoseUploads++
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/recreate":
		// Recreates a container with changed settings under a new ID, as
		// Compose or Portainer would: POST /fake/recreate?container=NAME
		// with any number of env=KEY=VALUE and label=KEY=VALUE; a KEY alone
		// removes it. image=REF changes the image reference it was created
		// from, keeping the image it runs. mount=DESTINATION=SOURCE moves the
		// bind mount at DESTINATION to another host folder.
		query := r.URL.Query()
		e.Recreate(query.Get("container"), query)
		w.WriteHeader(204)
		return
	case "/fake/game":
		// Adds a fake game server like a real image's: POST
		// /fake/game?game=GAME&name=NAME. POST /fake/remove?container=NAME
		// removes it.
		query := r.URL.Query()
		if e.AddGame == nil || !e.AddGame(query.Get("game"), query.Get("name")) {
			fail(w, 404)
			return
		}
		w.WriteHeader(204)
		return
	case "/fake/remove":
		e.mu.Lock()
		if c := e.find(r.URL.Query().Get("container")); c != nil {
			e.removeLocked(c.ID)
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/health":
		// Sets what a container's health check reports, as Docker does once
		// its retries agree: POST /fake/health?container=NAME&status=unhealthy
		// or healthy.
		query := r.URL.Query()
		e.mu.Lock()
		if c := e.find(query.Get("container")); c != nil && c.State.Health != nil {
			result := docker.HealthResult{End: time.Now().UTC().Format(time.RFC3339Nano)}
			if query.Get("status") != "healthy" {
				result.ExitCode = 1
			}
			// Docker keeps the latest five results.
			c.State.Health.Status, c.State.Health.Log = query.Get("status"), append(c.State.Health.Log, result)
			c.State.Health.Log = c.State.Health.Log[max(0, len(c.State.Health.Log)-5):]
			e.emitLocked("health_status: "+c.State.Health.Status, c.ID)
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/stats":
		// Sets a container's memory in use: POST /fake/stats?container=NAME&memory=BYTES.
		query := r.URL.Query()
		memory, _ := strconv.ParseFloat(query.Get("memory"), 64)
		e.mu.Lock()
		if c := e.find(query.Get("container")); c != nil {
			c.Stats.MemoryStats.Usage, c.Stats.MemoryStats.Stats = memory, nil
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	fail(w, 404)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]string{"message": "fake docker error"})
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/fake/") {
		e.control(w, r)
		return
	}
	e.mu.Lock()
	e.Requests = append(e.Requests, r.Method+" "+r.URL.Path)
	failure, delay, injected, hang := e.Fail, e.Delay, e.faultLocked(r.Method+" "+r.URL.Path), e.Hang
	e.mu.Unlock()
	if hang != nil && hang(r.Method+" "+r.URL.Path) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if injected != nil && injected.stall {
		<-r.Context().Done()
		return
	}
	if injected != nil {
		failure = injected.status
	}
	if failure != 0 {
		fail(w, failure)
		return
	}
	switch path {
	case "/_ping":
		w.Write([]byte("OK"))
		return
	case "/version":
		writeJSON(w, 200, map[string]string{"ApiVersion": e.APIVersion, "MinAPIVersion": "1.24"})
		return
	}
	match := versioned.FindStringSubmatch(path)
	if match == nil {
		fail(w, 404)
		return
	}
	path = match[1]
	switch {
	case path == "/containers/json" && r.Method == "GET":
		e.list(w, r)
	case path == "/containers/create" && r.Method == "POST":
		e.create(w, r)
	case path == "/events":
		e.streamEvents(w, r)
	case path == "/volumes" && r.Method == "GET":
		e.mu.Lock()
		volumes := []docker.VolumeInspect{}
		for _, info := range e.volumes {
			volumes = append(volumes, info)
		}
		e.mu.Unlock()
		writeJSON(w, 200, map[string]any{"Volumes": volumes})
	case path == "/volumes/create" && r.Method == "POST":
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		info := docker.VolumeInspect{Name: body.Name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + body.Name + "/_data"}
		e.volumes[body.Name] = info
		e.mu.Unlock()
		writeJSON(w, 201, info)
	case path == "/images/create" && r.Method == "POST":
		reference := r.URL.Query().Get("fromImage")
		e.mu.Lock()
		pulled, ok := e.registry[reference]
		if !ok {
			pulled = "sha256:" + strings.Repeat("1", 64)
		}
		e.images[reference] = pulled
		e.mu.Unlock()
		w.Write([]byte(`{"status":"Pulled"}` + "\n"))
	case container.MatchString(path):
		parts := container.FindStringSubmatch(path)
		e.containerRequest(w, r, parts[1], parts[2])
	case execPath.MatchString(path):
		parts := execPath.FindStringSubmatch(path)
		e.execRequest(w, r, parts[1], parts[2])
	case volume.MatchString(path):
		name := volume.FindStringSubmatch(path)[1]
		e.mu.Lock()
		info, ok := e.volumes[name]
		if ok && r.Method == "DELETE" {
			delete(e.volumes, name)
		}
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else if r.Method == "DELETE" {
			w.WriteHeader(204)
		} else {
			writeJSON(w, 200, info)
		}
	case image.MatchString(path):
		reference := image.FindStringSubmatch(path)[1]
		e.mu.Lock()
		id, ok := e.images[reference]
		// Every image ID a container reports exists.
		if !ok && strings.HasPrefix(reference, "sha256:") {
			id, ok = reference, true
		}
		config := e.imageConfigs[id]
		if variant, ok := e.imageConfigs[id+" "+r.URL.Query().Get("platform")]; ok {
			config = variant
		}
		repoDigests := e.repoDigestsLocked(id)
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else {
			writeJSON(w, 200, map[string]any{"Id": id, "Config": config, "RepoDigests": repoDigests})
		}
	case distribution.MatchString(path):
		reference := distribution.FindStringSubmatch(path)[1]
		e.mu.Lock()
		e.RegistryAuth = append(e.RegistryAuth, r.Header.Get("X-Registry-Auth"))
		status, id := e.registryFail[reference], e.tagLocked(reference)
		e.mu.Unlock()
		if status == 0 && id == "" {
			// Docker answers 401 both for refused credentials and unknown tags.
			status = 401
		}
		if status != 0 {
			fail(w, status)
			return
		}
		writeJSON(w, 200, map[string]any{
			"Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": ManifestDigest(id)},
			"Platforms":  []docker.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}})
	default:
		fail(w, 404)
	}
}

func (e *Engine) AddImage(reference string) {
	e.mu.Lock()
	e.images[reference] = "sha256:" + strings.Repeat("2", 64)
	e.mu.Unlock()
}

// SetImageConfig sets the container defaults of an image ID, as its
// Dockerfile would.
func (e *Engine) SetImageConfig(id string, config map[string]any) {
	e.mu.Lock()
	e.imageConfigs[id] = config
	e.mu.Unlock()
}

// SetVariantConfig sets the container defaults of one platform variant of
// an image ID, which inspection returns when asked for that platform.
func (e *Engine) SetVariantConfig(id string, platform docker.Platform, config map[string]any) {
	encoded, _ := json.Marshal(platform)
	e.mu.Lock()
	e.imageConfigs[id+" "+string(encoded)] = config
	e.mu.Unlock()
}

// ManifestDigest is the registry digest the fake gives image ID id.
func ManifestDigest(id string) string {
	sum := sha256.Sum256([]byte("manifest " + id))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PublishImage points reference's tag in the fake registry at image ID id,
// which pulling it then yields. The new image keeps the defaults of the one
// the tag held.
func (e *Engine) PublishImage(reference, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.imageConfigs[id]; !ok {
		if config, ok := e.imageConfigs[e.tagLocked(reference)]; ok {
			e.imageConfigs[id] = config
		}
	}
	e.registry[reference] = id
}

// HelperLogging maps each kind of helper created (its ludock.internal
// label) to the logging driver it asked for, "" for Docker's default.
func (e *Engine) HelperLogging() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	kinds := map[string]string{}
	for _, body := range e.Creates {
		if kind := body.Labels["ludock.internal"]; kind != "" {
			driver := ""
			if body.HostConfig != nil && body.HostConfig.LogConfig != nil {
				driver = body.HostConfig.LogConfig.Type
			}
			kinds[kind] = driver
		}
	}
	return kinds
}

// Disk is the measurement a journey set for a host path: its JSON, or ""
// for a failure. ok is false when the path is measured for real.
func (e *Engine) Disk(path string) (result string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, ok = e.disks[path]
	return result, ok
}

// FailRegistry makes registry lookups of reference fail with status, or
// succeed again with 0.
func (e *Engine) FailRegistry(reference string, status int) {
	e.mu.Lock()
	e.registryFail[reference] = status
	e.mu.Unlock()
}

// tagLocked is the image ID reference's tag points to: the published one,
// else the local tag, else the image a container created from it runs, as
// if the registry still held what was pulled.
func (e *Engine) tagLocked(reference string) string {
	if id, ok := e.registry[reference]; ok {
		return id
	}
	if id, ok := e.images[reference]; ok {
		return id
	}
	for _, id := range e.order {
		if c := e.containers[id]; c.Config.Image == reference {
			return c.Image
		}
	}
	return ""
}

// repoDigestsLocked lists "repository@digest" for each reference whose tag
// or container uses image ID id, as a pull of it would have recorded.
func (e *Engine) repoDigestsLocked(id string) []string {
	references := map[string]bool{}
	for reference, local := range e.images {
		if local == id {
			references[reference] = true
		}
	}
	for _, c := range e.containers {
		if c.Image == id && c.Config.Image != "" {
			references[c.Config.Image] = true
		}
	}
	digests := []string{}
	for reference := range references {
		repository, _, _ := strings.Cut(reference, "@")
		if slash, colon := strings.LastIndex(repository, "/"), strings.LastIndex(repository, ":"); colon > slash {
			repository = repository[:colon]
		}
		digests = append(digests, repository+"@"+ManifestDigest(id))
	}
	sort.Strings(digests)
	return slices.Compact(digests)
}

func (e *Engine) list(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	all := r.URL.Query().Get("all") == "true"
	result := []docker.ContainerSummary{}
	for _, id := range e.order {
		c := e.containers[id]
		if !all && !c.State.Running {
			continue
		}
		state, status := c.State.Status, c.State.Status
		// Like Docker, a container whose tag now points to another image, such
		// as after a pull, is listed by its own image's ID.
		image := c.Config.Image
		if local, ok := e.images[image]; ok && local != c.Image {
			image = c.Image
		}
		result = append(result, docker.ContainerSummary{ID: id, Names: []string{c.Name}, Image: image,
			Labels: c.Config.Labels, State: &state, Status: &status, Mounts: c.Mounts})
	}
	writeJSON(w, 200, result)
}

func (e *Engine) create(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body docker.ContainerCreate
	var fields map[string]any
	if json.Unmarshal(raw, &body) != nil || json.Unmarshal(raw, &fields) != nil {
		fail(w, 400)
		return
	}
	name := r.URL.Query().Get("name")
	e.mu.Lock()
	imageID, known := e.images[body.Image]
	defaults := e.imageConfigs[imageID]
	taken := name != "" && e.find(name) != nil
	e.Creates = append(e.Creates, body)
	e.mu.Unlock()
	if !known {
		fail(w, 404)
		return
	}
	if taken {
		fail(w, 409)
		return
	}
	host, _ := fields["HostConfig"].(map[string]any)
	var networking struct {
		EndpointsConfig map[string]docker.EndpointSettings
	}
	if encoded, err := json.Marshal(fields["NetworkingConfig"]); err == nil {
		json.Unmarshal(encoded, &networking)
	}
	delete(fields, "HostConfig")
	delete(fields, "NetworkingConfig")
	config := mergeDefaults(fields, defaults)
	var labels map[string]string
	var env []string
	remarshal(config["Labels"], &labels)
	remarshal(config["Env"], &env)
	mounts := e.mountsOf(body, config)
	id := e.AddContainer(GameOptions{Name: name, Image: body.Image, Labels: labels, Env: env})
	e.Update(id, func(c *Container) {
		c.Image = imageID
		c.Config.OpenStdin, c.Config.StdinOnce, c.Config.Tty = body.OpenStdin, body.StdinOnce, body.Tty
		c.CreateRequest, c.ExtraConfig, c.HostConfig, c.Networks = body, config, host, networking.EndpointsConfig
		c.Platform = r.URL.Query().Get("platform")
		c.Mounts = append(c.Mounts, mounts...)
	})
	e.mu.Lock()
	hang := e.HangCreates > 0
	if hang {
		e.HangCreates--
	}
	e.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	writeJSON(w, 201, map[string]string{"Id": id})
}

func remarshal(value, target any) {
	if encoded, err := json.Marshal(value); err == nil {
		json.Unmarshal(encoded, target)
	}
}

// mergeDefaults applies an image's defaults to a create request as Docker
// does.
func mergeDefaults(config, image map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range config {
		result[key] = value
	}
	set := func(key string) bool {
		value, ok := result[key]
		if !ok || value == nil || value == "" {
			return false
		}
		if list, isList := value.([]any); isList {
			return len(list) > 0
		}
		return true
	}
	if !set("Entrypoint") && !set("Cmd") {
		result["Cmd"] = image["Cmd"]
	}
	for _, key := range []string{"Entrypoint", "WorkingDir", "User", "StopSignal", "Healthcheck"} {
		if !set(key) && image[key] != nil {
			result[key] = image[key]
		}
	}
	var env, imageEnv []string
	remarshal(result["Env"], &env)
	remarshal(image["Env"], &imageEnv)
	var merged []string
	for _, entry := range imageEnv {
		key, _, _ := strings.Cut(entry, "=")
		if !slices.ContainsFunc(env, func(own string) bool { return strings.HasPrefix(own, key+"=") || own == key }) {
			merged = append(merged, entry)
		}
	}
	result["Env"] = append(merged, env...)
	for _, key := range []string{"Labels", "Volumes", "ExposedPorts"} {
		union := map[string]any{}
		imageValues, _ := image[key].(map[string]any)
		ownValues, _ := result[key].(map[string]any)
		for name, value := range imageValues {
			union[name] = value
		}
		for name, value := range ownValues {
			union[name] = value
		}
		result[key] = union
	}
	return result
}

func (c *Container) inspection() map[string]any {
	var result map[string]any
	remarshal(c.ContainerInspect, &result)
	config, _ := result["Config"].(map[string]any)
	for key, value := range c.ExtraConfig {
		if _, modeled := config[key]; !modeled {
			config[key] = value
		}
	}
	host := c.HostConfig
	if host == nil {
		// Docker reports the binds that made a container's bind and named
		// volume mounts.
		binds := []string{}
		for _, mount := range c.Mounts {
			source := mount.Source
			if mount.Type == "volume" {
				source = mount.Name
			}
			bind := source + ":" + mount.Destination
			if !mount.RW {
				bind += ":ro"
			}
			binds = append(binds, bind)
		}
		host = map[string]any{"NetworkMode": "bridge", "Binds": binds}
	}
	if c.Host.Memory > 0 {
		host = maps.Clone(host)
		host["Memory"] = c.Host.Memory
	}
	if c.Host.Privileged {
		host = maps.Clone(host)
		host["Privileged"] = true
	}
	result["HostConfig"] = host
	if settings, ok := result["NetworkSettings"].(map[string]any); ok {
		settings["Networks"] = c.Networks
	}
	if c.ImageManifestDescriptor != nil {
		result["ImageManifestDescriptor"] = c.ImageManifestDescriptor
	}
	return result
}

// Recreate applies the /fake/recreate control to a container and returns
// its new ID, or "" when there is no such container.
func (e *Engine) Recreate(reference string, query url.Values) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(reference)
	if c == nil {
		return ""
	}
	return e.recreateLocked(c, query.Get("image"), query["env"], query["label"], query["mount"])
}

func (e *Engine) recreateLocked(c *Container, image string, env, labels, mounts []string) string {
	replacement := *c
	replacement.ID = newID()
	replacement.Mounts = slices.Clone(c.Mounts)
	for _, entry := range mounts {
		destination, source, _ := strings.Cut(entry, "=")
		for index := range replacement.Mounts {
			if mount := &replacement.Mounts[index]; mount.Type == "bind" && mount.Destination == destination {
				mount.Source = source
			}
		}
	}
	if image != "" {
		replacement.Config.Image = image
	}
	replacement.Config.Env = slices.Clone(c.Config.Env)
	for _, entry := range env {
		key, _, set := strings.Cut(entry, "=")
		replacement.Config.Env = slices.DeleteFunc(replacement.Config.Env, func(existing string) bool { return strings.HasPrefix(existing, key+"=") })
		if set {
			replacement.Config.Env = append(replacement.Config.Env, entry)
		}
	}
	replacement.Config.Labels = maps.Clone(c.Config.Labels)
	for _, entry := range labels {
		if key, value, set := strings.Cut(entry, "="); set {
			replacement.Config.Labels[key] = value
		} else {
			delete(replacement.Config.Labels, key)
		}
	}
	index := slices.Index(e.order, c.ID)
	e.removeLocked(c.ID)
	e.containers[replacement.ID] = &replacement
	e.order = slices.Insert(e.order, index, replacement.ID)
	e.emitLocked("create", replacement.ID)
	return replacement.ID
}

func (e *Engine) find(reference string) *Container {
	if c := e.containers[reference]; c != nil {
		return c
	}
	for id, c := range e.containers {
		if strings.HasPrefix(id, reference) || strings.TrimPrefix(c.Name, "/") == reference {
			return c
		}
	}
	return nil
}

func (e *Engine) containerRequest(w http.ResponseWriter, r *http.Request, reference, action string) {
	e.mu.Lock()
	c := e.find(reference)
	if c == nil {
		e.mu.Unlock()
		fail(w, 404)
		return
	}
	id := c.ID
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch {
	case action == "/json" && r.Method == "GET":
		info := c.inspection()
		e.mu.Unlock()
		writeJSON(w, 200, info)
	case action == "/update" && r.Method == "POST":
		if c.State.Status == "dead" {
			e.mu.Unlock()
			fail(w, 409)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if c.HostConfig == nil {
			c.HostConfig = c.inspection()["HostConfig"].(map[string]any)
		}
		if policy, ok := body["RestartPolicy"]; ok {
			c.HostConfig["RestartPolicy"] = policy
		}
		e.mu.Unlock()
		writeJSON(w, 200, map[string]any{"Warnings": []string{}})
	case action == "/rename" && r.Method == "POST":
		name := r.URL.Query().Get("name")
		if other := e.find(name); other != nil && other != c {
			e.mu.Unlock()
			fail(w, 409)
			return
		}
		c.Name = "/" + name
		e.emitLocked("rename", id)
		e.mu.Unlock()
		w.WriteHeader(204)
	case action == "/start" && r.Method == "POST":
		if c.State.Running {
			e.mu.Unlock()
			w.WriteHeader(304)
			return
		}
		c.State.Status, c.State.Running, c.State.StartedAt = "running", true, now
		// Healthchecks pass at once.
		if check, _ := c.ExtraConfig["Healthcheck"].(map[string]any); check != nil {
			if test, _ := check["Test"].([]any); len(test) > 0 && test[0] != "NONE" {
				c.State.Health = &docker.HealthState{Status: "healthy", Log: []docker.HealthResult{{End: now}}}
			}
		}
		e.emitLocked("start", id)
		hook := e.OnStart
		e.mu.Unlock()
		if hook != nil {
			hook(c)
		}
		w.WriteHeader(204)
	case action == "/stop" && r.Method == "POST":
		if !c.State.Running {
			e.mu.Unlock()
			w.WriteHeader(304)
			return
		}
		if !c.IgnoresStop {
			hook := e.exitLocked(c, 0)
			e.mu.Unlock()
			if hook != nil {
				hook(c)
			}
			w.WriteHeader(204)
			return
		}
		e.mu.Unlock()
		seconds, err := strconv.Atoi(r.URL.Query().Get("t"))
		if err != nil {
			seconds = 10
		}
		for deadline := time.Now().Add(time.Duration(seconds) * time.Second); ; {
			e.mu.Lock()
			if !c.State.Running {
				e.mu.Unlock()
				break
			}
			if time.Now().After(deadline) {
				hook := e.exitLocked(c, 137)
				e.mu.Unlock()
				if hook != nil {
					hook(c)
				}
				break
			}
			e.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}
		w.WriteHeader(204)
	case action == "/restart" && r.Method == "POST":
		c.State.Status, c.State.Running, c.State.StartedAt = "running", true, now
		e.emitLocked("restart", id)
		e.mu.Unlock()
		w.WriteHeader(204)
	case action == "/wait" && r.Method == "POST":
		e.mu.Unlock()
		for {
			e.mu.Lock()
			current := e.containers[id]
			done := current == nil || !current.State.Running
			code := 0
			if current != nil {
				code = current.State.ExitCode
			}
			e.mu.Unlock()
			if done {
				writeJSON(w, 200, map[string]int{"StatusCode": code})
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	case action == "/stats":
		stats := c.Stats
		e.mu.Unlock()
		writeJSON(w, 200, stats)
	case action == "/logs":
		lines := append([]string(nil), c.Logs...)
		e.mu.Unlock()
		sent := len(lines)
		if tail, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && tail >= 0 && tail < len(lines) {
			lines = lines[len(lines)-tail:]
		}
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.WriteHeader(200)
		write := func(lines []string) {
			for _, line := range lines {
				if r.URL.Query().Get("timestamps") == "true" {
					line = now + " " + line
				}
				w.Write(Frame(1, []byte(line+"\n")))
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		write(lines)
		// Like Docker, a followed log streams the lines written after it opened.
		for r.URL.Query().Get("follow") == "true" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			e.mu.Lock()
			var added []string
			if current := e.containers[id]; current != nil && len(current.Logs) > sent {
				added = append(added, current.Logs[sent:]...)
				sent = len(current.Logs)
			}
			e.mu.Unlock()
			write(added)
		}
	case action == "/exec" && r.Method == "POST":
		var options docker.ExecCreate
		json.NewDecoder(r.Body).Decode(&options)
		execID := newID()
		e.execs[execID] = &exec{id: execID, container: id, options: options}
		e.mu.Unlock()
		writeJSON(w, 201, map[string]string{"Id": execID})
	case action == "/archive" && r.Method == "GET":
		content, ok := c.Files[r.URL.Query().Get("path")]
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
			return
		}
		header := &tar.Header{Name: filepath.Base(r.URL.Query().Get("path")), Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(content))}
		if target, link := strings.CutPrefix(content, "->"); link {
			header.Typeflag, header.Linkname, header.Size, content = tar.TypeSymlink, strings.TrimSpace(target), 0, ""
		}
		w.Header().Set("Content-Type", "application/x-tar")
		archive := tar.NewWriter(w)
		archive.WriteHeader(header)
		archive.Write([]byte(content))
		archive.Close()
	case action == "/attach" && r.Method == "POST":
		attach := e.Attach
		e.mu.Unlock()
		if attach != nil {
			e.hijack(w, r, func(conn net.Conn, reader *bufio.Reader) {
				attach(c, reader, func(data []byte) { conn.Write(Frame(1, data)) })
			})
			return
		}
		e.hijack(w, r, func(conn net.Conn, reader *bufio.Reader) {
			data, _ := io.ReadAll(reader)
			e.mu.Lock()
			if current := e.containers[id]; current != nil {
				current.Stdin = append(current.Stdin, data...)
			}
			e.mu.Unlock()
		})
	case action == "" && r.Method == "DELETE":
		if e.FailRemoves > 0 {
			e.FailRemoves--
			e.mu.Unlock()
			fail(w, 500)
			return
		}
		if c.State.Running && r.URL.Query().Get("force") != "true" {
			e.mu.Unlock()
			fail(w, 409)
			return
		}
		e.removeLocked(id)
		e.mu.Unlock()
		w.WriteHeader(204)
	default:
		e.mu.Unlock()
		fail(w, 404)
	}
}

func (e *Engine) hijack(w http.ResponseWriter, r *http.Request, run func(net.Conn, *bufio.Reader)) {
	// The request body precedes the raw stream.
	io.Copy(io.Discard, r.Body)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		fail(w, 500)
		return
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	e.mu.Lock()
	if e.streams == nil {
		e.streams = map[net.Conn]bool{}
	}
	e.streams[conn] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.streams, conn)
		e.mu.Unlock()
		conn.Close()
	}()
	buffered.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	buffered.Flush()
	run(conn, buffered.Reader)
}

func (e *Engine) execRequest(w http.ResponseWriter, r *http.Request, id, action string) {
	e.mu.Lock()
	instance := e.execs[id]
	var c *Container
	if instance != nil {
		c = e.containers[instance.container]
	}
	e.mu.Unlock()
	if instance == nil {
		fail(w, 404)
		return
	}
	if action == "json" {
		e.mu.Lock()
		info := docker.ExecInspect{Running: instance.running, ExitCode: instance.exitCode}
		lost := instance.lost
		e.mu.Unlock()
		if lost {
			fail(w, 500)
			return
		}
		writeJSON(w, 200, info)
		return
	}
	e.mu.Lock()
	instance.running = true
	handler := e.Exec
	hang := e.HangExecStarts > 0
	if hang {
		e.HangExecStarts--
	}
	e.mu.Unlock()
	if hang {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		killed := 137
		e.mu.Lock()
		instance.running, instance.exitCode = false, &killed
		e.mu.Unlock()
		return
	}
	e.hijack(w, r, func(conn net.Conn, reader *bufio.Reader) {
		var mu sync.Mutex
		write := func(channel byte) func([]byte) {
			return func(data []byte) {
				mu.Lock()
				defer mu.Unlock()
				conn.Write(Frame(channel, data))
			}
		}
		code := 0
		if handler != nil && c != nil {
			code = handler(c, instance.options.Cmd, reader, write(1), write(2))
		}
		e.mu.Lock()
		delay := e.ExitDelay
		e.mu.Unlock()
		if delay > 0 {
			conn.Close()
			time.Sleep(delay)
		}
		e.mu.Lock()
		instance.running = false
		instance.exitCode = &code
		if e.LoseUploads > 0 && slices.ContainsFunc(instance.options.Cmd, func(arg string) bool { return strings.Contains(arg, `"operation":"upload"`) }) {
			e.LoseUploads--
			instance.lost = true
		}
		e.mu.Unlock()
	})
}

func (e *Engine) streamEvents(w http.ResponseWriter, r *http.Request) {
	events := make(chan []byte, 256)
	e.mu.Lock()
	e.events = append(e.events, events)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		for index, existing := range e.events {
			if existing == events {
				e.events = append(e.events[:index], e.events[index+1:]...)
				break
			}
		}
		e.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			w.Write(event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// mountsOf lists the mounts Docker would report for a created container:
// its mounts and binds, and an anonymous volume for each other declared
// volume.
func (e *Engine) mountsOf(body docker.ContainerCreate, config map[string]any) []docker.MountPoint {
	var mounts []docker.MountPoint
	mounted := map[string]bool{}
	if body.HostConfig != nil {
		for _, mount := range body.HostConfig.Mounts {
			mounts = append(mounts, docker.MountPoint{Type: mount.Type, Source: mount.Source, Destination: mount.Target,
				RW: !mount.ReadOnly, Name: mount.Source})
			mounted[mount.Target] = true
		}
		for _, bind := range body.HostConfig.Binds {
			parts := strings.Split(bind, ":")
			if len(parts) < 2 {
				// A destination-only bind is an anonymous volume, which Docker
				// reports without its name.
				name := newID()
				info := docker.VolumeInspect{Name: name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + name + "/_data"}
				e.mu.Lock()
				e.volumes[name] = info
				e.mu.Unlock()
				mounts = append(mounts, docker.MountPoint{Type: "volume", Source: info.Mountpoint, Destination: parts[0], RW: true})
				mounted[parts[0]] = true
				continue
			}
			mount := docker.MountPoint{Type: "bind", Source: parts[0], Destination: parts[1], RW: !slices.Contains(parts[2:], "ro")}
			if !strings.HasPrefix(parts[0], "/") {
				mount.Type, mount.Name = "volume", parts[0]
			}
			mounts = append(mounts, mount)
			mounted[parts[1]] = true
		}
	}
	volumes, _ := config["Volumes"].(map[string]any)
	destinations := slices.Sorted(maps.Keys(volumes))
	for _, destination := range destinations {
		if mounted[destination] {
			continue
		}
		name := newID()
		info := docker.VolumeInspect{Name: name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + name + "/_data"}
		e.mu.Lock()
		e.volumes[name] = info
		e.mu.Unlock()
		mounts = append(mounts, docker.MountPoint{Type: "volume", Source: info.Mountpoint, Destination: destination, RW: true, Name: name})
	}
	return mounts
}

// Frame encodes one multiplexed stream frame.
func Frame(channel byte, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = channel
	binary.BigEndian.PutUint32(frame[4:], uint32(len(data)))
	copy(frame[8:], data)
	return frame
}

func (e *Engine) SortedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := append([]string(nil), e.order...)
	sort.Strings(ids)
	return ids
}
