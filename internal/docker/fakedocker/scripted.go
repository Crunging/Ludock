package fakedocker

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
)

// Exec requests, as "METHOD path" patterns for AddFault and HangRequests.
const (
	ExecCreate  = `^POST /v[0-9.]+/containers/[^/]+/exec$`
	ExecStart   = `^POST /v[0-9.]+/exec/[^/]+/start$`
	ExecInspect = `^GET /v[0-9.]+/exec/[^/]+/json$`
)

// Scripted runs exec commands named by their first argument: "ok" prints
// hello, "exit N" exits N, "block" waits, and "big" prints Big bytes and then
// waits. A waiting command is announced on Started and ends on Next or once
// Release is closed; Finished reports each command's end.
type Scripted struct {
	Big               int
	Started, Finished chan string
	Next, Release     chan struct{}
}

// Script makes the engine run scripted commands, with "big" printing big
// bytes. Closing Release before the engine closes ends waiting commands.
func (e *Engine) Script(big int) *Scripted {
	s := &Scripted{Big: big, Started: make(chan string, 8), Finished: make(chan string, 8),
		Next: make(chan struct{}), Release: make(chan struct{})}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Exec = func(_ *Container, cmd []string, _ *bufio.Reader, stdout, _ func([]byte)) int {
		defer func() {
			select {
			case s.Finished <- cmd[0]:
			default:
			}
		}()
		switch cmd[0] {
		case "ok":
			stdout([]byte("hello"))
		case "exit":
			code, _ := strconv.Atoi(cmd[1])
			return code
		case "big":
			stdout(bytes.Repeat([]byte("x"), s.Big))
			s.Started <- cmd[0]
			s.wait()
		case "block":
			s.Started <- cmd[0]
			s.wait()
		}
		return 0
	}
	return s
}

func (s *Scripted) wait() {
	select {
	case <-s.Next:
	case <-s.Release:
	}
}

// FinishOne lets one waiting command finish, unless Release already did.
func (s *Scripted) FinishOne() {
	select {
	case s.Next <- struct{}{}:
	case <-s.Release:
	}
}

// Unfinished reports that no command has finished yet.
func (s *Scripted) Unfinished() bool {
	select {
	case <-s.Finished:
		return false
	default:
		return true
	}
}

// HangRequests leaves requests matching pattern unanswered, as Hang does,
// and reports each one's arrival.
func (e *Engine) HangRequests(pattern string) <-chan struct{} {
	match := regexp.MustCompile(pattern)
	arrived := make(chan struct{}, 1)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Hang = func(request string) bool {
		if match.MatchString(request) {
			arrived <- struct{}{}
			return true
		}
		return false
	}
	return arrived
}
