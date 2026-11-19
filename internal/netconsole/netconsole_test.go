package netconsole

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var ctx = context.Background()

func listen(t *testing.T, serve func(net.Conn)) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() { defer conn.Close(); serve(conn) }()
		}
	}()
	address := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", address.Port
}

func readPacket(r io.Reader) (int32, int32, string, error) {
	var size int32
	if err := binary.Read(r, binary.LittleEndian, &size); err != nil {
		return 0, 0, "", err
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, 0, "", err
	}
	return int32(binary.LittleEndian.Uint32(data)), int32(binary.LittleEndian.Uint32(data[4:])), string(data[8 : len(data)-2]), nil
}

func rconServer(password string) func(net.Conn) {
	return func(conn net.Conn) {
		id, _, body, err := readPacket(conn)
		if err != nil {
			return
		}
		if body != password {
			packet, _ := EncodeRCON(-1, 2, "")
			conn.Write(packet)
			return
		}
		packet, _ := EncodeRCON(id, 2, "")
		conn.Write(packet)
		commandID, _, command, _ := readPacket(conn)
		first, _ := EncodeRCON(commandID, 0, "players: ")
		second, _ := EncodeRCON(commandID, 0, command)
		conn.Write(append(first, second...))
		io.Copy(io.Discard, conn)
	}
}

func TestSourceRCON(t *testing.T) {
	host, port := listen(t, rconServer("secret"))
	response, err := SourceRCON(ctx, host, port, "secret", "list", Control{})
	if err != nil || response != "players: list" {
		t.Fatal(response, err)
	}
	if _, err := SourceRCON(ctx, host, port, "wrong", "list", Control{}); err == nil || err.Error() != "RCON authentication failed" {
		t.Fatal(err)
	}
	// Revoked access stops the command before the credential is sent.
	calls := 0
	_, err = SourceRCON(ctx, host, port, "secret", "list", Control{AssertAccess: func() error {
		calls++
		if calls > 1 {
			return errors.New("revoked")
		}
		return nil
	}})
	if !errors.Is(err, errAccessChanged) {
		t.Fatal(err)
	}
	if _, _, err := DecodeRCON([]byte{1, 0, 0, 0}); err == nil {
		t.Fatal("accepted an undersized packet")
	}
}

func TestTelnet(t *testing.T) {
	host, port := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte{255, 251, 1})
		conn.Write([]byte("Please enter password:"))
		reply := make([]byte, 3)
		io.ReadFull(reader, reply)
		password, _ := reader.ReadString('\n')
		if strings.TrimSpace(password) != "telnet-secret" {
			conn.Write([]byte("Password incorrect"))
			return
		}
		command, _ := reader.ReadString('\n')
		conn.Write([]byte(command + "Total of 2 in the game\r\n"))
		io.Copy(io.Discard, reader)
	})
	response, err := Telnet(ctx, host, port, "telnet-secret", "listplayers", Control{})
	if err != nil || response != "Total of 2 in the game" {
		t.Fatalf("%q %v", response, err)
	}
	if _, err := Telnet(ctx, host, port, "bad", "listplayers", Control{}); err == nil || err.Error() != "Telnet console authentication failed" {
		t.Fatal(err)
	}
}

// WebRCON never goes through a configured proxy, which would see the
// password in its URL.
func TestRustWebRCONIgnoresProxy(t *testing.T) {
	proxied := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- struct{}{}
		http.Error(w, "proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	// A host the proxy would be used for; the dial fails without reaching it.
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	RustWebRCON(short, "192.0.2.1", 28016, "secret", "status", Control{})
	select {
	case <-proxied:
		t.Fatal("WebRCON request went through the proxy")
	default:
	}
}

func TestRustWebRCON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/web secret" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, data, _ := conn.Read(r.Context())
		var request struct {
			Identifier int
			Message    string
		}
		json.Unmarshal(data, &request)
		conn.Write(r.Context(), websocket.MessageText, []byte(`{"Identifier":1,"Message":"other"}`))
		reply, _ := json.Marshal(map[string]any{"Identifier": request.Identifier, "Message": "saved " + request.Message})
		conn.Write(r.Context(), websocket.MessageText, reply)
	}))
	defer server.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	response, err := RustWebRCON(ctx, "127.0.0.1", port, "web secret", "server.save", Control{})
	if err != nil || response != "saved server.save" {
		t.Fatal(response, err)
	}
	_, err = RustWebRCON(ctx, "127.0.0.1", port, "wrong", "server.save", Control{})
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatal("credential leaked or accepted:", err)
	}
}

func TestCleanTelnet(t *testing.T) {
	if got := CleanTelnet("say hi\nok", "say hi"); got != "ok" {
		t.Fatalf("%q", got)
	}
}

// Log lines streamed to the console, such as player chat, neither end the
// sign-in nor replace the command's response.
func TestTelnetIgnoresStreamedLog(t *testing.T) {
	host, port := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte("Please enter password:"))
		reader.ReadString('\n')
		conn.Write([]byte("Logon successful.\r\n"))
		command, _ := reader.ReadString('\n')
		conn.Write([]byte(command + "Chat 'Bob': my password is incorrect\r\nTotal of 2 in the game\r\n"))
		io.Copy(io.Discard, reader)
	})
	response, err := Telnet(ctx, host, port, "telnet-secret", "listplayers", Control{})
	if err != nil || response != "Chat 'Bob': my password is incorrect\r\nTotal of 2 in the game" {
		t.Fatalf("%q %v", response, err)
	}
	// Chat right after sign-in, before the command, is not a rejection, and
	// neither is a command that says the word.
	chatty, chattyPort := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte("Please enter password:"))
		reader.ReadString('\n')
		conn.Write([]byte("Logon successful.\r\nChat 'Bob': password is incorrect\r\n"))
		command, _ := reader.ReadString('\n')
		conn.Write([]byte(command + "done\r\n"))
		io.Copy(io.Discard, reader)
	})
	if response, err := Telnet(ctx, chatty, chattyPort, "telnet-secret", "say password is incorrect", Control{}); err != nil || response != "done" {
		t.Fatalf("%q %v", response, err)
	}
	// Without a banner, the first line can be chat or the command's echo.
	quiet, quietPort := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte("Please enter password:"))
		reader.ReadString('\n')
		conn.Write([]byte("2026-11-19 INF Chat 'Bob': password is incorrect\r\n"))
		command, _ := reader.ReadString('\n')
		conn.Write([]byte(command + "done\r\n"))
		io.Copy(io.Discard, reader)
	})
	if response, err := Telnet(ctx, quiet, quietPort, "telnet-secret", "say password is incorrect", Control{}); err != nil || response != "done" {
		t.Fatalf("%q %v", response, err)
	}
}

// A rejection that arrives after the command was sent still fails the
// sign-in instead of becoming the command's output.
func TestTelnetDelayedRejection(t *testing.T) {
	host, port := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte("Please enter password:"))
		reader.ReadString('\n')
		time.Sleep(4 * telnetCommandGap)
		conn.Write([]byte("Password incorrect, please enter password:\r\n"))
		io.Copy(io.Discard, reader)
	})
	if _, err := Telnet(ctx, host, port, "bad", "listplayers", Control{}); err == nil || err.Error() != "Telnet console authentication failed" {
		t.Fatal(err)
	}
}

// A rejection after a streamed log line still fails the sign-in, even when
// the server prompts again instead of hanging up.
func TestTelnetRejectionAfterLog(t *testing.T) {
	host, port := listen(t, func(conn net.Conn) {
		reader := bufio.NewReader(conn)
		conn.Write([]byte("Please enter password:"))
		reader.ReadString('\n')
		conn.Write([]byte("2026-11-19 INF Chat 'Bob': hi\r\nPassword incorrect, please enter password:\r\n"))
		io.Copy(io.Discard, reader)
	})
	if _, err := Telnet(ctx, host, port, "bad", "listplayers", Control{}); err == nil || err.Error() != "Telnet console authentication failed" {
		t.Fatal(err)
	}
}

// A reader stops when its command ends with results unread.
func TestReaderStopsWithCommand(t *testing.T) {
	host, port := listen(t, func(conn net.Conn) {
		chunk := make([]byte, 64*1024)
		for {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
	})
	conn, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	results := reader(conn, 1<<30, errors.New("too large"), done)
	// The command reads one chunk and returns, as on a malformed first
	// packet, while the endpoint keeps sending.
	<-results
	time.Sleep(200 * time.Millisecond)
	close(done)
	conn.Close()
	readers := func() int {
		stacks := make([]byte, 1<<20)
		return strings.Count(string(stacks[:runtime.Stack(stacks, true)]), "netconsole.reader")
	}
	for deadline := time.Now().Add(5 * time.Second); readers() > 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a reader is left waiting to send results no one reads")
		}
	}
}
