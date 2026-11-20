package helper

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/gamequery"
	"github.com/Crunging/Ludock/internal/netconsole"
)

// NetworkConsoleRequest is the first record the backend sends. No request
// data is put in Docker configuration or diagnostics.
type NetworkConsoleRequest struct {
	Transport string `json:"transport"`
	Port      int    `json:"port"`
	Password  string `json:"password"`
	Command   string `json:"command"`
	// Query asks the game's status query beside the command, or alone when
	// Transport is empty.
	Query *StatusRequest `json:"query,omitempty"`
	// Known is the player list the command last returned. With it the query
	// starts first, and the command is skipped when the game's status,
	// answered within a second, lists exactly these players, since every
	// console connection can write to the game's log.
	Known *[]string `json:"known,omitempty"`
}

type StatusRequest struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

// StatusResult is a status query's outcome; Answered is false for silence
// and for any reply that isn't a valid status.
type StatusResult struct {
	Answered bool `json:"answered"`
	gamequery.Status
}

const (
	combinedCommandTimeout = 7 * time.Second
	queryHeadStart         = time.Second
)

// RunNetworkConsole runs inside the game's network namespace. The backend
// authorizes each network write over the private attached stdin channel.
// Errors are never printed: WebSocket diagnostics can contain the password.
func RunNetworkConsole(stdin io.Reader, stdout io.Writer) int {
	// Independent of the parent connection, including a crash before stdin opens.
	timer := time.AfterFunc(15*time.Second, func() { os.Exit(1) })
	defer timer.Stop()
	reader := bufio.NewReaderSize(stdin, 64*1024)
	var writeMu sync.Mutex
	send := func(record any) {
		encoded, _ := json.Marshal(record)
		writeMu.Lock()
		stdout.Write(append(encoded, '\n'))
		writeMu.Unlock()
	}
	readRecord := func(target any) error {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return errors.New("console parent disconnected")
		}
		return json.Unmarshal(line, target)
	}
	var request NetworkConsoleRequest
	if err := readRecord(&request); err != nil {
		send(map[string]string{"type": "error"})
		return 1
	}
	console := request.Transport != ""
	validPort := func(port int) bool { return port >= 1 && port <= 65535 }
	if (console && (!slices.Contains([]string{"source-rcon", "rust-webrcon", "telnet"}, request.Transport) || !validPort(request.Port))) ||
		(request.Query != nil && (!slices.Contains([]string{string(gamequery.MinecraftPing), string(gamequery.ValveA2S)}, request.Query.Protocol) ||
			!validPort(request.Query.Port))) || (!console && request.Query == nil) {
		send(map[string]string{"type": "error"})
		return 1
	}
	// The command and the query each ask before writing, one at a time.
	var authorizeMu sync.Mutex
	authorize := func() error {
		authorizeMu.Lock()
		defer authorizeMu.Unlock()
		send(map[string]string{"type": "authorize"})
		var reply struct{ Type string }
		if err := readRecord(&reply); err != nil || reply.Type != "authorized" {
			return errors.New("console access changed")
		}
		return nil
	}
	// A query that never ran is no evidence, so it has no status.
	var status *StatusResult
	var queried sync.WaitGroup
	// Beside a query, the command ends early enough for the query's result
	// to reach the backend within its 10-second wait.
	deadline := time.Now().Add(combinedCommandTimeout)
	if request.Query != nil {
		queried.Add(1)
		query := func() {
			defer queried.Done()
			denied := false
			reply, err := gamequery.Query(context.Background(), gamequery.Protocol(request.Query.Protocol), "127.0.0.1", request.Query.Port,
				func() error {
					err := authorize()
					denied = denied || err != nil
					return err
				})
			if !denied {
				status = &StatusResult{Answered: err == nil, Status: reply}
			}
		}
		go query()
	}
	unchanged := false
	if request.Known != nil && request.Query != nil {
		// The query gets a head start; a slow one still leaves the command
		// most of its time.
		done := make(chan struct{})
		go func() { queried.Wait(); close(done) }()
		select {
		case <-done:
			unchanged = status != nil && status.Answered && status.Lists(*request.Known)
		case <-time.After(queryHeadStart):
		}
	}
	var response *string
	if console && !unchanged {
		// WebRCON sends its credential during the handshake.
		ctx, cancel := context.Background(), func() {}
		if request.Query != nil {
			ctx, cancel = context.WithDeadline(ctx, deadline)
		}
		defer cancel()
		if authorize() == nil {
			text, err := netconsole.Execute(ctx, request.Transport, "127.0.0.1", request.Port, request.Password,
				request.Command, netconsole.Control{Authorize: authorize})
			if err == nil {
				response = &text
			}
		}
	}
	queried.Wait()
	if response == nil && status == nil {
		send(map[string]string{"type": "error"})
		return 1
	}
	send(map[string]any{"type": "result", "response": response, "status": status})
	return 0
}
