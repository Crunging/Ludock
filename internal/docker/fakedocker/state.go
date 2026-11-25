package fakedocker

import (
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
)

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

// CrashLoop shows a container as Docker does between the restarts of a
// game that keeps exiting under its restart policy, until it is stopped:
// still running to the Engine API, with the last run's start time. run
// shows it during one of the loop's brief runs instead.
func (e *Engine) CrashLoop(id string, run bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.containers[id]; c != nil {
		c.RestartCount++
		if run {
			c.State.Status, c.State.Running, c.State.Restarting = "running", true, false
			c.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
			// Docker checks a new run's health afresh.
			if c.State.Health != nil {
				c.State.Health = &docker.HealthState{Status: "starting"}
			}
			e.emitLocked("start", c.ID)
			return
		}
		c.State.Status, c.State.Running, c.State.Restarting, c.State.ExitCode = "restarting", true, true, 1
		e.emitLocked("die", c.ID)
	}
}

// exitLocked returns OnStop, to run once the lock is released.
func (e *Engine) exitLocked(c *Container, code int) func(*Container) {
	c.State.Status, c.State.Running, c.State.Restarting, c.State.ExitCode = "exited", false, false, code
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
		e.autoRemoveLocked(c.ID)
	}
	return e.OnStop
}

var anonymousVolume = regexp.MustCompile(`^[0-9a-f]{64}$`)

// autoRemoveLocked removes a --rm container as Docker does once it stops:
// its unnamed volumes go with it.
func (e *Engine) autoRemoveLocked(id string) {
	if c := e.containers[id]; c != nil {
		for _, mount := range c.Mounts {
			name := mount.Name
			if name == "" {
				name = path.Base(path.Dir(mount.Source))
			}
			if mount.Type == "volume" && anonymousVolume.MatchString(name) {
				delete(e.volumes, name)
			}
		}
	}
	e.removeLocked(id)
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

// Recreate applies the /fake/recreate control to a container and returns
// its new ID, or "" when there is no such container.
func (e *Engine) Recreate(reference string, query url.Values) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.find(reference)
	if c == nil {
		return ""
	}
	replacement := e.recreateLocked(c, query.Get("image"), query["env"], query["label"], query["mount"])
	if name := query.Get("name"); name != "" {
		e.containers[replacement].Name = "/" + name
	}
	return replacement
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

func (e *Engine) SortedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := append([]string(nil), e.order...)
	sort.Strings(ids)
	return ids
}
