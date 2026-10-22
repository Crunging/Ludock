package compose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// Project locates a Compose project's sources.
type Project struct {
	ProjectName      string   `json:"projectName"`
	ProjectDirectory string   `json:"projectDirectory"`
	ComposeFiles     []string `json:"composeFiles"`
	EnvFiles         []string `json:"envFiles"`
}

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func (p Project) valid() bool {
	if !projectName.MatchString(p.ProjectName) || len(p.ProjectName) > 128 || p.ProjectDirectory == "" ||
		len(p.ProjectDirectory) > 1024 || len(p.ComposeFiles) < 1 || len(p.ComposeFiles) > 16 || len(p.EnvFiles) > 16 {
		return false
	}
	for _, file := range append(slices.Clone(p.ComposeFiles), p.EnvFiles...) {
		if file == "" || len(file) > 1024 {
			return false
		}
	}
	return true
}

// Source is a discovered project and whether its default .env applies.
type Source struct {
	Project        Project `json:"project"`
	LoadDefaultEnv bool    `json:"loadDefaultEnv"`
}

var errSourceUnavailable = apperr.New("COMPOSE_SOURCE_UNAVAILABLE", 409,
	"Docker has no usable Compose source paths for this server. Recreate it through its owning manager to restore that metadata.")

// DiscoverSource reads a project's sources from container labels. Like all
// labels this is untrusted input, never permission to read a path.
func DiscoverSource(name string, labels map[string]string) (Source, error) {
	var source Source
	if saved, ok := labels[SourceLabel]; ok {
		var parsed struct {
			Project        *Project `json:"project"`
			LoadDefaultEnv *bool    `json:"loadDefaultEnv"`
		}
		if json.Unmarshal([]byte(saved), &parsed) != nil || parsed.Project == nil || parsed.LoadDefaultEnv == nil {
			return Source{}, errSourceUnavailable
		}
		source = Source{Project: *parsed.Project, LoadDefaultEnv: *parsed.LoadDefaultEnv}
		if source.Project.EnvFiles == nil {
			source.Project.EnvFiles = []string{}
		}
	} else {
		environment := labels[EnvironmentFilesLbl]
		envFiles := []string{}
		if environment != "" {
			envFiles = strings.Split(environment, ",")
		}
		var files []string
		if value, ok := labels[ConfigFilesLabel]; ok {
			files = strings.Split(value, ",")
		}
		source = Source{Project: Project{ProjectName: name, ProjectDirectory: labels[WorkingDirLabel], ComposeFiles: files,
			EnvFiles: envFiles}, LoadDefaultEnv: environment == ""}
	}
	if !source.Project.valid() || source.Project.ProjectName != name {
		return Source{}, errSourceUnavailable
	}
	for _, file := range append(append([]string{source.Project.ProjectDirectory}, source.Project.ComposeFiles...), source.Project.EnvFiles...) {
		if !filepath.IsAbs(file) || strings.Contains(file, "\x00") {
			return Source{}, errSourceUnavailable
		}
	}
	return source, nil
}

// Runner runs `docker compose`.
type Runner struct {
	// Binary is "docker" unless tests override it.
	Binary string
	Socket string
	Config string // Docker client config folder for registry access
	Roots  string // approved source roots, ':'-separated
}

// NewRunner runs Compose against socket with approved source roots.
func NewRunner(socket, dockerConfig string, roots []string) *Runner {
	return &Runner{Binary: "docker", Socket: socket, Config: dockerConfig, Roots: strings.Join(roots, ":")}
}

func (r *Runner) environment() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	config := r.Config
	if config == "" {
		config = "/nonexistent"
	}
	return []string{"PATH=" + path, "HOME=/nonexistent", "LANG=C.UTF-8", "DOCKER_HOST=unix://" + r.Socket,
		"DOCKER_CONFIG=" + config, "COMPOSE_DISABLE_ENV_FILE=true", "COMPOSE_ANSI=never", "COMPOSE_PROGRESS=plain"}
}

type cappedBuffer struct {
	buffer  bytes.Buffer
	limited bool
	kill    func()
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buffer.Len()+len(p) > 8*1024*1024 {
		c.limited = true
		c.kill()
		return len(p), nil
	}
	return c.buffer.Write(p)
}

// Run runs `docker compose args…` with an argument vector, never a shell.
// Raw diagnostics and resolved configuration can contain secrets, so
// stderr is drained and discarded. The CLI and its plugin share a process
// group so a deadline cannot leave the plugin modifying the project.
func (r *Runner) Run(ctx context.Context, args []string, timeout time.Duration) (string, error) {
	binary := r.Binary
	if binary == "" {
		binary = "docker"
	}
	command := exec.Command(binary, append([]string{"compose"}, args...)...)
	command.Env = r.environment()
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signal := func(value syscall.Signal) {
		if command.Process != nil {
			if syscall.Kill(-command.Process.Pid, value) != nil {
				command.Process.Signal(value)
			}
		}
	}
	stdout := &cappedBuffer{kill: func() { signal(syscall.SIGKILL) }}
	command.Stdout = stdout
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return "", apperr.New("COMPOSE_UNAVAILABLE", 409, "Docker Compose could not be executed")
	}
	limited := false
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		limited = true
		signal(syscall.SIGTERM)
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			signal(syscall.SIGKILL)
			err = <-done
		}
	case <-ctx.Done():
		limited = true
		signal(syscall.SIGTERM)
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			signal(syscall.SIGKILL)
			err = <-done
		}
	}
	if limited || stdout.limited {
		return "", apperr.New("COMPOSE_FAILED", 409, "Docker Compose exceeded its execution limit")
	}
	if err != nil {
		return "", apperr.New("COMPOSE_FAILED", 409, "Docker Compose failed. Review the project through its owning manager.")
	}
	return stdout.buffer.String(), nil
}

// Available reports whether Compose updates can run here.
func (r *Runner) Available(ctx context.Context) bool {
	roots, err := ConfiguredRoots(r.Roots)
	if err != nil || len(roots) == 0 {
		return false
	}
	_, err = r.Run(ctx, []string{"version", "--short"}, 5*time.Second)
	return err == nil
}

// Snapshot is a private copy of a project's vetted inputs. Compose reads it
// for config, pull, and up; mutable sources are never reparsed between
// them, and source files are never edited.
type Snapshot struct {
	Directory   string
	ConfigPath  string
	Model       map[string]any
	Fingerprint string
	Project     Project
}

// Cleanup removes the snapshot.
func (s *Snapshot) Cleanup() { os.RemoveAll(s.Directory) }

// Args are the global flags that point Compose at the snapshot.
func (s *Snapshot) Args() []string {
	return []string{"--project-name", s.Project.ProjectName, "--project-directory", s.Project.ProjectDirectory,
		"--env-file", filepath.Join(s.Directory, "empty.env"), "-f", s.ConfigPath}
}

func unsupported(message string) error { return apperr.New("UNSUPPORTED_COMPOSE", 400, message) }

func object(value any, message string) (map[string]any, error) {
	result, ok := value.(map[string]any)
	if !ok {
		return nil, unsupported(message)
	}
	return result, nil
}

func refuse(model map[string]any, keys ...string) error {
	for _, key := range keys {
		if _, ok := model[key]; ok {
			return unsupported("Compose '" + key + "' is outside the supported update configuration")
		}
	}
	return nil
}

func inputPath(value any, base string) (string, error) {
	text, ok := value.(string)
	if !ok || text == "" || strings.Contains(text, "$") || strings.Contains(text, "://") {
		return "", unsupported("Compose file references must use literal local paths")
	}
	if filepath.IsAbs(text) {
		return text, nil
	}
	return filepath.Join(base, text), nil
}

var serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func writeHash(h hash.Hash, values ...any) {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			h.Write([]byte(typed))
		case []byte:
			h.Write(typed)
		}
	}
}

// Snapshots creates project snapshots.
type Snapshots struct {
	Runner *Runner
	DB     *db.DB
}

// Create materializes only vetted inputs.
func (s *Snapshots) Create(ctx context.Context, source Source) (*Snapshot, error) {
	project := source.Project
	if !project.valid() {
		return nil, errSourceUnavailable
	}
	roots, err := ConfiguredRoots(s.Runner.Roots)
	if err != nil {
		return nil, err
	}
	base, err := ApprovedPath(project.ProjectDirectory, roots)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "ludock-compose-")
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{Directory: directory, Project: project}
	ok := false
	defer func() {
		if !ok {
			snapshot.Cleanup()
		}
	}()
	h := sha256.New()
	header, _ := json.Marshal(source)
	h.Write(header)
	inputs := 0
	snapshotFile := func(original string, required bool) (string, error) {
		data, present, err := ReadApprovedFile(original, roots, !required)
		if err != nil {
			return "", err
		}
		record, _ := json.Marshal(map[string]any{"original": original, "present": present})
		writeHash(h, record, "\x00", data)
		filename := filepath.Join(directory, "input-"+strconv.Itoa(inputs))
		inputs++
		// A missing optional env_file contributes nothing. An empty private copy
		// preserves that without reopening the mutable source.
		return filename, os.WriteFile(filename, data, 0o600)
	}
	emptyEnv := filepath.Join(directory, "empty.env")
	if err := os.WriteFile(emptyEnv, nil, 0o600); err != nil {
		return nil, err
	}
	flags := []string{"--project-name", project.ProjectName, "--project-directory", base}
	for _, file := range project.EnvFiles {
		original, err := inputPath(file, base)
		if err != nil {
			return nil, err
		}
		copy, err := snapshotFile(original, true)
		if err != nil {
			return nil, err
		}
		flags = append(flags, "--env-file", copy)
	}
	if len(project.EnvFiles) == 0 {
		filename := filepath.Join(base, ".env")
		var data []byte
		present := false
		if source.LoadDefaultEnv {
			if data, present, err = ReadApprovedFile(filename, roots, true); err != nil {
				return nil, err
			}
		}
		if present {
			writeHash(h, filename, "\x00", data)
			copy := filepath.Join(directory, "default.env")
			if err := os.WriteFile(copy, data, 0o600); err != nil {
				return nil, err
			}
			flags = append(flags, "--env-file", copy)
		} else {
			flags = append(flags, "--env-file", emptyEnv)
		}
	}
	for _, original := range project.ComposeFiles {
		filename, err := inputPath(original, base)
		if err != nil {
			return nil, err
		}
		data, _, err := ReadApprovedFile(filename, roots, false)
		if err != nil {
			return nil, err
		}
		writeHash(h, filename, "\x00", data)
		model, err := ParseFile(data)
		if err != nil {
			return nil, err
		}
		if err := refuse(model, "include", "secrets", "configs", "models"); err != nil {
			return nil, err
		}
		services, err := object(model["services"], "Compose must declare services")
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			if !serviceName.MatchString(name) {
				return nil, apperr.New("INVALID_SERVICE", 400, "Invalid Compose service name")
			}
			service, err := object(services[name], "Unsupported Compose structure")
			if err != nil {
				return nil, err
			}
			if err := refuse(service, "extends", "label_file", "secrets", "configs", "credential_spec", "pre_start",
				"post_start", "pre_stop", "provider", "develop", "volumes_from"); err != nil {
				return nil, err
			}
			// Builds can read arbitrary contexts. Image+build services stay
			// usable by keeping only the pullable image definition.
			if _, ok := service["build"]; ok {
				if image, _ := service["image"].(string); image == "" {
					return nil, apperr.New("BUILD_ONLY", 400, "Build-only services cannot be updated by Ludock")
				}
				delete(service, "build")
			}
			if value, ok := service["env_file"]; ok {
				entries, isList := value.([]any)
				if !isList {
					entries = []any{value}
				}
				var copies []any
				// Fingerprints follow the declared order.
				for _, entry := range entries {
					record := map[string]any{}
					if text, ok := entry.(string); ok {
						record["path"] = text
					} else if mapped, ok := entry.(map[string]any); ok {
						for key, item := range mapped {
							if !slices.Contains([]string{"path", "required", "format"}, key) {
								return nil, unsupported("Unsupported environment-file option")
							}
							record[key] = item
						}
					} else {
						return nil, unsupported("Unsupported Compose structure")
					}
					original, err := inputPath(record["path"], base)
					if err != nil {
						return nil, err
					}
					copy, err := snapshotFile(original, record["required"] != false)
					if err != nil {
						return nil, err
					}
					record["path"] = copy
					copies = append(copies, record)
				}
				service["env_file"] = copies
			}
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, apperr.New("INVALID_COMPOSE", 400, "Compose YAML could not be parsed safely")
		}
		copy := filepath.Join(directory, "compose-"+strconv.Itoa(inputs)+".json")
		inputs++
		if err := os.WriteFile(copy, encoded, 0o600); err != nil {
			return nil, err
		}
		flags = append(flags, "-f", copy)
	}
	output, err := s.Runner.Run(ctx, append(flags, "config", "--format", "json"), 2*time.Minute)
	if err != nil {
		return nil, err
	}
	var model map[string]any
	if json.Unmarshal([]byte(output), &model) != nil || model == nil {
		return nil, apperr.New("INVALID_COMPOSE", 400, "Docker Compose returned an invalid configuration")
	}
	services, err := object(model["services"], "Unsupported Compose structure")
	if err != nil {
		return nil, err
	}
	// Keep original paths on replacements: Compose's own labels will point at
	// this disposable snapshot. Compose writes literal dollars as $$ in its
	// reusable output; escape this value the same way.
	files := make([]string, 0, len(project.ComposeFiles))
	for _, file := range project.ComposeFiles {
		path, _ := inputPath(file, base)
		files = append(files, path)
	}
	envFiles := []string{}
	for _, file := range project.EnvFiles {
		path, _ := inputPath(file, base)
		envFiles = append(envFiles, path)
	}
	label, _ := json.Marshal(Source{Project: Project{ProjectName: project.ProjectName, ProjectDirectory: project.ProjectDirectory,
		ComposeFiles: files, EnvFiles: envFiles}, LoadDefaultEnv: source.LoadDefaultEnv})
	escaped := strings.ReplaceAll(string(label), "$", "$$")
	for name, value := range services {
		service, err := object(value, "Unsupported Compose structure")
		if err != nil {
			return nil, err
		}
		labels, _ := service["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
		}
		labels[SourceLabel] = escaped
		service["labels"] = labels
		services[name] = service
	}
	resolved, _ := json.Marshal(model)
	snapshot.ConfigPath = filepath.Join(directory, "resolved.json")
	if err := os.WriteFile(snapshot.ConfigPath, resolved, 0o600); err != nil {
		return nil, err
	}
	snapshot.Model = model
	if snapshot.Fingerprint, err = s.DB.ComposeSourceFingerprint(hex.EncodeToString(h.Sum(nil))); err != nil {
		return nil, err
	}
	ok = true
	return snapshot, nil
}

// Selected is a validated project ready for an update.
type Selected struct {
	Snapshot *Snapshot
	Service  string
	Image    string
}

// Validate checks one replica and no namespace dependencies.
func ValidateService(service map[string]any) error {
	deploy := map[string]any{}
	if value, ok := service["deploy"]; ok {
		var err error
		if deploy, err = object(value, "Unsupported Compose structure"); err != nil {
			return err
		}
	}
	replicas := any(1.0)
	if value, ok := service["scale"]; ok {
		replicas = value
	} else if value, ok := deploy["replicas"]; ok {
		replicas = value
	}
	if number, ok := toNumber(replicas); !ok || number != 1 {
		return apperr.New("COMPOSE_REPLICAS", 409, "The selected service must have exactly one configured replica")
	}
	for _, key := range []string{"network_mode", "pid", "ipc"} {
		if text, ok := service[key].(string); ok && strings.HasPrefix(text, "service:") {
			return apperr.New("SERVICE_DEPENDENCY", 409, "Service namespace dependencies must be updated through their original manager")
		}
	}
	return nil
}

func toNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil
	}
	return 0, false
}

// Updates validates projects for updates.
type Updates struct {
	Snapshots *Snapshots
	Docker    *docker.Client
	Locks     *locks.Table
}

var errExternal = apperr.New("EXTERNAL_MANAGER", 409, "Update this server through its original container manager")

// Validated snapshots the server's project and checks its service.
func (u *Updates) Validated(ctx context.Context, server *servers.Context) (*Selected, error) {
	identity := server.Observation.Compose
	if identity == nil {
		return nil, errExternal
	}
	roots, err := ConfiguredRoots(u.Snapshots.Runner.Roots)
	if err != nil || len(roots) == 0 {
		return nil, apperr.New("COMPOSE_SOURCES_NOT_MOUNTED", 409,
			"Mount your Compose folder read-only into Ludock at the same absolute host path and set LUDOCK_COMPOSE_ROOTS to that folder. Then check again.")
	}
	source, err := DiscoverSource(identity.Project, server.Observation.ComposeSourceLabels)
	if err != nil {
		return nil, err
	}
	snapshot, err := u.Snapshots.Create(ctx, source)
	if err != nil {
		if apperr.Is(err, "UNAPPROVED_PATH") || apperr.Is(err, "UNSAFE_CONFIG_PATH") {
			code := "UNAPPROVED_PATH"
			if apperr.Is(err, "UNSAFE_CONFIG_PATH") {
				code = "UNSAFE_CONFIG_PATH"
			}
			return nil, apperr.New(code, 409, "Ludock cannot read this server’s Compose files. Mount the source folder read-only at its original absolute host path, include it in LUDOCK_COMPOSE_ROOTS, and check that all referenced files exist without symlinks.")
		}
		return nil, err
	}
	selected, err := u.validate(ctx, server, snapshot)
	if err != nil {
		snapshot.Cleanup()
		return nil, err
	}
	return selected, nil
}

func (u *Updates) validate(ctx context.Context, server *servers.Context, snapshot *Snapshot) (*Selected, error) {
	services, err := object(snapshot.Model["services"], "Unsupported Compose structure")
	if err != nil {
		return nil, err
	}
	name := server.Observation.Compose.Service
	service, err := object(services[name], "The selected service is absent from the Compose project")
	if err != nil {
		return nil, err
	}
	if err := ValidateService(service); err != nil {
		return nil, err
	}
	if err := u.AssertSingleContainer(ctx, server); err != nil {
		return nil, err
	}
	image, _ := service["image"].(string)
	if image == "" || strings.Contains(image, "@") {
		return nil, apperr.New("UNSUPPORTED_IMAGE", 409, "Updates require a pullable image tag, not a digest-pinned or build-only service")
	}
	if platform, ok := service["platform"]; ok {
		if _, isString := platform.(string); !isString {
			return nil, apperr.New("UNSUPPORTED_IMAGE", 409, "Invalid service platform")
		}
	}
	return &Selected{Snapshot: snapshot, Service: name, Image: image}, nil
}

// AssertSingleContainer requires exactly one non-one-off container for the
// service: the server's own.
func (u *Updates) AssertSingleContainer(ctx context.Context, server *servers.Context) error {
	identity := server.Observation.Compose
	if identity == nil {
		return errExternal
	}
	containers, err := u.Docker.ListContainers(ctx, true, docker.Filters{"label": {
		"com.docker.compose.project=" + identity.Project, "com.docker.compose.service=" + identity.Service}})
	if err != nil {
		return err
	}
	var normal []docker.ContainerSummary
	for _, container := range containers {
		if strings.ToLower(container.Labels["com.docker.compose.oneoff"]) != "true" {
			normal = append(normal, container)
		}
	}
	if len(normal) != 1 || normal[0].ID != server.Container.ID {
		return apperr.New("COMPOSE_REPLICAS", 409, "Updates require exactly one container for the selected Compose service")
	}
	return nil
}

// Capability describes whether a server can be updated here.
type Capability struct {
	UpdatesOnStart    bool
	Available         bool
	ActionLabel       string
	Manager           string // compose or external
	ProjectName       string
	ServiceName       string
	Image             string
	UnavailableReason string
}

// Capability reports update availability. busy reports active work.
func (u *Updates) Capability(ctx context.Context, server *servers.Context, allowCompose bool, busy func() bool) Capability {
	capability := Capability{ActionLabel: "Update server", Manager: "external"}
	if server.Container.GameType == "unknown" {
		capability.ActionLabel = "Update image"
	}
	if server.Observation.Compose != nil {
		capability.Manager = "compose"
	}
	if integration := games.ImageIntegration(server.Container.Image); integration != nil {
		capability.UpdatesOnStart = integration.UpdatesOnStart
	}
	fail := func(err error) Capability {
		if public, ok := apperr.As(err); ok {
			capability.UnavailableReason = public.Message
		} else {
			capability.UnavailableReason = "The Compose source is inaccessible or invalid. Check its read-only mount and LUDOCK_COMPOSE_ROOTS in Settings."
		}
		return capability
	}
	if !allowCompose {
		return fail(apperr.New("FORBIDDEN", 403, "Administrator permission is required to pull and recreate the image with Compose."))
	}
	if busy() {
		return fail(apperr.New("OPERATION_CONFLICT", 409, "Wait for the active operation to finish before updating"))
	}
	selected, err := u.Validated(ctx, server)
	if err != nil {
		return fail(err)
	}
	defer selected.Snapshot.Cleanup()
	capability.Available, capability.ProjectName, capability.ServiceName, capability.Image = true, selected.Snapshot.Project.ProjectName,
		selected.Service, selected.Image
	return capability
}
