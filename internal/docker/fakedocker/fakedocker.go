// Package fakedocker is an in-memory Docker Engine API on a unix socket. It
// implements the endpoints Ludock uses so tests and browser journeys can run
// the real binary without a daemon. Never point it at real game servers.
package fakedocker

import (
	"archive/tar"
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
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
	// pulls are the image IDs pulling a reference yields.
	pulls  map[string]string
	execs  map[string]*exec
	events []chan []byte
	// Exec handles exec commands; nil exits 0 without output.
	Exec ExecHandler
	// OnStart runs when a container starts, under no lock, e.g. to fill in
	// the logs a helper process would print.
	OnStart func(c *Container)
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
	// HangCreates makes the next N container creates make the container and
	// never answer, as a daemon whose response is lost can.
	HangCreates int
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

func Start(dir string) (*Engine, error) {
	socket := filepath.Join(dir, "docker.sock")
	os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	engine := &Engine{containers: map[string]*Container{}, volumes: map[string]docker.VolumeInspect{},
		images: map[string]string{}, imageConfigs: map[string]map[string]any{}, pulls: map[string]string{},
		execs: map[string]*exec{}, APIVersion: "1.47", listener: listener, Socket: socket}
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
	e.mu.Unlock()
	return e.server.Close()
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
		pulled, ok := e.pulls[reference]
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
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else {
			writeJSON(w, 200, map[string]any{"Id": id, "Config": config})
		}
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

// SetPull makes pulling reference yield the image ID id.
func (e *Engine) SetPull(reference, id string) {
	e.mu.Lock()
	e.pulls[reference] = id
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
	result["HostConfig"] = host
	if settings, ok := result["NetworkSettings"].(map[string]any); ok {
		settings["Networks"] = c.Networks
	}
	if c.ImageManifestDescriptor != nil {
		result["ImageManifestDescriptor"] = c.ImageManifestDescriptor
	}
	return result
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
				c.State.Health = &docker.HealthState{Status: "healthy"}
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
		c.State.Status, c.State.Running, c.State.FinishedAt, c.State.ExitCode = "exited", false, now, 0
		// Docker releases a stopped container's addresses.
		for name, endpoint := range c.Networks {
			endpoint.IPAddress, endpoint.MacAddress = "", ""
			c.Networks[name] = endpoint
		}
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
