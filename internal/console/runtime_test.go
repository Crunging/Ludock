package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	if len(cmd) > 8 && cmd[0] == "/bin/sh" && cmd[3] == "ludock-console" {
		return cmd[8:]
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

func TestDockerExecAdapterAndExitCodes(t *testing.T) {
	engine, runtime, id := setup(t)
	exitCode := 0
	engine.Exec = func(_ *fakedocker.Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		user := wrapped(cmd)
		if strings.Join(user, " ") != "rcon-cli list" {
			t.Errorf("command %v", cmd)
		}
		// A rune split across frames must arrive intact.
		stdout([]byte("There are 2 players: Zoë"[:22]))
		stdout([]byte("There are 2 players: Zoë"[22:]))
		return exitCode
	}
	adapter := games.ResolveAdapter("minecraft", nil)
	out := &collected{}
	if err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "/list", out.output(), nil); err != nil {
		t.Fatal(err)
	}
	if out.stdout.String() != "There are 2 players: Zoë" || !out.completed {
		t.Fatalf("%q", out.stdout.String())
	}
	for code, want := range map[int]string{124: "timed out", 125: "cancelled", 1: "command failed"} {
		exitCode = code
		err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "list", Discard, nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("exit %d: %v", code, err)
		}
	}
	if err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "exited"}, adapter, "list", Discard, nil); err == nil {
		t.Fatal("ran on a stopped server")
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
	adapter := games.ResolveAdapter("minecraft", nil)
	if err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "list", Discard, nil); !errors.Is(err, errLimited) {
		t.Fatal(err)
	}
	chunks = 2
	calls := 0
	err := runtime.GameCommand(ctx, Target{ContainerID: id, State: "running"}, adapter, "list", Discard, func() error {
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
