// Package console runs game console and shell commands in managed
// containers, and streams container logs, with credentials redacted from
// everything a browser sees.
package console

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/netconsole"
)

const (
	MaxExecOutput      = 4 * 1024 * 1024
	execKillGrace      = 3
	execStreamGrace    = 5 * time.Second
	execCreateTimeout  = 5 * time.Second
	execInspectTimeout = 5 * time.Second
	stdinTimeout       = 10 * time.Second
)

// boundedExecScript keeps the watchdog inside the container, since Docker
// cannot signal an exec and closing its stream does not stop it. User input
// stays a positional argument to "$@", never shell source. Without a writable
// /tmp the command runs alone, bounded by timeout(1).
const boundedExecScript = `
control="$1"; deadline="$2"; grace="$3"; shift 3
ready="$control/ready"; cancel="$control/cancel"; reason="$control/reason"
umask 077
if ! mkdir "$control" 2>/dev/null; then exec "$@"; fi
cleanup() {
  rm -f "$ready" "$cancel" "$reason"
  rmdir "$control" 2>/dev/null || true
}
if ! : > "$ready"; then cleanup; exit 126; fi
if [ -e "$cancel" ]; then cleanup; exit 125; fi
"$@" &
child=$!
(
  remaining="$deadline"
  while [ "$remaining" -gt 0 ] && [ ! -e "$cancel" ]; do
    sleep 1
    remaining=$((remaining - 1))
  done
  if [ -e "$cancel" ]; then
    printf '%s\n' cancelled > "$reason"
  else
    printf '%s\n' timeout > "$reason"
  fi
  kill -TERM "$child" 2>/dev/null || exit 0
  sleep "$grace"
  kill -KILL "$child" 2>/dev/null || true
) &
watchdog=$!
wait "$child"
status=$?
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
cause=""
if [ -r "$reason" ]; then IFS= read -r cause < "$reason" || true; fi
cleanup
if [ "$cause" = timeout ]; then exit 124; fi
if [ "$cause" = cancelled ]; then exit 125; fi
exit "$status"
`

const cancelScript = `remaining=5; while [ "$remaining" -gt 0 ] && [ ! -r "$1/ready" ]; do sleep 1; remaining=$((remaining - 1)); done; if [ -r "$1/ready" ]; then umask 077; : > "$1/cancel"; fi`

// Output receives command output. Complete flushes redaction tails only
// after the whole stream was received.
type Output struct {
	Stdout, Stderr, System func(string)
	Complete               func()
}

func (o Output) stdout(value string) {
	if o.Stdout != nil {
		o.Stdout(value)
	}
}

func (o Output) system(value string) {
	if o.System != nil {
		o.System(value)
	}
}

func (o Output) complete() {
	if o.Complete != nil {
		o.Complete()
	}
}

var Discard = Output{}

type Target struct {
	ContainerID string
	Labels      map[string]string // approved configuration labels
	State       string
}

type Runtime struct {
	Docker *docker.Client
	Images *launch.Images
	// pollInterval, startLimit, createLimit, and replyLimit are shortened in
	// tests.
	pollInterval time.Duration
	startLimit   time.Duration
	createLimit  time.Duration
	replyLimit   time.Duration
}

var (
	errCancelled  = errors.New("console command cancelled")
	errTimedOut   = errors.New("game console command timed out")
	errNotRunning = apperr.New("SERVER_NOT_RUNNING", 409, "The game server must be running to accept console commands")
)

// PasswordUnreadable is the code of a console password file Ludock can't
// read, which a game that writes it as it starts may yet write.
const PasswordUnreadable = "CONSOLE_PASSWORD_UNREADABLE"

// notConfigured explains a console setting the owner must fix. Its message
// is shown in the console, so it never includes a label or environment value.
func notConfigured(message string) error {
	return apperr.New("CONSOLE_NOT_CONFIGURED", 409, message)
}

func IsTimeout(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "timed out")
}

// Shell runs a shell command, bounded by timeout(1) inside the container.
func (r *Runtime) Shell(ctx context.Context, containerID, command string, out Output, assertAccess func() error) error {
	return r.exec(ctx, containerID, docker.ExecCreate{Cmd: []string{"timeout", "-k", "5", "60", "/bin/sh", "-c", command},
		AttachStdout: true, AttachStderr: true}, out, assertAccess, 65)
}

// GameCommand sends one command through the server's console adapter.
func (r *Runtime) GameCommand(ctx context.Context, target Target, adapter *games.Adapter, command string, out Output, assertAccess func() error) error {
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	if target.State != "running" {
		return errNotRunning
	}
	command = adapter.Command(command)
	switch adapter.Transport {
	case games.TransportContainerStdin:
		if err := r.stdin(ctx, target.ContainerID, command, out, assertAccess); err != nil {
			return err
		}
	default:
		response, err := r.query(ctx, target, adapter, command, assertAccess)
		if err != nil {
			return err
		}
		if response == "" {
			response = "Command completed with no response"
		}
		out.stdout(response)
	}
	out.complete()
	return nil
}

func (r *Runtime) query(ctx context.Context, target Target, adapter *games.Adapter, command string, assertAccess func() error) (string, error) {
	env, err := r.environment(ctx, target.ContainerID)
	if err != nil {
		return "", err
	}
	host, port, password, err := r.networkTarget(ctx, target, adapter, env)
	if err != nil {
		return "", err
	}
	transport := adapter.Transport
	if announce := adapter.Announce; announce != nil && announce.On(env) {
		if message, ok := strings.CutPrefix(command, announce.Prefix); ok {
			var variable string
			if port, variable = announce.PortFor(env); port == 0 {
				return "", notConfigured(variable + " isn’t a port (1-65535)")
			}
			// The REST API listens in the game's own network, whatever host
			// the console's label names.
			transport, command, host = announce.Transport, message, ""
		}
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return "", err
		}
	}
	if ctx.Err() != nil {
		return "", errCancelled
	}
	var response string
	if host != "" {
		response, err = netconsole.Execute(ctx, string(transport), host, port, password, command, netconsole.Control{AssertAccess: assertAccess})
	} else {
		var reply helperReply
		reply, err = r.networkHelper(ctx, target.ContainerID, helper.NetworkConsoleRequest{Transport: string(transport),
			Port: port, Password: password, Command: command}, assertAccess)
		if err == nil && reply.Response == nil {
			err = errors.New("game console command failed")
		}
		if err == nil {
			response = *reply.Response
		}
	}
	if err != nil {
		return "", err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return "", err
		}
	}
	if ctx.Err() != nil {
		return "", errCancelled
	}
	// A password read from a file is not among the server's redacted
	// environment values.
	return strings.ReplaceAll(response, password, "[redacted]"), nil
}

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostChars = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]-]+$`)
	badHost   = regexp.MustCompile(`[/\\\s@?#]`)
)

func (r *Runtime) environment(ctx context.Context, containerID string) (map[string]string, error) {
	info, err := r.Docker.Inspect(ctx, containerID)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, entry := range info.Config.Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	return env, nil
}

func (r *Runtime) networkTarget(ctx context.Context, target Target, adapter *games.Adapter, env map[string]string) (string, int, string, error) {
	var err error
	raw := target.Labels[games.LabelConsolePort]
	if raw == "" {
		raw = env["RCON_PORT"]
	}
	port := adapter.DefaultPort
	if raw != "" {
		port, err = strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			port = 0
		}
	}
	if port < 1 || port > 65535 {
		return "", 0, "", notConfigured("Set " + games.LabelConsolePort + " to the container's console port (1-65535)")
	}
	password, err := r.password(ctx, target, adapter, env)
	if err != nil {
		return "", 0, "", err
	}
	host := strings.TrimSpace(target.Labels[games.LabelConsoleHost])
	if host != "" && (len(host) > 253 || strings.Contains(host, "://") || badHost.MatchString(host) || !hostChars.MatchString(host)) {
		return "", 0, "", notConfigured(games.LabelConsoleHost + " must be an IP address or plain hostname")
	}
	return host, port, password, nil
}

// password is the console password: from the variable the label names,
// else the first candidate variable that is set, else the adapter's file.
func (r *Runtime) password(ctx context.Context, target Target, adapter *games.Adapter, env map[string]string) (string, error) {
	name := target.Labels[games.LabelConsolePasswordEnv]
	if name != "" && !envName.MatchString(name) {
		return "", notConfigured(games.LabelConsolePasswordEnv + " is not a valid environment variable name")
	}
	for _, candidate := range adapter.PasswordEnvCandidates {
		if name == "" && env[candidate] != "" {
			name = candidate
		}
	}
	if name == "" && adapter.PasswordFile != "" {
		return r.passwordFile(ctx, target.ContainerID, adapter.PasswordFile, adapter.PasswordKey)
	}
	if name == "" {
		return "", notConfigured("Set " + games.LabelConsolePasswordEnv + " to the name of the container environment variable that holds its console password")
	}
	if env[name] == "" {
		return "", notConfigured("The console password variable is empty or not set in the container")
	}
	return env[name], nil
}

// passwordFile reads the password from a small file in the container: the
// rest of the line that starts with key, or the first line without one.
func (r *Runtime) passwordFile(ctx context.Context, containerID, file, key string) (string, error) {
	unavailable := apperr.New(PasswordUnreadable, 409, "Ludock could not read the console password from "+file+
		". Check that the game's console is on, or set "+games.LabelConsolePasswordEnv)
	data, err := r.Docker.ReadFile(ctx, containerID, file, 4096)
	if docker.Unavailable(err) {
		return "", err
	}
	if err != nil {
		return "", unavailable
	}
	for _, line := range strings.Split(string(data), "\n") {
		if password, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), key); ok && password != "" {
			return password, nil
		}
		if key == "" {
			break
		}
	}
	return "", unavailable
}

type exitInfo = docker.ExecInspect

func (r *Runtime) interval() time.Duration {
	if r.pollInterval > 0 {
		return r.pollInterval
	}
	return 250 * time.Millisecond
}

// waitForExit retains the caller's lock until Docker confirms the exec
// ended. Stream EOF does not prove it. A missing record (the container or
// daemon was replaced) returns nil.
func (r *Runtime) waitForExit(execID string, requestCancellation func()) *exitInfo {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), execInspectTimeout)
		result, err := r.Docker.InspectExec(ctx, execID)
		cancel()
		// A prepared exec is also not running, but has no exit code yet.
		if err == nil && !result.Running && result.ExitCode != nil {
			return result
		}
		if docker.IsNotFound(err) {
			return nil
		}
		requestCancellation()
		time.Sleep(r.interval())
	}
}

// utf8Stream decodes chunks without splitting runes.
type utf8Stream struct{ carry []byte }

func (u *utf8Stream) decode(chunk []byte) string {
	data := append(u.carry, chunk...)
	cut := len(data)
	for back := 1; back <= 3 && back <= len(data); back++ {
		b := data[len(data)-back]
		if utf8.RuneStart(b) {
			if !utf8.FullRune(data[len(data)-back:]) {
				cut = len(data) - back
			}
			break
		}
	}
	u.carry = append([]byte(nil), data[cut:]...)
	return strings.ToValidUTF8(string(data[:cut]), "�")
}

func (u *utf8Stream) flush() string {
	rest := strings.ToValidUTF8(string(u.carry), "�")
	u.carry = nil
	return rest
}

type funcWriter func([]byte) error

func (f funcWriter) Write(p []byte) (int, error) {
	if err := f(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

var errLimited = errors.New("game console output exceeded its limit")

func (r *Runtime) exec(ctx context.Context, containerID string, options docker.ExecCreate, out Output, assertAccess func() error,
	timeoutSeconds int) error {
	if len(options.Cmd) == 0 {
		return errors.New("the console adapter is missing its command configuration")
	}
	if ctx.Err() != nil {
		return errCancelled
	}
	control := "/tmp/.ludock-console-" + identity.NewUUID()
	user := options.User
	options.Cmd = append([]string{"/bin/sh", "-c", boundedExecScript, "ludock-console", control, strconv.Itoa(timeoutSeconds),
		strconv.Itoa(execKillGrace)}, options.Cmd...)
	createCtx, cancelCreate := context.WithTimeout(ctx, execCreateTimeout)
	execID, err := r.Docker.CreateExec(createCtx, containerID, options)
	cancelCreate()
	if err != nil {
		if ctx.Err() != nil {
			return errCancelled
		}
		if createCtx.Err() != nil {
			return errors.New("game console preparation timed out")
		}
		return err
	}
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return errCancelled
	}
	var cancelMu sync.Mutex
	cancelling := false
	requestCancellation := func() {
		cancelMu.Lock()
		if cancelling {
			cancelMu.Unlock()
			return
		}
		cancelling = true
		cancelMu.Unlock()
		go func() {
			if r.cancel(containerID, control, user) != nil {
				cancelMu.Lock()
				cancelling = false
				cancelMu.Unlock()
			}
		}()
	}
	// Starting is a mutation: a lost response cannot prove Docker rejected it,
	// so the start ignores the caller's cancellation. It still has its own
	// deadline, as Docker can accept it and never answer; a start that times
	// out is reconciled below as already dispatched.
	limit := time.Duration(timeoutSeconds+execKillGrace)*time.Second + execStreamGrace
	if r.startLimit > 0 {
		limit = r.startLimit
	}
	start, cancelStart := context.WithTimeout(context.WithoutCancel(ctx), limit)
	conn, err := r.Docker.StartExec(start, execID)
	cancelStart()
	if err != nil {
		var open *docker.StreamOpenError
		if docker.StatusCode(err) != 0 || (errors.As(err, &open) && !open.RequestStarted) {
			return err
		}
		requestCancellation()
		r.waitForExit(execID, requestCancellation)
		return err
	}
	defer conn.Close()
	var stateMu sync.Mutex
	cancelled, forced, limited := false, false, false
	stopAbort := context.AfterFunc(ctx, func() {
		stateMu.Lock()
		cancelled = true
		stateMu.Unlock()
		requestCancellation()
	})
	defer stopAbort()
	hard := time.AfterFunc(time.Duration(timeoutSeconds+execKillGrace)*time.Second+execStreamGrace, func() {
		stateMu.Lock()
		forced = true
		stateMu.Unlock()
		requestCancellation()
		conn.Close()
	})
	received := 0
	decoders := map[string]*utf8Stream{"stdout": {}, "stderr": {}}
	deliver := func(kind string, chunk []byte) error {
		stateMu.Lock()
		if limited {
			stateMu.Unlock()
			return nil
		}
		received += len(chunk)
		if received > MaxExecOutput {
			limited = true
			stateMu.Unlock()
			requestCancellation()
			return nil
		}
		stop := cancelled
		stateMu.Unlock()
		if assertAccess != nil && assertAccess() != nil {
			stateMu.Lock()
			cancelled = true
			stateMu.Unlock()
			requestCancellation()
			return nil
		}
		if stop {
			return nil
		}
		if value := decoders[kind].decode(chunk); value != "" {
			if kind == "stdout" {
				out.stdout(value)
			} else if out.Stderr != nil {
				out.Stderr(value)
			}
		}
		return nil
	}
	streamErr := docker.Demux(conn, funcWriter(func(p []byte) error { return deliver("stdout", p) }),
		funcWriter(func(p []byte) error { return deliver("stderr", p) }))
	hard.Stop()
	if streamErr != nil {
		requestCancellation()
	}
	stateMu.Lock()
	if !limited && !cancelled && streamErr == nil {
		if rest := decoders["stdout"].flush(); rest != "" {
			out.stdout(rest)
		}
		if rest := decoders["stderr"].flush(); rest != "" && out.Stderr != nil {
			out.Stderr(rest)
		}
		out.complete()
	}
	stateMu.Unlock()
	result := r.waitForExit(execID, requestCancellation)
	stateMu.Lock()
	defer stateMu.Unlock()
	switch {
	case limited:
		return errLimited
	case cancelled:
		return errCancelled
	case forced:
		return errTimedOut
	case streamErr != nil:
		return errors.New("game console stream failed")
	case result == nil:
		return errors.New("game console process is no longer available")
	case *result.ExitCode == 124:
		return errTimedOut
	case *result.ExitCode == 125:
		return errCancelled
	case result.Running:
		return errors.New("game console command failed")
	case *result.ExitCode != 0:
		return &ExitError{Code: *result.ExitCode}
	}
	return nil
}

// ExitError is a command that ran to its end and exited with a non-zero
// status, such as grep finding nothing.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return "console command failed with exit code " + strconv.Itoa(e.Code)
}

func (r *Runtime) cancel(containerID, control, user string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	execID, err := r.Docker.CreateExec(ctx, containerID, docker.ExecCreate{Cmd: []string{"/bin/sh", "-c", cancelScript,
		"ludock-console-cancel", control}, User: user})
	if err != nil {
		return err
	}
	conn, err := r.Docker.StartExec(ctx, execID)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	return docker.Demux(conn, nil, nil)
}

func (r *Runtime) stdin(ctx context.Context, containerID, command string, out Output, assertAccess func() error) error {
	ctx, cancel := context.WithTimeout(ctx, stdinTimeout)
	defer cancel()
	ready := func() error {
		if ctx.Err() == context.DeadlineExceeded {
			return errTimedOut
		}
		if assertAccess != nil {
			return assertAccess()
		}
		return nil
	}
	info, err := r.Docker.Inspect(ctx, containerID)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errTimedOut
		}
		return err
	}
	if err := ready(); err != nil {
		return err
	}
	if !info.Config.OpenStdin {
		return notConfigured("This server’s console input is closed. Add stdin_open: true where the container is defined and recreate it.")
	}
	if info.Config.StdinOnce {
		return notConfigured("This server’s console input closes after one connection. Turn off StdinOnce where the container is defined and recreate it.")
	}
	conn, err := r.Docker.Attach(ctx, containerID, docker.AttachOptions{Stream: true, Stdin: true})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errTimedOut
		}
		return err
	}
	defer conn.Close()
	if err := ready(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write([]byte(command + "\n")); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return errTimedOut
		}
		return errors.New("game console command failed")
	}
	out.system("Command sent to the server process")
	return nil
}

const (
	stdinReplyTimeout = 5 * time.Second
	maxStdinReply     = 256 * 1024
)

var errNoReply = errors.New("the game printed no reply to the command")

// stdinReply types command into the game's standard input and returns what
// the game logged after it, once complete says that holds the reply. The log
// is followed before the command is sent, so the reply can't be missed; other
// lines the game logs meanwhile come with it.
func (r *Runtime) stdinReply(ctx context.Context, containerID, command string, complete func(string) bool,
	assertAccess func() error) (string, error) {
	limit := stdinReplyTimeout
	if r.replyLimit > 0 {
		limit = r.replyLimit
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	stream, err := r.Docker.Logs(ctx, containerID, docker.LogsOptions{Follow: true, Stdout: true, Stderr: true, Tail: 0})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var text strings.Builder
	decoder := NewLogDecoder(func(_, value string) { text.WriteString(value) })
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buffer := make([]byte, 32*1024)
			n, err := stream.Read(buffer)
			if n > 0 {
				select {
				case chunks <- buffer[:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	if err := r.stdin(ctx, containerID, command, Discard, assertAccess); err != nil {
		return "", err
	}
	for {
		select {
		case chunk, open := <-chunks:
			if !open {
				return "", errNoReply
			}
			decoder.Push(chunk)
			if text.Len() > maxStdinReply {
				return "", errLimited
			}
			if complete(text.String()) {
				if assertAccess != nil {
					if err := assertAccess(); err != nil {
						return "", err
					}
				}
				return text.String(), nil
			}
		case <-ctx.Done():
			return "", errNoReply
		}
	}
}

// networkHelper runs a network console command inside the game's network
// namespace, for consoles that listen only on the container's loopback.
// The helper is removed before returning, even after timeouts or revocation,
// because the caller holds the server lock until Docker confirms it.
type helperReply struct {
	Response *string
	Status   *helper.StatusResult
}

// RemoveLeftovers removes console helpers left by an earlier process: one
// may still be sending a command, and none holds its server's lock. One
// Ludock runs per Docker host, so at startup every one is left over.
func (r *Runtime) RemoveLeftovers(ctx context.Context) error {
	helpers, err := r.Docker.ListContainers(ctx, true, docker.Filters{"label": {"ludock.internal=console-helper"}})
	if err != nil {
		return err
	}
	for _, candidate := range helpers {
		if candidate.Labels["ludock.internal"] != "console-helper" {
			continue
		}
		if err := r.Docker.RemoveHelper(ctx, candidate.ID); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) networkHelper(ctx context.Context, containerID string, request helper.NetworkConsoleRequest, assertAccess func() error) (helperReply, error) {
	check := func() error {
		if ctx.Err() != nil {
			return errCancelled
		}
		if assertAccess != nil {
			if err := assertAccess(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := check(); err != nil {
		return helperReply{}, err
	}
	if len(request.Password)+len(request.Command) > netconsole.MaxBytes {
		return helperReply{}, errors.New("Console command or credential is too large")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if assertAccess != nil && assertAccess() != nil {
					cancel(errors.New("Console access changed"))
					return
				}
			}
		}
	}()
	preparation := time.AfterFunc(5*time.Second, func() { cancel(errors.New("game console preparation timed out")) })
	defer preparation.Stop()
	failure := func(err error) error {
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		if ctx.Err() != nil {
			return errCancelled
		}
		return err
	}
	image, err := r.Images.Resolve(ctx)
	if err != nil {
		return helperReply{}, failure(err)
	}
	name := "ludock-console-" + identity.NewUUID()
	// Never race create/start: wait for the outcome, then remove even a
	// late-created helper. The wait has its own bound, since the server lock is
	// held until then.
	limit := 2 * time.Minute
	if r.createLimit > 0 {
		limit = r.createLimit
	}
	create, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancelCreate()
	created, err := r.Docker.CreateHelper(create, docker.ContainerCreate{Name: name, Image: image,
		Entrypoint: []string{helper.Binary}, Cmd: []string{"helper", "network-console"}, User: "65534:65534",
		OpenStdin: true, StdinOnce: true, Env: launch.HelperEnv,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "console-helper"},
		HostConfig: &docker.HostConfig{NetworkMode: "container:" + containerID, ReadonlyRootfs: true, AutoRemove: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, PidsLimit: 64, Memory: 256 * 1024 * 1024,
			NanoCpus: 1_000_000_000, LogConfig: launch.NoLogs}})
	helperRef := created
	if helperRef == "" {
		// A lost create response may still have created the named helper.
		helperRef = name
	}
	defer func() {
		for r.Docker.RemoveHelper(context.Background(), helperRef) != nil {
			time.Sleep(time.Second)
		}
	}()
	if err != nil {
		return helperReply{}, failure(err)
	}
	if err := check(); err != nil {
		return helperReply{}, failure(err)
	}
	conn, err := r.Docker.Attach(ctx, helperRef, docker.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return helperReply{}, failure(err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := r.Docker.Start(ctx, helperRef); err != nil {
		return helperReply{}, failure(err)
	}
	if err := check(); err != nil {
		return helperReply{}, failure(err)
	}
	preparation.Stop()
	commandTimer := time.AfterFunc(10*time.Second, func() { cancel(errTimedOut) })
	defer commandTimer.Stop()
	encoded, _ := json.Marshal(request)
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return helperReply{}, failure(errors.New("game console command failed"))
	}
	stdout := docker.Stdout(conn)
	reader := bufio.NewReaderSize(stdout, 64*1024)
	maxWire := 6*(netconsole.MaxBytes+65536) + 65536
	total := 0
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		total += len(chunk)
		if total > maxWire {
			return helperReply{}, failure(errLimited)
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return helperReply{}, failure(errors.New("game console helper ended without a response"))
		}
		var record struct {
			Type     string               `json:"type"`
			Response *string              `json:"response"`
			Status   *helper.StatusResult `json:"status"`
		}
		decodeErr := json.Unmarshal(line, &record)
		line = nil
		if decodeErr != nil {
			return helperReply{}, failure(errors.New("invalid console helper response"))
		}
		if err := check(); err != nil {
			return helperReply{}, failure(err)
		}
		switch {
		case record.Type == "authorize":
			if _, err := conn.Write([]byte("{\"type\":\"authorized\"}\n")); err != nil {
				return helperReply{}, failure(errors.New("game console command failed"))
			}
		case record.Type == "result" && (record.Response != nil || record.Status != nil):
			if record.Response != nil && len(*record.Response) > netconsole.MaxBytes+65536 {
				return helperReply{}, failure(errLimited)
			}
			return helperReply{Response: record.Response, Status: record.Status}, nil
		default:
			return helperReply{}, failure(errors.New("game console command failed"))
		}
	}
}
