// Package fakedocker is an in-memory Docker Engine API on a unix socket. It
// implements the endpoints Ludock uses so tests and browser journeys can run
// the real binary without a daemon. Never point it at real game servers.
package fakedocker

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
)

// Container is a fake container.
type Container struct {
	docker.ContainerInspect
	// Logs are emitted on /logs as stdout lines.
	Logs []string
	// Stats is returned by /stats.
	Stats docker.ContainerStats
}

// ExecHandler runs a fake exec. It reads stdin and writes stdout/stderr and
// returns the exit code.
type ExecHandler func(container *Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int

// Engine is the fake daemon state.
type Engine struct {
	mu         sync.Mutex
	containers map[string]*Container
	order      []string
	volumes    map[string]docker.VolumeInspect
	images     map[string]string
	execs      map[string]*exec
	events     []chan []byte
	// Exec handles exec commands; nil exits 0 without output.
	Exec ExecHandler
	// APIVersion is reported by /version.
	APIVersion string
	// Fail makes every API request return this status when non-zero.
	Fail int
	// Requests records "METHOD path" for each request.
	Requests []string

	listener net.Listener
	server   *http.Server
	Socket   string
}

type exec struct {
	id        string
	container string
	options   docker.ExecCreate
	running   bool
	exitCode  *int
}

// Start serves a new engine on a socket in dir.
func Start(dir string) (*Engine, error) {
	socket := filepath.Join(dir, "docker.sock")
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	engine := &Engine{containers: map[string]*Container{}, volumes: map[string]docker.VolumeInspect{},
		images: map[string]string{}, execs: map[string]*exec{}, APIVersion: "1.47", listener: listener, Socket: socket}
	engine.server = &http.Server{Handler: http.HandlerFunc(engine.serve)}
	go engine.server.Serve(listener)
	return engine, nil
}

// Close stops the engine.
func (e *Engine) Close() error {
	e.mu.Lock()
	for _, events := range e.events {
		close(events)
	}
	e.events = nil
	e.mu.Unlock()
	return e.server.Close()
}

var idCounter atomic.Int64

func newID() string {
	return fmt.Sprintf("%064x", time.Now().UnixNano()+idCounter.Add(1))
}

// GameOptions describes a fake game container.
type GameOptions struct {
	ID, Name, Image string
	Labels          map[string]string
	Env             []string
	Running         bool
	Mounts          []docker.MountPoint
	Ports           map[string][]docker.PortBinding
}

// AddContainer adds a container and returns its ID.
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
		Config: docker.ContainerConfig{Image: options.Image, Labels: labels, Env: options.Env},
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

// RemoveContainer deletes a container.
func (e *Engine) RemoveContainer(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.removeLocked(id)
}

func (e *Engine) removeLocked(id string) {
	delete(e.containers, id)
	for index, existing := range e.order {
		if existing == id {
			e.order = append(e.order[:index], e.order[index+1:]...)
			break
		}
	}
	e.emitLocked("destroy", id)
}

func (e *Engine) emitLocked(action, id string) {
	event, _ := json.Marshal(map[string]any{"Type": "container", "Action": action, "Actor": map[string]string{"ID": id},
		"id": id, "time": time.Now().Unix()})
	event = append(event, '\n')
	for _, events := range e.events {
		select {
		case events <- event:
		default:
		}
	}
}

var (
	versioned = regexp.MustCompile(`^/v1\.\d+(/.*)$`)
	container = regexp.MustCompile(`^/containers/([^/]+)(/[a-z]+)?$`)
	execPath  = regexp.MustCompile(`^/exec/([^/]+)/(start|json)$`)
	volume    = regexp.MustCompile(`^/volumes/([^/]+)$`)
	image     = regexp.MustCompile(`^/images/(.+)/json$`)
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]string{"message": "fake docker error"})
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.Requests = append(e.Requests, r.Method+" "+r.URL.Path)
	failure := e.Fail
	e.mu.Unlock()
	if failure != 0 {
		fail(w, failure)
		return
	}
	path := r.URL.Path
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
	case path == "/volumes/create" && r.Method == "POST":
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		info := docker.VolumeInspect{Name: body.Name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + body.Name + "/_data"}
		e.volumes[body.Name] = info
		e.mu.Unlock()
		writeJSON(w, 201, info)
	case path == "/images/create" && r.Method == "POST":
		e.mu.Lock()
		e.images[r.URL.Query().Get("fromImage")] = "sha256:" + strings.Repeat("1", 64)
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
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else {
			writeJSON(w, 200, map[string]string{"Id": id})
		}
	default:
		fail(w, 404)
	}
}

// AddImage registers an image reference.
func (e *Engine) AddImage(reference string) {
	e.mu.Lock()
	e.images[reference] = "sha256:" + strings.Repeat("2", 64)
	e.mu.Unlock()
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
		result = append(result, docker.ContainerSummary{ID: id, Names: []string{c.Name}, Image: c.Config.Image,
			Labels: c.Config.Labels, State: &state, Status: &status})
	}
	writeJSON(w, 200, result)
}

func (e *Engine) create(w http.ResponseWriter, r *http.Request) {
	var body docker.ContainerCreate
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		fail(w, 400)
		return
	}
	e.mu.Lock()
	_, known := e.images[body.Image]
	e.mu.Unlock()
	if !known {
		fail(w, 404)
		return
	}
	id := e.AddContainer(GameOptions{Name: r.URL.Query().Get("name"), Image: body.Image, Labels: body.Labels, Env: body.Env})
	e.Update(id, func(c *Container) {
		c.Config.OpenStdin, c.Config.StdinOnce, c.Config.Tty = body.OpenStdin, body.StdinOnce, body.Tty
	})
	writeJSON(w, 201, map[string]string{"Id": id})
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
		info := c.ContainerInspect
		e.mu.Unlock()
		writeJSON(w, 200, info)
	case action == "/start" && r.Method == "POST":
		if c.State.Running {
			e.mu.Unlock()
			w.WriteHeader(304)
			return
		}
		c.State.Status, c.State.Running, c.State.StartedAt = "running", true, now
		e.emitLocked("start", id)
		e.mu.Unlock()
		w.WriteHeader(204)
	case action == "/stop" && r.Method == "POST":
		if !c.State.Running {
			e.mu.Unlock()
			w.WriteHeader(304)
			return
		}
		c.State.Status, c.State.Running, c.State.FinishedAt, c.State.ExitCode = "exited", false, now, 0
		e.emitLocked("stop", id)
		e.emitLocked("die", id)
		e.mu.Unlock()
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
		w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
		w.WriteHeader(200)
		for _, line := range lines {
			if r.URL.Query().Get("timestamps") == "true" {
				line = now + " " + line
			}
			w.Write(Frame(1, []byte(line+"\n")))
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if r.URL.Query().Get("follow") == "true" {
			<-r.Context().Done()
		}
	case action == "/exec" && r.Method == "POST":
		var options docker.ExecCreate
		json.NewDecoder(r.Body).Decode(&options)
		execID := newID()
		e.execs[execID] = &exec{id: execID, container: id, options: options}
		e.mu.Unlock()
		writeJSON(w, 201, map[string]string{"Id": execID})
	case action == "/attach" && r.Method == "POST":
		e.mu.Unlock()
		e.hijack(w, r, func(conn net.Conn, reader *bufio.Reader) {
			<-r.Context().Done()
		})
	case action == "" && r.Method == "DELETE":
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
	defer conn.Close()
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
		e.mu.Unlock()
		writeJSON(w, 200, info)
		return
	}
	e.mu.Lock()
	instance.running = true
	handler := e.Exec
	e.mu.Unlock()
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
		instance.running = false
		instance.exitCode = &code
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

// Frame encodes one multiplexed stream frame.
func Frame(channel byte, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = channel
	binary.BigEndian.PutUint32(frame[4:], uint32(len(data)))
	copy(frame[8:], data)
	return frame
}

// SortedIDs lists container IDs in a stable order, for tests.
func (e *Engine) SortedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := append([]string(nil), e.order...)
	sort.Strings(ids)
	return ids
}
