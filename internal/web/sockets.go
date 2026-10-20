package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/logging"
)

// Console socket limits. Administrators keep a reserve so a busy panel
// can still be managed.
const (
	maxSockets           = 100
	maxSocketsPerUser    = 12
	maxSocketsPerSession = 8
	reservedAdminSockets = 10
	maxSocketMessage     = 64 * 1024
	maxSocketBuffer      = 1024 * 1024
	socketRecheck        = 15 * time.Second
)

var socketLog = logging.New("websocket")

// sockets tracks open console connections for admission and shutdown.
type sockets struct {
	mu       sync.Mutex
	open     map[*socketChannel]struct{}
	stopping bool
}

// socketChannel adapts a WebSocket to console.Channel. Output is queued so
// a slow reader is disconnected instead of buffering without bound.
type socketChannel struct {
	conn      *websocket.Conn
	userID    string
	tokenHash string
	admin     bool

	mu       sync.Mutex
	queue    [][]byte
	buffered int
	closed   bool
	wake     chan struct{}
	done     chan struct{}
	// finished closes once the close handshake has ended.
	finished chan struct{}
}

func newSocketChannel(conn *websocket.Conn, principal *db.SessionUser, tokenHash string) *socketChannel {
	return &socketChannel{conn: conn, userID: principal.ID, tokenHash: tokenHash, admin: principal.Role == db.RoleAdmin,
		wake: make(chan struct{}, 1), done: make(chan struct{}), finished: make(chan struct{})}
}

func (c *socketChannel) Send(kind, data string) {
	message, _ := json.Marshal(map[string]string{"type": kind, "data": data})
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if c.buffered+len(message) > maxSocketBuffer {
		c.mu.Unlock()
		c.Close(1013, "Client is not reading output")
		return
	}
	c.queue = append(c.queue, message)
	c.buffered += len(message)
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *socketChannel) Close(code int, reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	// An admission placeholder has no connection yet; its handler checks for
	// shutdown after the upgrade and closes the connection itself.
	if c.conn == nil {
		return
	}
	// Let queued output (such as the reason for a policy close) go first.
	go func() {
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
		}
		c.conn.Close(websocket.StatusCode(code), reason)
		close(c.finished)
	}()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *socketChannel) Open() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

// write delivers queued output until the channel closes and drains.
func (c *socketChannel) write(ctx context.Context) {
	defer close(c.done)
	for {
		c.mu.Lock()
		queue := c.queue
		c.queue, c.buffered = nil, 0
		closed := c.closed
		c.mu.Unlock()
		for _, message := range queue {
			writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := c.conn.Write(writeCtx, websocket.MessageText, message)
			cancel()
			if err != nil {
				c.Close(1011, "Output connection failed")
				return
			}
		}
		if closed {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		}
	}
}

func (s *sockets) admit(channel *socketChannel) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = map[*socketChannel]struct{}{}
	}
	if s.stopping {
		return http.StatusServiceUnavailable, "Server shutting down"
	}
	user, session, nonAdmin := 0, 0, 0
	for other := range s.open {
		if other.userID == channel.userID {
			user++
		}
		if other.tokenHash == channel.tokenHash {
			session++
		}
		if !other.admin {
			nonAdmin++
		}
	}
	switch {
	case user >= maxSocketsPerUser:
		return http.StatusTooManyRequests, "WebSocket user connection limit reached"
	case session >= maxSocketsPerSession:
		return http.StatusTooManyRequests, "WebSocket session connection limit reached"
	case len(s.open) >= maxSockets:
		return http.StatusServiceUnavailable, "WebSocket connection limit reached"
	case !channel.admin && nonAdmin >= maxSockets-reservedAdminSockets:
		return http.StatusServiceUnavailable, "WebSocket capacity reserved for administrators"
	}
	s.open[channel] = struct{}{}
	return 0, ""
}

func (s *sockets) remove(channel *socketChannel) {
	s.mu.Lock()
	delete(s.open, channel)
	s.mu.Unlock()
}

// CloseSockets ends every console connection for shutdown. Hijacked
// connections outlive http.Server.Shutdown otherwise.
func (s *Server) CloseSockets() {
	s.sockets.mu.Lock()
	s.sockets.stopping = true
	open := make([]*socketChannel, 0, len(s.sockets.open))
	for channel := range s.sockets.open {
		open = append(open, channel)
	}
	s.sockets.mu.Unlock()
	for _, channel := range open {
		channel.Close(1001, "Server shutting down")
	}
}

// consoleSocket serves Docker logs, a game console, or an administrator
// shell for one server. The session cookie authenticates the upgrade and
// the browser's Origin must match; the console package rechecks access
// before every frame and command.
func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request, request *Request) {
	mode := r.URL.Query().Get("mode")
	if mode != "logs" && mode != "game" && mode != "shell" {
		http.Error(w, "Unknown console mode", http.StatusNotFound)
		return
	}
	if mode == "shell" && request.User().Role != db.RoleAdmin {
		http.Error(w, "Administrator access required", http.StatusForbidden)
		return
	}
	// Admission is counted before the upgrade so concurrent handshakes
	// cannot pass the caps together.
	pending := &socketChannel{userID: request.User().ID, tokenHash: request.Principal.TokenHash,
		admin: request.User().Role == db.RoleAdmin}
	if status, message := s.sockets.admit(pending); status != 0 {
		http.Error(w, message, status)
		return
	}
	defer s.sockets.remove(pending)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxSocketMessage)
	channel := newSocketChannel(conn, request.User(), request.Principal.TokenHash)
	s.sockets.mu.Lock()
	delete(s.sockets.open, pending)
	stopping := s.sockets.stopping
	s.sockets.open[channel] = struct{}{}
	s.sockets.mu.Unlock()
	defer s.sockets.remove(channel)
	if stopping {
		conn.Close(websocket.StatusGoingAway, "Server shutting down")
		return
	}

	// The connection outlives the request context once hijacked.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	go channel.write(ctx)
	token := request.Principal.Token
	validate := func(ctx context.Context) *db.SessionUser {
		principal, err := s.App.Auth.Session(ctx, token)
		if err != nil || principal == nil || (mode == "shell" && principal.User.Role != db.RoleAdmin) {
			return nil
		}
		return &principal.User
	}
	// Sessions expire or are revoked while a socket stays quiet.
	go func() {
		ticker := time.NewTicker(socketRecheck)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if validate(ctx) == nil {
					channel.Close(1008, "Session expired or access revoked")
				}
			}
		}
	}()
	input := make(chan string, 16)
	go func() {
		defer close(input)
		defer cancel()
		for {
			kind, data, err := conn.Read(ctx)
			if err != nil {
				channel.Close(1000, "")
				return
			}
			if kind != websocket.MessageText || validate(ctx) == nil {
				if kind == websocket.MessageText {
					channel.Close(1008, "Session expired or access revoked")
				}
				continue
			}
			select {
			case input <- string(data):
			case <-ctx.Done():
				return
			}
		}
	}()
	ip := request.Client.IP
	id := r.PathValue("id")
	switch mode {
	case "logs":
		go func() {
			for range input {
			}
		}()
		s.App.Sessions.Logs(ctx, channel, validate, id, ip)
	case "game":
		s.App.Sessions.Console(ctx, channel, validate, id, console.GameMode, input, ip)
	case "shell":
		s.App.Sessions.Console(ctx, channel, validate, id, console.ShellMode, input, ip)
	}
	// Logs end when the stream does; consoles when the browser leaves.
	if channel.Open() && mode == "logs" {
		<-ctx.Done()
	}
	channel.Close(1000, "")
	// Cancelling the context tears the connection down, so let the close
	// handshake (and a policy close code) finish first.
	<-channel.finished
	socketLog.Debug("WebSocket closed", logging.F("mode", mode))
}
