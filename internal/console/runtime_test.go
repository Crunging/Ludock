package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Crunging/Ludock/internal/launch"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
)

var ctx = context.Background()

func setup(t *testing.T) (*fakedocker.Engine, *Runtime, string) {
	t.Helper()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	id := engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	return engine, &Runtime{Docker: docker.New(engine.Socket), pollInterval: time.Millisecond}, id
}

// wrapped returns the user command behind the bounded exec wrapper.
func wrapped(cmd []string) []string {
	if len(cmd) > 7 && cmd[0] == "/bin/sh" && cmd[3] == "ludock-console" {
		return cmd[7:]
	}
	return nil
}

type collected struct {
	mu             sync.Mutex
	stdout, system strings.Builder
	completed      bool
}

func (c *collected) output() Output {
	return Output{
		Stdout:   func(v string) { c.mu.Lock(); c.stdout.WriteString(v); c.mu.Unlock() },
		Stderr:   func(string) {},
		System:   func(v string) { c.mu.Lock(); c.system.WriteString(v); c.mu.Unlock() },
		Complete: func() { c.completed = true },
	}
}

func TestShellCommandAndExitCodes(t *testing.T) {
	engine, runtime, id := setup(t)
	exitCode := 0
	engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		user := wrapped(cmd)
		if strings.Join(user, " ") != "timeout -k 5 60 /bin/sh -c ls" {
			t.Errorf("command %v", cmd)
		}
		// A rune split across frames must arrive intact.
		stdout([]byte("world Zoë"[:7]))
		stdout([]byte("world Zoë"[7:]))
		return exitCode
	}
	out := &collected{}
	if err := runtime.Shell(ctx, id, "ls", out.output(), nil); err != nil {
		t.Fatal(err)
	}
	if out.stdout.String() != "world Zoë" || !out.completed {
		t.Fatalf("%q", out.stdout.String())
	}
	for code, want := range map[int]string{124: "timed out", 125: "cancelled", 1: "command failed"} {
		exitCode = code
		err := runtime.Shell(ctx, id, "ls", Discard, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("exit %d: %v", code, err)
		}
	}
}

func TestOutputLimitAndRevocationCancel(t *testing.T) {
	engine, runtime, id := setup(t)
	var cancels int
	var mu sync.Mutex
	chunks := 5
	engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		if len(cmd) > 3 && cmd[3] == "ludock-console-cancel" {
			mu.Lock()
			cancels++
			mu.Unlock()
			return 0
		}
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range chunks {
			stdout(chunk)
		}
		return 0
	}
	if err := runtime.Shell(ctx, id, "ls", Discard, nil); !errors.Is(err, errLimited) {
		t.Fatal(err)
	}
	chunks = 2
	calls := 0
	err := runtime.Shell(ctx, id, "ls", Discard, func() error {
		calls++
		if calls > 2 {
			return errors.New("revoked")
		}
		return nil
	})
	if !errors.Is(err, errCancelled) {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if cancels == 0 {
		t.Fatal("no in-container cancellation requested")
	}
}

func TestStdinConsole(t *testing.T) {
	engine, runtime, id := setup(t)
	adapter := games.ResolveAdapter("terraria", nil)
	if err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "save", Discard, nil); err == nil ||
		!strings.Contains(err.Error(), "stdin_open") {
		t.Fatal(err)
	}
	engine.Update(id, func(c *fakedocker.Container) { c.Config.OpenStdin = true })
	out := &collected{}
	if err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "save", out.output(), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	engine.Update(id, func(c *fakedocker.Container) {
		if string(c.Stdin) != "save\n" {
			t.Errorf("stdin %q", c.Stdin)
		}
	})
	if out.system.String() != "Command sent to the server process" {
		t.Fatal(out.system.String())
	}
}

func TestNetworkTargetResolution(t *testing.T) {
	engine, runtime, id := setup(t)
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Env = []string{"RCON_PASSWORD=secret", "RCON_PORT=27015"} })
	adapter := games.ResolveAdapter("factorio", nil)
	host, port, password, err := runtime.networkTarget(ctx, Target{ContainerID: id, Labels: map[string]string{}}, adapter)
	if err != nil || host != "" || port != 27015 || password != "secret" {
		t.Fatal(host, port, password, err)
	}
	for labels, want := range map[string]string{
		`{"ludock.console.port":"0"}`:               "console port",
		`{"ludock.console.password-env":"1BAD"}`:    "valid environment name",
		`{"ludock.console.password-env":"MISSING"}`: "empty or unavailable",
		`{"ludock.console.host":"http://evil"}`:     "plain hostname",
	} {
		var parsed map[string]string
		json.Unmarshal([]byte(labels), &parsed)
		_, _, _, err := runtime.networkTarget(ctx, Target{ContainerID: id, Labels: parsed}, adapter)
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: %v", labels, err)
		}
	}
}

// Minecraft's console password comes from RCON_PASSWORD, else from the file
// the image writes for rcon-cli, which also holds a password it generated.
// Only a small regular file is read, and the password never shows in
// command output.
func TestMinecraftConsolePassword(t *testing.T) {
	engine, runtime, id := setup(t)
	adapter := games.ResolveAdapter("minecraft", nil)
	target := Target{ContainerID: id, Labels: map[string]string{}}
	engine.Update(id, func(c *fakedocker.Container) {
		c.Files = map[string]string{"/data/.rcon-cli.env": "port=25575\r\npassword=generated-secret\r\n"}
	})
	host, port, password, err := runtime.networkTarget(ctx, target, adapter)
	if err != nil || host != "" || port != 25575 || password != "generated-secret" {
		t.Fatal(host, port, password, err)
	}
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=given-secret", "RCON_PORT=25580"}
	})
	if _, port, password, err = runtime.networkTarget(ctx, target, adapter); err != nil || port != 25580 || password != "given-secret" {
		t.Fatal(port, password, err)
	}
	for name, content := range map[string]string{"link": "-> /etc/shadow", "large": "password=" + strings.Repeat("x", 5000),
		"no password": "port=25575\n", "missing": ""} {
		engine.Update(id, func(c *fakedocker.Container) {
			c.Config.Env, c.Files = nil, map[string]string{"/data/.rcon-cli.env": content}
			if content == "" {
				c.Files = nil
			}
		})
		_, _, _, err := runtime.networkTarget(ctx, target, adapter)
		if err == nil || !strings.Contains(err.Error(), "could not read the console password") {
			t.Errorf("%s: %v", name, err)
		}
	}
	var sent string
	rcon, err := fakedocker.ServeRCON("generated-secret", func(command string) string {
		sent = command
		return "rcon password is generated-secret"
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.Update(id, func(c *fakedocker.Container) {
		c.Files = map[string]string{"/data/.rcon-cli.env": "password=generated-secret\n"}
	})
	out := &collected{}
	direct := Target{ContainerID: id, State: "running", Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(rcon)}}
	if err := runtime.GameCommand(ctx, direct, adapter, "/list", out.output(), nil); err != nil {
		t.Fatal(err)
	}
	if sent != "list" || out.stdout.String() != "rcon password is [redacted]" {
		t.Fatalf("%q %q", sent, out.stdout.String())
	}
}

func TestNetworkConsoleHelperProtocol(t *testing.T) {
	// The helper asks for authorization before each write; a refusal stops it.
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- helper.RunNetworkConsole(stdinReader, stdoutWriter); stdoutWriter.Close() }()
	request, _ := json.Marshal(helper.NetworkConsoleRequest{Transport: "source-rcon", Port: 1, Password: "p", Command: "c"})
	stdinWriter.Write(append(request, '\n'))
	lines := bufio.NewReader(stdoutReader)
	first, _ := lines.ReadString('\n')
	if strings.TrimSpace(first) != `{"type":"authorize"}` {
		t.Fatal(first)
	}
	stdinWriter.Write([]byte("{\"type\":\"denied\"}\n"))
	last, _ := lines.ReadString('\n')
	if strings.TrimSpace(last) != `{"type":"error"}` || <-done != 1 {
		t.Fatal(last)
	}
}

// A stalled command start ends and is reconciled.
func TestStalledCommandStart(t *testing.T) {
	engine, runtime, id := setup(t)
	runtime.startLimit = 200 * time.Millisecond
	// Docker accepts the start and never answers it.
	engine.HangExecStarts = 1
	done := make(chan error, 1)
	go func() {
		done <- runtime.Shell(ctx, id, "ls", Discard, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a command whose start never answered reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command start kept waiting for Docker")
	}
}

// A stalled helper create ends and removes the helper.
func TestStalledHelperCreate(t *testing.T) {
	engine, runtime, id := setup(t)
	image := "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"
	engine.AddImage(image)
	runtime.Images = &launch.Images{Docker: runtime.Docker, Configured: image}
	runtime.createLimit = 200 * time.Millisecond
	// Docker creates the helper and never answers.
	engine.HangCreates = 1
	done := make(chan error, 1)
	go func() {
		_, err := runtime.networkHelper(ctx, id, helper.NetworkConsoleRequest{Transport: "source-rcon", Port: 1, Command: "c"}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a helper whose create never answered reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the console kept waiting for Docker to create its helper")
	}
	for _, container := range engine.SortedIDs() {
		if c := engine.Container(container); c != nil && c.Config.Labels["ludock.internal"] == "console-helper" {
			t.Fatal("left the helper Docker created")
		}
	}
}
