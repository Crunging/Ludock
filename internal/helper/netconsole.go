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

	"github.com/Crunging/Ludock/internal/netconsole"
)

// NetworkConsoleRequest is the first record the backend sends. No request
// data is put in Docker configuration or diagnostics.
type NetworkConsoleRequest struct {
	Transport string `json:"transport"`
	Port      int    `json:"port"`
	Password  string `json:"password"`
	Command   string `json:"command"`
}

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
	run := func() (string, error) {
		var request NetworkConsoleRequest
		if err := readRecord(&request); err != nil {
			return "", err
		}
		if !slices.Contains([]string{"source-rcon", "rust-webrcon", "telnet"}, request.Transport) || request.Port < 1 || request.Port > 65535 {
			return "", errors.New("invalid console request")
		}
		authorize := func() error {
			send(map[string]string{"type": "authorize"})
			var reply struct{ Type string }
			if err := readRecord(&reply); err != nil || reply.Type != "authorized" {
				return errors.New("console access changed")
			}
			return nil
		}
		// WebRCON sends its credential during the handshake.
		if err := authorize(); err != nil {
			return "", err
		}
		return netconsole.Execute(context.Background(), request.Transport, "127.0.0.1", request.Port, request.Password,
			request.Command, netconsole.Control{Authorize: authorize})
	}
	response, err := run()
	if err != nil {
		send(map[string]string{"type": "error"})
		return 1
	}
	send(map[string]string{"type": "result", "response": response})
	return 0
}
