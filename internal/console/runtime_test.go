package console

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/gamequery"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/launch"
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

// Terraria prints its reply to playing to its own output, among other log
// lines. A reading types the command and reads the reply from the log that
// follows it, never an older reply already in the log; a game that never
// replies gives no reading.
func TestStdinListReadsTheReplyFromTheLog(t *testing.T) {
	engine, runtime, id := setup(t)
	runtime.replyLimit = 300 * time.Millisecond
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.OpenStdin = true
		c.Logs = []string{": Old (172.18.0.9:40000)", "1 player connected."}
	})
	replies := true
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, _ func([]byte)) {
		line, _ := stdin.ReadString('\n')
		if !replies || line != "playing\n" {
			return
		}
		engine.Update(c.ID, func(c *fakedocker.Container) {
			c.Logs = append(c.Logs, "<Zed> hello", ": playing", "Ada (172.18.0.1:51234)")
		})
		time.Sleep(50 * time.Millisecond)
		engine.Update(c.ID, func(c *fakedocker.Container) {
			c.Logs = append(c.Logs, "Zed (172.18.0.2:51235)", "2 players connected.")
		})
	}
	adapter := games.ResolveAdapter("terraria", nil)
	probe := Probe{Adapter: adapter, Command: adapter.Players.Command}
	result, err := runtime.Probe(ctx, Target{ContainerID: id, State: "running"}, probe, nil)
	if err != nil || result.ResponseErr != nil {
		t.Fatal(err, result.ResponseErr)
	}
	players, ok := adapter.Players.Parse(result.Response)
	if !ok || players.Online != 2 || strings.Join(players.Names, ",") != "Ada,Zed" {
		t.Fatalf("%q: %+v", result.Response, players)
	}
	replies = false
	result, err = runtime.Probe(ctx, Target{ContainerID: id, State: "running"}, probe, nil)
	if err != nil || result.ResponseErr == nil {
		t.Fatal(err, result)
	}
}

func TestNetworkTargetResolution(t *testing.T) {
	engine, runtime, id := setup(t)
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Env = []string{"RCON_PASSWORD=secret", "RCON_PORT=27015"} })
	adapter := games.ResolveAdapter("factorio", nil)
	env, err := runtime.environment(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	host, port, password, err := runtime.networkTarget(ctx, Target{ContainerID: id, Labels: map[string]string{}}, adapter, env)
	if err != nil || host != "" || port != 27015 || password != "secret" {
		t.Fatal(host, port, password, err)
	}
	for labels, want := range map[string]string{
		`{"ludock.console.port":"0"}`:               "console port",
		`{"ludock.console.password-env":"1BAD"}`:    "valid environment variable name",
		`{"ludock.console.password-env":"MISSING"}`: "empty or not set",
		`{"ludock.console.host":"http://evil"}`:     "plain hostname",
	} {
		var parsed map[string]string
		json.Unmarshal([]byte(labels), &parsed)
		_, _, _, err := runtime.networkTarget(ctx, Target{ContainerID: id, Labels: parsed}, adapter, env)
		// The console shows these messages, so they are public and name no values.
		if !apperr.Is(err, "CONSOLE_NOT_CONFIGURED") || !strings.Contains(err.Error(), want) ||
			strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "MISSING") {
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
	host, port, password, err := resolveTarget(t, runtime, target, adapter)
	if err != nil || host != "" || port != 25575 || password != "generated-secret" {
		t.Fatal(host, port, password, err)
	}
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=given-secret", "RCON_PORT=25580"}
	})
	if _, port, password, err = resolveTarget(t, runtime, target, adapter); err != nil || port != 25580 || password != "given-secret" {
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
		// Shown as it is, not as a command the game didn't take.
		_, _, _, err := resolveTarget(t, runtime, target, adapter)
		if _, public := apperr.As(err); !public || !strings.Contains(err.Error(), "could not read the console password") {
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

// vinanrra/7dtd-server keeps 7 Days to Die's Telnet password in
// sdtdserver.xml, empty by default, which means no password; a value set
// there is the password, and a variable a label names still wins.
func TestSevenDaysConsolePassword(t *testing.T) {
	engine, runtime, id := setup(t)
	adapter := games.ResolveAdapter("7-days-to-die", nil)
	target := Target{ContainerID: id, Labels: map[string]string{}}
	config := func(value string) string {
		return `<?xml version="1.0"?>` + "\n<ServerSettings>\n\t<property name=\"TelnetEnabled\"\tvalue=\"true\"/>\t\t<!-- Enable/Disable the telnet -->\n" +
			"\t<property name=\"TelnetPassword\"\t\tvalue=\"" + value + "\"/>\t\t<!-- Password to gain entry to telnet interface. -->\n</ServerSettings>\n"
	}
	for value, want := range map[string]string{"": "", "s&amp;cret": "s&cret"} {
		engine.Update(id, func(c *fakedocker.Container) {
			c.Files = map[string]string{"/home/sdtdserver/serverfiles/sdtdserver.xml": config(value)}
		})
		if _, port, password, err := resolveTarget(t, runtime, target, adapter); err != nil || port != 8081 || password != want {
			t.Fatalf("%q: %d %q %v", value, port, password, err)
		}
	}
	// Nothing in the reply is taken for the empty password.
	engine.Update(id, func(c *fakedocker.Container) {
		c.Files = map[string]string{"/home/sdtdserver/serverfiles/sdtdserver.xml": config("")}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.Write([]byte("*** Connected with 7DTD server.\r\n"))
		bufio.NewReader(conn).ReadString('\n')
		conn.Write([]byte("Total of 0 in the game\r\n"))
		io.Copy(io.Discard, conn)
	}()
	out := &collected{}
	direct := Target{ContainerID: id, State: "running", Labels: map[string]string{"ludock.console.host": "127.0.0.1",
		"ludock.console.port": fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)}}
	if err := runtime.GameCommand(ctx, direct, adapter, "listplayers", out.output(), nil); err != nil || out.stdout.String() != "Total of 0 in the game" {
		t.Fatalf("%q %v", out.stdout.String(), err)
	}
	engine.Update(id, func(c *fakedocker.Container) { c.Config.Env = []string{"OWN_PASSWORD=given"} })
	named := Target{ContainerID: id, Labels: map[string]string{"ludock.console.password-env": "OWN_PASSWORD"}}
	if _, _, password, err := resolveTarget(t, runtime, named, adapter); err != nil || password != "given" {
		t.Fatal(password, err)
	}
	for name, content := range map[string]string{"no property": "<ServerSettings/>", "not XML": "TelnetPassword=x", "missing": ""} {
		engine.Update(id, func(c *fakedocker.Container) {
			c.Files = map[string]string{"/home/sdtdserver/serverfiles/sdtdserver.xml": content}
			if content == "" {
				c.Files = nil
			}
		})
		// Shown as it is, and what a warning waits out while the image
		// writes the file.
		_, _, _, err := resolveTarget(t, runtime, target, adapter)
		if public, ok := apperr.As(err); !ok || public.Code != PasswordUnreadable || !strings.Contains(err.Error(), "could not read the console password") {
			t.Errorf("%s: %v", name, err)
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

func resolveTarget(t *testing.T, runtime *Runtime, target Target, adapter *games.Adapter) (string, int, string, error) {
	env, err := runtime.environment(ctx, target.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.networkTarget(ctx, target, adapter, env)
}

// A reading asks the console and the game's status query in one helper,
// and a console that isn't set up still lets the query run alone.
func TestProbeUsesOneHelper(t *testing.T) {
	engine, runtime, id := setup(t)
	image := "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"
	engine.AddImage(image)
	runtime.Images = &launch.Images{Docker: runtime.Docker, Configured: image}
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, stdout func([]byte)) {
		helper.RunNetworkConsole(stdin, writerFunc(stdout))
	}
	rcon, _ := fakedocker.ServeRCON("secret", func(string) string { return "There are 0 of a max of 5 players online:" })
	query, _ := fakedocker.ServeMinecraftStatus(func() (int, int, bool) { return 3, 5, true })
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=secret", fmt.Sprintf("RCON_PORT=%d", rcon)}
	})
	adapter := games.ResolveAdapter("minecraft", nil)
	probe := Probe{Adapter: adapter, Command: "list", Query: &games.StatusQuery{Protocol: gamequery.MinecraftPing, Port: query}}
	helpers := func() int {
		count := 0
		for _, request := range engine.Requests {
			if strings.HasPrefix(request, "POST ") && strings.HasSuffix(request, "/containers/create") {
				count++
			}
		}
		return count
	}
	target := Target{ContainerID: id, Labels: map[string]string{}, State: "running"}
	result, err := runtime.Probe(ctx, target, probe, nil)
	if err != nil || result.ResponseErr != nil || !strings.Contains(result.Response, "max of 5") || result.Status == nil ||
		!result.Status.Answered || result.Status.Online != 3 || helpers() != 1 {
		t.Fatalf("%+v %v, %d helpers", result, err, helpers())
	}
	// The helper prints the raw reply, which Docker must not log.
	if logging := engine.HelperLogging(); logging["console-helper"] != "none" {
		t.Fatal(logging)
	}
	target.Labels = map[string]string{"ludock.console.password-env": "MISSING"}
	result, err = runtime.Probe(ctx, target, probe, nil)
	if err != nil || result.ResponseErr == nil || result.Status == nil || !result.Status.Answered || helpers() != 2 {
		t.Fatalf("%+v %v, %d helpers", result, err, helpers())
	}

	// A console that takes the connection and never replies still leaves
	// time for the query's result.
	stalled, _ := net.Listen("tcp", "127.0.0.1:0")
	defer stalled.Close()
	go func() {
		for {
			conn, err := stalled.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=secret", fmt.Sprintf("RCON_PORT=%d", stalled.Addr().(*net.TCPAddr).Port)}
	})
	target.Labels = map[string]string{}
	result, err = runtime.Probe(ctx, target, probe, nil)
	if err != nil || result.ResponseErr == nil || result.Status == nil || !result.Status.Answered {
		t.Fatalf("stalled console: %+v %v", result, err)
	}
}

// A query whose player sample names the players the console listed last
// stands in for the console's list, through a helper and through a console
// set to connect directly; a different sample still sends the command, as
// does a query too slow to answer within its head start.
func TestProbeSkipsTheListTheQueryVouchesFor(t *testing.T) {
	engine, runtime, id := setup(t)
	image := "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"
	engine.AddImage(image)
	runtime.Images = &launch.Images{Docker: runtime.Docker, Configured: image}
	engine.Attach = func(c *fakedocker.Container, stdin *bufio.Reader, stdout func([]byte)) {
		helper.RunNetworkConsole(stdin, writerFunc(stdout))
	}
	var mu sync.Mutex
	commands := 0
	rcon, _ := fakedocker.ServeRCON("secret", func(string) string {
		mu.Lock()
		defer mu.Unlock()
		commands++
		return "There are 1 of a max of 5 players online: Mara"
	})
	query, _ := fakedocker.ServeMinecraftPlayers(func() ([]string, int, int, bool) { return []string{"Mara"}, 1, 5, true })
	engine.Update(id, func(c *fakedocker.Container) {
		c.Config.Env = []string{"RCON_PASSWORD=secret", fmt.Sprintf("RCON_PORT=%d", rcon)}
	})
	adapter := games.ResolveAdapter("minecraft", nil)
	for _, labels := range []map[string]string{{}, {"ludock.console.host": "127.0.0.1"}} {
		target := Target{ContainerID: id, Labels: labels, State: "running"}
		for _, known := range [][]string{{"Mara"}, {"Theo"}} {
			mu.Lock()
			before := commands
			mu.Unlock()
			probe := Probe{Adapter: adapter, Command: "list", Query: &games.StatusQuery{Protocol: gamequery.MinecraftPing, Port: query},
				Known: &known}
			result, err := runtime.Probe(ctx, target, probe, nil)
			mu.Lock()
			sent := commands - before
			mu.Unlock()
			vouched := known[0] == "Mara"
			if err != nil || result.Status == nil || result.Unchanged != vouched || sent != map[bool]int{true: 0, false: 1}[vouched] ||
				(!vouched && !strings.Contains(result.Response, "Mara")) {
				t.Fatalf("%v, known %v: %+v %v, %d commands", labels, known, result, err, sent)
			}
		}
	}
	slow, _ := fakedocker.ServeMinecraftPlayers(func() ([]string, int, int, bool) {
		time.Sleep(2 * time.Second)
		return []string{"Mara"}, 1, 5, true
	})
	known := []string{"Mara"}
	probe := Probe{Adapter: adapter, Command: "list", Query: &games.StatusQuery{Protocol: gamequery.MinecraftPing, Port: slow}, Known: &known}
	result, err := runtime.Probe(ctx, Target{ContainerID: id, Labels: map[string]string{}, State: "running"}, probe, nil)
	if err != nil || result.Unchanged || !strings.Contains(result.Response, "Mara") || result.Status == nil || !result.Status.Answered {
		t.Fatalf("slow query: %+v %v", result, err)
	}
}

type writerFunc func([]byte)

func (w writerFunc) Write(data []byte) (int, error) {
	w(data)
	return len(data), nil
}
