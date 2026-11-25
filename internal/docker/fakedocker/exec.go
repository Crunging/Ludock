package fakedocker

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/docker"
)

type exec struct {
	id        string
	container string
	options   docker.ExecCreate
	running   bool
	exitCode  *int
	lost      bool
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
	e.mu.Lock()
	if e.streams == nil {
		e.streams = map[net.Conn]bool{}
	}
	e.streams[conn] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.streams, conn)
		e.mu.Unlock()
		conn.Close()
	}()
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
		lost := instance.lost
		e.mu.Unlock()
		if lost {
			fail(w, 500)
			return
		}
		writeJSON(w, 200, info)
		return
	}
	e.mu.Lock()
	instance.running = true
	handler := e.Exec
	failStart := e.FailExecStart != nil && e.FailExecStart(instance.options.Cmd)
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
		if failStart {
			unknown := 128
			e.mu.Lock()
			instance.running, instance.exitCode = false, &unknown
			e.mu.Unlock()
			conn.Write(Frame(1, []byte("DeadlineExceeded: failed to start io pipe copy\r\n")))
			return
		}
		e.mu.Lock()
		lose := e.LoseUploads > 0 && slices.ContainsFunc(instance.options.Cmd, func(arg string) bool {
			return strings.Contains(arg, `"operation":"upload"`) || strings.Contains(arg, `"operation":"upload-files"`)
		})
		if lose {
			e.LoseUploads--
		}
		e.mu.Unlock()
		var mu sync.Mutex
		write := func(channel byte) func([]byte) {
			return func(data []byte) {
				mu.Lock()
				defer mu.Unlock()
				// A lost result loses the helper's answers too, but not a batch
				// upload's requests to go on: for its next trailer, to make a
				// folder, or for its files.
				if !lose || channel != 1 || bytes.Contains(data, []byte(`"ready":true`)) || bytes.Contains(data, []byte(`"folder":true`)) ||
					bytes.Contains(data, []byte(`"files":true`)) {
					conn.Write(Frame(channel, data))
				}
			}
		}
		code := 0
		if handler != nil && c != nil {
			code = handler(c, instance.options.Cmd, reader, write(1), write(2))
		}
		if code == 0 {
			e.startAfterStep(instance.options.Cmd)
		}
		e.mu.Lock()
		delay := e.ExitDelay
		e.mu.Unlock()
		if delay > 0 {
			conn.Close()
			time.Sleep(delay)
		}
		e.mu.Lock()
		instance.running = false
		instance.exitCode = &code
		instance.lost = lose
		e.mu.Unlock()
	})
}

// startAfterStep starts the container a journey named with
// /fake/start-after, as another manager would, once the restore helper step
// it named has run.
func (e *Engine) startAfterStep(cmd []string) {
	e.mu.Lock()
	var c *Container
	for step, name := range e.startAfter {
		if slices.ContainsFunc(cmd, func(arg string) bool { return strings.Contains(arg, `"operation":"`+step+`"`) }) {
			delete(e.startAfter, step)
			c = e.find(name)
			break
		}
	}
	if c == nil || c.State.Running {
		e.mu.Unlock()
		return
	}
	c.State.Status, c.State.Running, c.State.StartedAt = "running", true, time.Now().UTC().Format(time.RFC3339Nano)
	e.emitLocked("start", c.ID)
	hook := e.OnStart
	e.mu.Unlock()
	if hook != nil {
		hook(c)
	}
}
