package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

func (p *pages) dial(t *testing.T, server *httptest.Server, principal *auth.Principal, mode, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	if principal != nil {
		header.Set("Cookie", auth.SessionCookie+"="+principal.Token)
	}
	if origin == "" {
		origin = server.URL
	}
	header.Set("Origin", origin)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/console/"+p.server+"?mode="+mode,
		&websocket.DialOptions{HTTPHeader: header})
	if conn != nil {
		t.Cleanup(func() { conn.CloseNow() })
	}
	return conn, response, err
}

func readMessage(t *testing.T, conn *websocket.Conn) (map[string]string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var message map[string]string
	json.Unmarshal(data, &message)
	return message, nil
}

func TestConsoleSocketAuthorization(t *testing.T) {
	p := newPages(t)
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.Logs = []string{"world loaded"} })
	server := httptest.NewServer(p.handler)
	t.Cleanup(server.Close)

	if _, response, err := p.dial(t, server, nil, "logs", ""); err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatal("anonymous", err)
	}
	if _, response, err := p.dial(t, server, p.admin, "logs", "https://attacker.example"); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("cross-origin", err)
	}
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView)
	if _, response, err := p.dial(t, server, operator, "shell", ""); err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatal("operator shell", err)
	}
	// Without the logs grant the socket opens, explains, and closes with a
	// policy code the page treats as access denied.
	conn, _, err := p.dial(t, server, operator, "logs", "")
	if err != nil {
		t.Fatal(err)
	}
	message, _ := readMessage(t, conn)
	if message["type"] != "error" {
		t.Fatal(message)
	}
	if _, err := readMessage(t, conn); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatal("close", err)
	}

	p.grant(operator, authz.ServerView, authz.LogsRead)
	conn, _, err = p.dial(t, server, operator, "logs", "")
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for !strings.Contains(output.String(), "world loaded") {
		message, err := readMessage(t, conn)
		if err != nil {
			t.Fatal(output.String(), err)
		}
		output.WriteString(message["data"])
	}
	// Signing out ends the stream with a policy close.
	operatorPrincipal := operator
	if err := p.app.Auth.Logout(context.Background(), operatorPrincipal.Token, auth.Client{}); err != nil {
		t.Fatal(err)
	}
	conn.Write(context.Background(), websocket.MessageText, []byte(`{"type":"input","data":"x"}`))
	for {
		if _, err := readMessage(t, conn); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatal("after sign-out", err)
			}
			break
		}
	}
}

func TestConsoleSocketCaps(t *testing.T) {
	p := newPages(t)
	server := httptest.NewServer(p.handler)
	t.Cleanup(server.Close)
	for index := range maxSocketsPerSession {
		if _, _, err := p.dial(t, server, p.admin, "logs", ""); err != nil {
			t.Fatal(index, err)
		}
	}
	if _, response, err := p.dial(t, server, p.admin, "logs", ""); err == nil || response.StatusCode != http.StatusTooManyRequests {
		t.Fatal("ninth socket", err)
	}
}

func TestShutdownSkipsPendingAdmissions(t *testing.T) {
	server := &Server{}
	// A request between admission and the upgrade has no connection yet.
	pending := &socketChannel{userID: "u", tokenHash: "t"}
	if status, _ := server.sockets.admit(pending); status != 0 {
		t.Fatal(status)
	}
	server.CloseSockets()
	pending.Close(1001, "again")
	if status, _ := server.sockets.admit(&socketChannel{userID: "v", tokenHash: "w"}); status != http.StatusServiceUnavailable {
		t.Fatal("admitted a socket during shutdown")
	}
}

func TestShutdownEndsEventStreams(t *testing.T) {
	p := newPages(t)
	server := httptest.NewServer(p.handler)
	t.Cleanup(server.Close)
	request, _ := http.NewRequest("GET", server.URL+"/events", nil)
	request.Header.Set("Cookie", auth.SessionCookie+"="+p.admin.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatal(response, err)
	}
	defer response.Body.Close()
	ended := make(chan struct{})
	go func() {
		io.Copy(io.Discard, response.Body)
		close(ended)
	}()
	p.web.CloseStreams()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("event stream stayed open after shutdown began")
	}
}

// Each event stream stays open for the session, so one session's streams
// are capped.
func TestEventStreamLimit(t *testing.T) {
	p := newPages(t)
	server := httptest.NewServer(p.handler)
	t.Cleanup(server.Close)
	open := func() *http.Response {
		request, _ := http.NewRequest("GET", server.URL+"/events", nil)
		request.Header.Set("Cookie", auth.SessionCookie+"="+p.admin.Token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	var streams []*http.Response
	for range maxStreamsPerSession {
		response := open()
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.Status)
		}
		streams = append(streams, response)
	}
	if extra := open(); extra.StatusCode != http.StatusTooManyRequests {
		t.Fatal("opened a stream past the session limit:", extra.Status)
	}
	// Closing one frees its place.
	streams[0].Body.Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		response := open()
		if response.StatusCode == http.StatusOK {
			response.Body.Close()
			break
		}
		response.Body.Close()
		if time.Now().After(deadline) {
			t.Fatal("a closed stream kept its place")
		}
	}
	for _, response := range streams[1:] {
		response.Body.Close()
	}
}
