package docker_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

var ctx = context.Background()

func engine(t *testing.T) (*fakedocker.Engine, *docker.Client) {
	t.Helper()
	e, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e, docker.New(e.Socket)
}

func TestNegotiatesVersionAndInspects(t *testing.T) {
	e, client := engine(t)
	id := e.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := client.Inspect(ctx, id)
	if err != nil || info.Name != "/mc" || !info.State.Running {
		t.Fatal(info, err)
	}
	if !strings.Contains(strings.Join(e.Requests, "\n"), "GET /v1.47/containers/"+id+"/json") {
		t.Fatal(e.Requests)
	}
	if err := client.Start(ctx, id); docker.StatusCode(err) != http.StatusNotModified {
		t.Fatal(err)
	}
	if _, err := client.Inspect(ctx, "../etc"); err == nil {
		t.Fatal("accepted a path as a container ID")
	}
	if _, err := client.Inspect(ctx, "missing"); !docker.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestRejectsUnsupportedEngine(t *testing.T) {
	e, client := engine(t)
	e.APIVersion = "1.41"
	if err := client.Ping(ctx); err == nil || !strings.Contains(err.Error(), "no supported API version") {
		t.Fatal(err)
	}
	e.APIVersion = "1.99"
	if err := client.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	client.ListContainers(ctx, true, nil)
	if !strings.Contains(strings.Join(e.Requests, "\n"), "/v1.55/containers/json") {
		t.Fatal("did not cap the API version")
	}
}

func TestErrorsKeepOnlyStatus(t *testing.T) {
	e, client := engine(t)
	client.Ping(ctx)
	e.Fail = 500
	_, err := client.ListContainers(ctx, true, nil)
	var api *docker.APIError
	if !errors.As(err, &api) || api.Status != 500 || strings.Contains(err.Error(), "fake docker error") {
		t.Fatal(err)
	}
}

// fakeSocket serves raw responses for transport edge cases.
func fakeSocket(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "raw.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handler(conn)
		}
	}()
	return socket
}

func respond(conn net.Conn, body string) {
	reader := bufio.NewReader(conn)
	request, err := http.ReadRequest(reader)
	if err != nil {
		conn.Close()
		return
	}
	io.Copy(io.Discard, request.Body)
	switch {
	case request.URL.Path == "/version":
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 21\r\n\r\n{\"ApiVersion\":\"1.47\"}")
		respond(conn, body)
	default:
		io.WriteString(conn, body)
		conn.Close()
	}
}

func TestPullChecksEveryRecord(t *testing.T) {
	for _, c := range []struct {
		body string
		ok   bool
	}{
		{"{\"status\":\"Pulling\"}\n{\"status\":\"Done\"}\n", true},
		{"{\"status\":\"Pulling\"}\n{\"errorDetail\":{\"message\":\"denied\"},\"error\":\"denied\"}\n", false},
		{"", false},
		{"{\"status\":\"Pull", false},
	} {
		socket := fakeSocket(t, func(conn net.Conn) {
			respond(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n\r\n"+c.body)
		})
		err := docker.New(socket).Pull(ctx, "alpine:3", "", "")
		if (err == nil) != c.ok {
			t.Errorf("%q: %v", c.body, err)
		}
		if err != nil && strings.Contains(err.Error(), "denied") {
			t.Errorf("leaked daemon text: %v", err)
		}
	}
}

func TestHijackRejectsChunkedAndReportsStart(t *testing.T) {
	socket := fakeSocket(t, func(conn net.Conn) {
		respond(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
	})
	client := docker.New(socket)
	_, err := client.StartExec(ctx, "abc")
	var open *docker.StreamOpenError
	if !errors.As(err, &open) || !open.RequestStarted {
		t.Fatal(err)
	}
	unreachable := docker.New(filepath.Join(t.TempDir(), "missing.sock"))
	// Version negotiation fails before any stream request is sent.
	if _, err := unreachable.StartExec(ctx, "abc"); err == nil {
		t.Fatal("expected failure")
	}
	// A daemon that never answers is bounded by the caller's deadline.
	deadline, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	hung := fakeSocket(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		request, _ := http.ReadRequest(reader)
		if request != nil && request.URL.Path == "/version" {
			io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 21\r\n\r\n{\"ApiVersion\":\"1.47\"}")
			http.ReadRequest(reader)
		}
		time.Sleep(2 * time.Second)
		conn.Close()
	})
	started := time.Now()
	if _, err := docker.New(hung).StartExec(deadline, "abc"); !errors.As(err, &open) || time.Since(started) > time.Second {
		t.Fatal(err, time.Since(started))
	}
}

func TestExecStreams(t *testing.T) {
	e, client := engine(t)
	id := e.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	e.Exec = func(_ *fakedocker.Container, cmd []string, stdin *bufio.Reader, stdout, stderr func([]byte)) int {
		input, _ := io.ReadAll(stdin)
		stdout([]byte(strings.Join(cmd, " ") + ":" + string(input)))
		stderr([]byte("warn"))
		return 3
	}
	execID, err := client.CreateExec(ctx, id, docker.ExecCreate{Cmd: []string{"rcon-cli", "list"}, AttachStdin: true, AttachStdout: true})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := client.StartExec(ctx, execID)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("hello"))
	conn.CloseWrite()
	var stdout, stderr bytes.Buffer
	if err := docker.Demux(conn, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if stdout.String() != "rcon-cli list:hello" || stderr.String() != "warn" {
		t.Fatalf("%q %q", stdout.String(), stderr.String())
	}
	info, err := client.InspectExec(ctx, execID)
	if err != nil || info.ExitCode == nil || *info.ExitCode != 3 {
		t.Fatal(info, err)
	}
}

func TestDemuxRejectsBadFrames(t *testing.T) {
	good := append(fakedocker.Frame(1, []byte("out")), fakedocker.Frame(2, []byte("err"))...)
	var stdout bytes.Buffer
	if err := docker.Demux(bytes.NewReader(good), &stdout, nil); err != nil || stdout.String() != "out" {
		t.Fatal(err)
	}
	if err := docker.Demux(bytes.NewReader(good[:5]), nil, nil); !errors.Is(err, docker.ErrIncompleteStream) {
		t.Fatal(err)
	}
	if err := docker.Demux(bytes.NewReader(good[:10]), nil, nil); !errors.Is(err, docker.ErrIncompleteStream) {
		t.Fatal(err)
	}
	bad := fakedocker.Frame(3, []byte("x"))
	if err := docker.Demux(bytes.NewReader(bad), nil, nil); !errors.Is(err, docker.ErrInvalidStream) {
		t.Fatal(err)
	}
}

func TestJSONLines(t *testing.T) {
	var values []string
	err := docker.DecodeJSONLines(strings.NewReader("{\"a\":1}\n\nnot json\n{\"b\":2}"), 100, true, func(raw json.RawMessage) error {
		values = append(values, string(raw))
		return nil
	})
	if err != nil || len(values) != 2 {
		t.Fatal(values, err)
	}
	if err := docker.DecodeJSONLines(strings.NewReader("not json\n"), 100, false, func(json.RawMessage) error { return nil }); err == nil {
		t.Fatal("accepted invalid line")
	}
	long := "\"" + strings.Repeat("x", 200) + "\"\n"
	if err := docker.DecodeJSONLines(strings.NewReader(long), 100, true, func(json.RawMessage) error { return nil }); !errors.Is(err, docker.ErrJSONLineTooLong) {
		t.Fatal(err)
	}
}

// Remove helper stops writer before giving up.
func TestRemoveHelperStopsWriter(t *testing.T) {
	e, client := engine(t)
	e.AddImage("ghcr.io/crunging/ludock@sha256:" + strings.Repeat("a", 64))
	id, err := client.CreateHelper(ctx, docker.ContainerCreate{Image: "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	e.Update(id, func(c *fakedocker.Container) { c.State.Running = true; c.State.Status = "running" })
	if err := client.RemoveHelper(ctx, id); err != nil {
		t.Fatal(err)
	}
	if e.Container(id) != nil {
		t.Fatal("helper left behind")
	}
	if err := client.RemoveHelper(ctx, id); err != nil {
		t.Fatal("already removed should succeed:", err)
	}
	// A missing pinned image is pulled once.
	pinned := "ghcr.io/crunging/ludock@sha256:" + strings.Repeat("b", 64)
	if _, err := client.CreateHelper(ctx, docker.ContainerCreate{Image: pinned}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateHelper(ctx, docker.ContainerCreate{Image: "unpinned:latest"}); !docker.IsNotFound(err) {
		t.Fatal(err)
	}
}
