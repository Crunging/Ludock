package console

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/servers"
)

// Channel is one browser connection (WebSocket or SSE).
type Channel interface {
	// Send delivers {type, data}; types are stdout, stderr, system, error.
	Send(kind, data string)
	Close(code int, reason string)
	Open() bool
}

// Policy close codes.
const (
	ClosePolicy   = 1008
	CloseInternal = 1011
)

// Sessions opens console and log streams.
type Sessions struct {
	DB       *db.DB
	Authz    *authz.Service
	Identity *identity.Store
	Servers  *servers.Service
	Locks    *locks.Table
	Runtime  *Runtime
	// IdleCheck is how often an idle stream inspects its container.
	IdleCheck time.Duration
	// Commanded, when set, is told after a game command succeeds, since it
	// may have changed who is online. It must not block.
	Commanded func(serverID string)
}

// Access keeps a stream attached to the exact binding used when it opened.
// Local revocation is checked before every output frame, and the container
// is inspected while idle so external recreation also closes a silent
// connection.
type Access struct {
	sessions   *Sessions
	channel    Channel
	validate   func(ctx context.Context) *db.SessionUser
	ServerID   string
	capability authz.Capability
	Context    *servers.Context
	revision   int64
	mu         sync.Mutex
	user       *db.SessionUser
	stop       context.CancelFunc
}

// Authorize resolves the server for capability, or reports why not and
// closes the channel. validate returns the current principal.
func (s *Sessions) Authorize(ctx context.Context, channel Channel, validate func(context.Context) *db.SessionUser,
	serverID string, capability authz.Capability, unavailable string) *Access {
	fail := func() *Access {
		channel.Send("error", unavailable)
		channel.Close(ClosePolicy, "Server access unavailable")
		return nil
	}
	user := validate(ctx)
	if user == nil || !identity.ValidID(serverID) {
		return fail()
	}
	serverContext, err := s.Servers.ResolveAuthorized(ctx, user, serverID, capability, -1)
	if err != nil {
		return fail()
	}
	access := &Access{sessions: s, channel: channel, validate: validate, ServerID: serverID, capability: capability,
		Context: serverContext, revision: serverContext.Logical.BindingRevision, user: user}
	if !access.Allowed(capability) {
		return nil
	}
	watch, stop := context.WithCancel(ctx)
	access.stop = stop
	interval := s.IdleCheck
	if interval == 0 {
		interval = 2 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-watch.Done():
				return
			case <-ticker.C:
				// The idle check inspects only this container; commands
				// rediscover the fleet before they run.
				access.revalidate(watch, false)
			}
		}
	}()
	return access
}

// Stop ends idle revalidation.
func (a *Access) Stop() {
	if a.stop != nil {
		a.stop()
	}
}

// User is the latest validated principal.
func (a *Access) User() *db.SessionUser {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.user
}

// Allowed rechecks the principal, both capabilities, and the binding. A
// failure closes the channel.
func (a *Access) Allowed(required authz.Capability) bool {
	if !a.channel.Open() {
		return false
	}
	ctx := context.Background()
	user := a.validate(ctx)
	var capabilities []authz.Capability
	var binding *identity.Server
	if user != nil {
		// One query reads the grant and binding together.
		capabilities, binding, _ = a.sessions.Authz.Access(ctx, user, a.ServerID)
	}
	if user == nil || !slices.Contains(capabilities, a.capability) || !slices.Contains(capabilities, required) ||
		binding == nil || binding.BindingRevision != a.revision || binding.ContainerID != a.Context.Logical.ContainerID {
		a.channel.Close(ClosePolicy, "Server access changed")
		return false
	}
	a.mu.Lock()
	a.user = user
	a.mu.Unlock()
	return true
}

func (a *Access) revalidate(ctx context.Context, discover bool) (*servers.Context, error) {
	if !a.Allowed(a.capability) {
		return nil, errCancelled
	}
	resolve := a.sessions.Servers.VerifyAuthorized
	if discover {
		resolve = a.sessions.Servers.ResolveAuthorized
	}
	refreshed, err := resolve(ctx, a.User(), a.ServerID, a.capability, a.revision)
	if err != nil || !a.Allowed(a.capability) {
		a.channel.Close(ClosePolicy, "Server access changed")
		return nil, errCancelled
	}
	return refreshed, nil
}

// Refresh rediscovers the fleet and confirms the binding again.
func (a *Access) Refresh(ctx context.Context) (*servers.Context, error) {
	return a.revalidate(ctx, true)
}

// audit records a console event. Sessions and commands start only once
// their entry is saved; later entries are logged if they cannot be.
func (s *Sessions) audit(user *db.SessionUser, action, serverID string, details map[string]any, ip string) error {
	err := s.DB.WriteAudit(context.Background(), nil, db.AuditEntry{UserID: user.ID, Action: action, TargetType: "server",
		TargetID: serverID, Details: details, IPAddress: ip})
	if err != nil {
		log.Error("Could not write an audit entry", logging.F("action", action))
	}
	return err
}

// unrecorded ends a connection whose opening could not be audited.
func unrecorded(channel Channel) {
	channel.Send("error", "Ludock could not record this connection, so it was not opened. Try again.")
	channel.Close(CloseInternal, "Audit unavailable")
}

// Logs follows a server's logs until ctx ends or access changes.
func (s *Sessions) Logs(ctx context.Context, channel Channel, validate func(context.Context) *db.SessionUser, serverID, ip string) {
	access := s.Authorize(ctx, channel, validate, serverID, authz.LogsRead, "Server logs are unavailable or access was denied")
	if access == nil {
		return
	}
	defer access.Stop()
	containerID := access.Context.Logical.ContainerID
	if err := s.audit(access.User(), "server.logs.opened", serverID, map[string]any{"containerId": containerID,
		"bindingRevision": access.Context.Logical.BindingRevision}, ip); err != nil {
		unrecorded(channel)
		return
	}
	log.Info("Docker log connection opened", logging.F("container", short(containerID)), logging.F("role", string(access.User().Role)))
	send := func(kind, data string) {
		if access.Allowed(authz.LogsRead) {
			channel.Send(kind, data)
		}
	}
	send("system", "Following Docker logs")
	err := Follow(ctx, s.Runtime.Docker, containerID, FollowOptions{Tail: 500, Timestamps: true, Secrets: access.Context.Observation.Secrets,
		Allowed: func() bool { return access.Allowed(authz.LogsRead) }, Send: send,
		EndedMessage: "Log stream ended (container may have stopped)"})
	if err != nil {
		// Nothing else uses this stream; closing lets the browser reconnect.
		channel.Close(CloseInternal, "Docker logs unavailable")
	}
}

// Mode is a console kind.
type Mode string

const (
	GameMode  Mode = "game"
	ShellMode Mode = "shell"
)

const maxPending = 10

// Console runs a game console or administrator shell. input carries raw
// client messages, {"type":"input","data":"..."}; it closes with the
// connection.
func (s *Sessions) Console(ctx context.Context, channel Channel, validate func(context.Context) *db.SessionUser, serverID string,
	mode Mode, input <-chan string, ip string) {
	capability := authz.ConsoleExecute
	if mode == ShellMode {
		capability = authz.ConsoleShell
	}
	access := s.Authorize(ctx, channel, validate, serverID, capability, "Server console is unavailable or access was denied")
	if access == nil {
		return
	}
	defer access.Stop()
	adapter := games.ResolveAdapter(access.Context.Container.GameType, access.Context.Container.Labels)
	if mode == GameMode && adapter == nil {
		channel.Send("error", "This server has no supported game console adapter")
		channel.Close(ClosePolicy, "Console unavailable")
		return
	}
	containerID := access.Context.Logical.ContainerID
	secrets := access.Context.Observation.Secrets
	send := func(kind, data string) {
		if access.Allowed(capability) {
			channel.Send(kind, data)
		}
	}
	opened := "server.game-console.opened"
	prefix := "server.game-command"
	if mode == ShellMode {
		opened, prefix = "server.shell.opened", "server.shell"
	}
	if err := s.audit(access.User(), opened, serverID, map[string]any{"containerId": containerID,
		"bindingRevision": access.Context.Logical.BindingRevision}, ip); err != nil {
		unrecorded(channel)
		return
	}
	if mode == GameMode {
		send("system", "Connected to "+adapter.Name)
	} else {
		send("system", "Administrator shell. Commands require the container's timeout utility and run for at most 60 seconds.")
	}

	commands := make(chan string, maxPending)
	go func() {
		defer close(commands)
		for raw := range input {
			if !access.Allowed(capability) {
				continue
			}
			select {
			case commands <- raw:
			default:
				send("error", "Too many commands queued")
			}
		}
	}()
	// A console grant never grants general log reading, including the
	// initial history usually shown in game consoles.
	if ok, _ := s.Authz.Has(ctx, access.User(), serverID, authz.LogsRead); ok {
		go Follow(ctx, s.Runtime.Docker, containerID, FollowOptions{Tail: 200, Secrets: secrets,
			Allowed: func() bool { return access.Allowed(authz.LogsRead) },
			Send: func(kind, data string) {
				if kind == "stdout" || kind == "stderr" {
					if access.Allowed(authz.LogsRead) {
						channel.Send(kind, data)
					}
				} else if access.Allowed(capability) {
					channel.Send(kind, data)
				}
			}, EndedMessage: "Log stream ended"})
	}
	for raw := range commands {
		if !access.Allowed(capability) {
			continue
		}
		// The command's lock is released by now, so a reading can take it.
		if s.execute(ctx, access, mode, raw, secrets, send, prefix, ip) && mode == GameMode && s.Commanded != nil {
			s.Commanded(serverID)
		}
	}
}

// execute runs one command and reports whether it succeeded.
func (s *Sessions) execute(ctx context.Context, access *Access, mode Mode, raw string, secrets []string, send func(string, string), prefix, ip string) bool {
	var message struct {
		Type string  `json:"type"`
		Data *string `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &message) != nil {
		send("error", "Invalid message format (expected JSON)")
		return false
	}
	if message.Type != "input" || message.Data == nil {
		send("error", `Expected { type: "input", data: "..." }`)
		return false
	}
	command := strings.TrimSpace(*message.Data)
	if command == "" {
		return false
	}
	limit := 1024
	if mode == ShellMode {
		limit = 4096
	}
	if len([]rune(command)) > limit {
		send("error", "Command exceeds the "+itoa(limit)+" character limit")
		return false
	}
	failed := func() {
		log.Warn("Console command could not complete", logging.F("serverId", access.ServerID), logging.F("mode", string(mode)))
		if mode == GameMode {
			send("error", "Game command failed or a conflicting operation is running")
		} else {
			send("error", "Shell command failed. Ensure timeout is installed; commands are limited to 60 seconds.")
		}
	}
	release, err := s.Locks.AcquirePreempting(ctx, access.Context.LockKeys, nil)
	if err != nil {
		failed()
		return false
	}
	defer release()
	current, err := access.Refresh(ctx)
	if err != nil {
		failed()
		return false
	}
	capability := authz.ConsoleExecute
	if mode == ShellMode {
		capability = authz.ConsoleShell
	}
	assertAccess := func() error {
		if !access.Allowed(capability) {
			return errCancelled
		}
		return nil
	}
	adapter := games.ResolveAdapter(current.Container.GameType, current.Container.Labels)
	if mode == GameMode && adapter == nil {
		failed()
		return false
	}
	stdout := NewRedactor(secrets, func(value string) { send("stdout", value) })
	stderr := NewRedactor(secrets, func(value string) { send("stderr", value) })
	output := Output{Stdout: stdout.Push, Stderr: stderr.Push, Complete: func() { stdout.End(); stderr.End() },
		System: func(value string) {
			redactor := NewRedactor(secrets, func(safe string) { send("system", safe) })
			redactor.Push(value)
			redactor.End()
		}}
	user := access.User()
	// A command runs only once its audit entry is saved.
	if err := s.audit(user, prefix+".started", access.ServerID, map[string]any{"containerId": current.Logical.ContainerID,
		"bindingRevision": current.Logical.BindingRevision}, ip); err != nil {
		send("error", "Ludock could not record this command, so it was not sent. Try again.")
		return false
	}
	if mode == GameMode {
		err = s.Runtime.GameCommand(ctx, Target{ContainerID: current.Logical.ContainerID, Labels: current.Container.Labels,
			State: current.Container.State}, adapter, command, output, assertAccess)
	} else {
		err = s.Runtime.Shell(ctx, current.Logical.ContainerID, command, output, assertAccess)
	}
	if err != nil {
		s.audit(user, prefix+".failed", access.ServerID, nil, "")
		failed()
		return false
	}
	s.audit(user, prefix+".succeeded", access.ServerID, nil, "")
	return true
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// Broadcaster sends schedule countdown warnings through the console.
type Broadcaster struct{ Runtime *Runtime }

// Broadcast implements schedules.Broadcaster.
func (b Broadcaster) Broadcast(ctx context.Context, server *servers.Context, command string, assertAccess func() error) error {
	adapter := games.ResolveAdapter(server.Container.GameType, server.Container.Labels)
	if adapter == nil {
		return errCancelled
	}
	return b.Runtime.GameCommand(ctx, Target{ContainerID: server.Logical.ContainerID, Labels: server.Container.Labels,
		State: server.Container.State}, adapter, command, Discard, assertAccess)
}
