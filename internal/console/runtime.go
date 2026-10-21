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

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/launch"
	"github.com/Crunging/Ludock/internal/netconsole"
)

// Limits.
const (
	MaxExecOutput      = 4 * 1024 * 1024
	execTimeoutSeconds = 15
	execKillGrace      = 3
	execStreamGrace    = 5 * time.Second
	execCreateTimeout  = 5 * time.Second
	execInspectTimeout = 5 * time.Second
	stdinTimeout       = 10 * time.Second
)

// boundedExecScript keeps the watchdog inside the container. Docker cannot
// signal an exec and closing its stream does not stop it, so the command
// really terminates even if Ludock disconnects. User input stays a
// positional argument to "$@" and is never shell source.
const boundedExecScript = `
control="$1"; deadline="$2"; grace="$3"; independent_timeout="$4"; shift 4
ready="$control/ready"; cancel="$control/cancel"; reason="$control/reason"
umask 077
if ! mkdir "$control" 2>/dev/null; then
  if [ "$independent_timeout" = 1 ]; then exec "$@"; fi
  exit 126
fi
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

// Discard drops all output.
var Discard = Output{}

// Target is the container a command runs in.
type Target struct {
	ContainerID string
	Labels      map[string]string // approved configuration labels
	State       string
}

// Runtime runs commands.
type Runtime struct {
	Docker *docker.Client
	Images *launch.Images
	// pollInterval is shortened in tests.
	pollInterval time.Duration
}

var (
	errCancelled = errors.New("Console command cancelled")
	errTimedOut  = errors.New("Game console command timed out")
)

// IsTimeout reports a console timeout.
func IsTimeout(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "timed out")
}

// Shell runs a shell command, bounded by timeout(1) inside the container.
func (r *Runtime) Shell(ctx context.Context, containerID, command string, out Output, assertAccess func() error) error {
	return r.exec(ctx, containerID, docker.ExecCreate{Cmd: []string{"timeout", "-k", "5", "60", "/bin/sh", "-c", command},
		AttachStdout: true, AttachStderr: true}, out, assertAccess, 65, true)
}

// GameCommand sends one command through the server's console adapter.
func (r *Runtime) GameCommand(ctx context.Context, target Target, adapter *games.Adapter, command string, out Output, assertAccess func() error) error {
	if assertAccess != nil {
		if err := assertAccess(); err != nil {
			return err
		}
	}
	if target.State != "running" {
		return errors.New("The game server must be running to accept console commands")
	}
	switch adapter.Transport {
	case games.TransportDockerExec:
		if err := r.exec(ctx, target.ContainerID, docker.ExecCreate{Cmd: games.MinecraftExec(command), AttachStdout: true, AttachStderr: true},
			out, assertAccess, execTimeoutSeconds, false); err != nil {
			return err
		}
		return nil
	case games.TransportContainerStdin:
		if err := r.stdin(ctx, target.ContainerID, command, out, assertAccess); err != nil {
			return err
		}
	default:
		host, port, password, err := r.networkTarget(ctx, target, adapter)
		if err != nil {
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
		var response string
		if host != "" {
			response, err = netconsole.Execute(ctx, string(adapter.Transport), host, port, password, command, netconsole.Control{AssertAccess: assertAccess})
		} else {
			response, err = r.networkHelper(ctx, target.ContainerID, helper.NetworkConsoleRequest{Transport: string(adapter.Transport),
				Port: port, Password: password, Command: command}, assertAccess)
		}
		if err != nil {
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
		if response == "" {
			response = "Command completed with no response"
		}
		out.stdout(response)
	}
	out.complete()
	return nil
}

var (
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hostChars = regexp.MustCompile(`^[A-Za-z0-9_.:\[\]-]+$`)
	badHost   = regexp.MustCompile(`[/\\\s@?#]`)
)

func (r *Runtime) networkTarget(ctx context.Context, target Target, adapter *games.Adapter) (string, int, string, error) {
	info, err := r.Docker.Inspect(ctx, target.ContainerID)
	if err != nil {
		return "", 0, "", err
	}
	env := map[string]string{}
	for _, entry := range info.Config.Env {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
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
		return "", 0, "", errors.New("Set " + games.LabelConsolePort + " to the container's console port (1-65535)")
	}
	name := target.Labels[games.LabelConsolePasswordEnv]
	if name != "" {
		if !envName.MatchString(name) {
			return "", 0, "", errors.New(games.LabelConsolePasswordEnv + " is not a valid environment name")
		}
	} else {
		for _, candidate := range adapter.PasswordEnvCandidates {
			if env[candidate] != "" {
				name = candidate
				break
			}
		}
		if name == "" {
			return "", 0, "", errors.New("Set " + games.LabelConsolePasswordEnv + " to the name of the container environment variable that holds its console password")
		}
	}
	password := env[name]
	if password == "" {
		return "", 0, "", errors.New("Console credential environment variable " + name + " is empty or unavailable")
	}
	host := strings.TrimSpace(target.Labels[games.LabelConsoleHost])
	if host != "" && (len(host) > 253 || strings.Contains(host, "://") || badHost.MatchString(host) || !hostChars.MatchString(host)) {
		return "", 0, "", errors.New(games.LabelConsoleHost + " must be an IP address or plain hostname")
	}
	return host, port, password, nil
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

var errLimited = errors.New("Game console output exceeded its limit")

func (r *Runtime) exec(ctx context.Context, containerID string, options docker.ExecCreate, out Output, assertAccess func() error,
	timeoutSeconds int, independentTimeout bool) error {
	if len(options.Cmd) == 0 {
		return errors.New("The console adapter is missing its command configuration")
	}
	if ctx.Err() != nil {
		return errCancelled
	}
	control := "/tmp/.ludock-console-" + identity.NewUUID()
	independent := "0"
	if independentTimeout {
		independent = "1"
	}
	user := options.User
	options.Cmd = append([]string{"/bin/sh", "-c", boundedExecScript, "ludock-console", control, strconv.Itoa(timeoutSeconds),
		strconv.Itoa(execKillGrace), independent}, options.Cmd...)
	createCtx, cancelCreate := context.WithTimeout(ctx, execCreateTimeout)
	execID, err := r.Docker.CreateExec(createCtx, containerID, options)
	cancelCreate()
	if err != nil {
		if ctx.Err() != nil {
			return errCancelled
		}
		if createCtx.Err() != nil {
			return errors.New("Game console preparation timed out")
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
	start, cancelStart := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(timeoutSeconds+execKillGrace)*time.Second+execStreamGrace)
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
		return errors.New("Game console stream failed")
	case result == nil:
		return errors.New("Game console process is no longer available")
	case *result.ExitCode == 124:
		return errTimedOut
	case *result.ExitCode == 125:
		return errCancelled
	case result.Running || *result.ExitCode != 0:
		return errors.New("Game console command failed")
	}
	return nil
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
		return errors.New("Container standard input is closed; recreate it with stdin_open: true")
	}
	if info.Config.StdinOnce {
		return errors.New("Container standard input closes after an attached client disconnects; recreate it with StdinOnce disabled")
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
		return errors.New("Game console command failed")
	}
	out.system("Command sent to the server process")
	return nil
}

// networkHelper runs a network console command inside the game's network
// namespace, for consoles that listen only on the container's loopback.
// The helper is removed before returning, even after timeouts or revocation,
// because the caller holds the server lock until Docker confirms it.
func (r *Runtime) networkHelper(ctx context.Context, containerID string, request helper.NetworkConsoleRequest, assertAccess func() error) (string, error) {
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
		return "", err
	}
	if len(request.Password)+len(request.Command) > netconsole.MaxBytes {
		return "", errors.New("Console command or credential is too large")
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
	preparation := time.AfterFunc(5*time.Second, func() { cancel(errors.New("Game console preparation timed out")) })
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
		return "", failure(err)
	}
	name := "ludock-console-" + identity.NewUUID()
	// Never race create/start: wait for the outcome, then remove even a
	// late-created helper.
	created, err := r.Docker.CreateHelper(context.WithoutCancel(ctx), docker.ContainerCreate{Name: name, Image: image,
		Entrypoint: []string{helper.Binary}, Cmd: []string{"helper", "network-console"}, User: "65534:65534",
		OpenStdin: true, StdinOnce: true, Env: launch.HelperEnv,
		Labels: map[string]string{"ludock.enable": "false", "ludock.internal": "console-helper"},
		HostConfig: &docker.HostConfig{NetworkMode: "container:" + containerID, ReadonlyRootfs: true, AutoRemove: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, PidsLimit: 64, Memory: 256 * 1024 * 1024,
			NanoCpus: 1_000_000_000}})
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
		return "", failure(err)
	}
	if err := check(); err != nil {
		return "", failure(err)
	}
	conn, err := r.Docker.Attach(ctx, helperRef, docker.AttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		return "", failure(err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := r.Docker.Start(ctx, helperRef); err != nil {
		return "", failure(err)
	}
	if err := check(); err != nil {
		return "", failure(err)
	}
	preparation.Stop()
	commandTimer := time.AfterFunc(10*time.Second, func() { cancel(errTimedOut) })
	defer commandTimer.Stop()
	encoded, _ := json.Marshal(request)
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return "", failure(errors.New("Game console command failed"))
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
			return "", failure(errLimited)
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", failure(errors.New("Game console helper ended without a response"))
		}
		var record struct {
			Type     string  `json:"type"`
			Response *string `json:"response"`
		}
		decodeErr := json.Unmarshal(line, &record)
		line = nil
		if decodeErr != nil {
			return "", failure(errors.New("Invalid console helper response"))
		}
		if err := check(); err != nil {
			return "", failure(err)
		}
		switch {
		case record.Type == "authorize":
			if _, err := conn.Write([]byte("{\"type\":\"authorized\"}\n")); err != nil {
				return "", failure(errors.New("Game console command failed"))
			}
		case record.Type == "result" && record.Response != nil:
			if len(*record.Response) > netconsole.MaxBytes+65536 {
				return "", failure(errLimited)
			}
			return *record.Response, nil
		default:
			return "", failure(errors.New("Game console command failed"))
		}
	}
}
