package fakedocker

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
)

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
		// Docker's status, such as "Up 5 seconds (health: starting)", changes
		// with each run and health check.
		state, status := c.State.Status, c.State.Status+" "+c.State.StartedAt
		if c.State.Health != nil {
			status += " (health: " + c.State.Health.Status + ")"
		}
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
	// Docker creates from any local image by its ID.
	if !known && strings.HasPrefix(body.Image, "sha256:") && !e.manifestLocked(body.Image) {
		imageID, known = body.Image, true
	}
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
		c.State.Status, c.State.StartedAt = "created", "0001-01-01T00:00:00Z"
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
	// Answered after the lock is released, so it must not share the map a
	// later update writes.
	host = maps.Clone(host)
	if c.Host.Memory > 0 {
		host["Memory"] = c.Host.Memory
	}
	if c.Host.Privileged {
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
		// A start by hand resets the restart policy's count.
		c.State.Status, c.State.Running, c.State.StartedAt, c.RestartCount = "running", true, now, 0
		// Healthchecks pass at once.
		if check, _ := c.ExtraConfig["Healthcheck"].(map[string]any); check != nil {
			if test, _ := check["Test"].([]any); len(test) > 0 && test[0] != "NONE" {
				c.State.Health = &docker.HealthState{Status: "healthy", Log: []docker.HealthResult{{End: now}}}
			}
		}
		e.emitLocked("start", id)
		hook, run := e.OnStart, e.Starts[c.CreateRequest.Labels["ludock.internal"]]
		e.mu.Unlock()
		if run != nil {
			run(c)
		}
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
		// Docker stops a restarting container at once; its exit stays the
		// game's own.
		if !c.IgnoresStop || c.State.Restarting {
			code := 0
			if c.State.Restarting {
				code = c.State.ExitCode
			}
			hook := e.exitLocked(c, code)
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
		c.State.Status, c.State.Running, c.State.Restarting, c.State.StartedAt = "running", true, false, now
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
		mounts := slices.Clone(c.Mounts)
		e.mu.Unlock()
		// Other files are read through the container's bind mounts, never
		// outside the mounted folder.
		for _, mount := range mounts {
			if ok || mount.Type != "bind" {
				continue
			}
			relative, err := filepath.Rel(mount.Destination, r.URL.Query().Get("path"))
			if err != nil {
				continue
			}
			root, err := os.OpenRoot(mount.Source)
			if err != nil {
				continue
			}
			if data, err := root.ReadFile(relative); err == nil {
				content, ok = string(data), true
			}
			root.Close()
		}
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
				info := e.addVolume(newID())
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
		info := e.addVolume(newID())
		mounts = append(mounts, docker.MountPoint{Type: "volume", Source: info.Mountpoint, Destination: destination, RW: true, Name: info.Name})
	}
	return mounts
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

// addVolume creates a local volume, replacing any of the same name.
func (e *Engine) addVolume(name string) docker.VolumeInspect {
	info := docker.VolumeInspect{Name: name, Driver: "local", Mountpoint: "/var/lib/docker/volumes/" + name + "/_data"}
	e.mu.Lock()
	e.volumes[name] = info
	e.mu.Unlock()
	return info
}
