package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

var (
	errNoModeration = apperr.New("PLAYER_ACTION_UNSUPPORTED", 409, "Ludock can’t act on players on this game")
	errSecretName   = apperr.New("PLAYER_NAME_INVALID", 400, "That isn’t a player name Ludock can send. Use the console.")
)

// moderation is the server's console moderation, or nil.
func moderation(container servers.Container) *games.Moderation {
	if adapter := games.ResolveAdapter(container.GameType, container.Labels); adapter != nil {
		return adapter.Moderation
	}
	return nil
}

// playerAction sends one player action as a console command: the same
// capability, recheck, lock, binding, and adapter as a typed command.
func (s *Server) playerAction(w http.ResponseWriter, r *http.Request, request *Request) {
	id, action, name := r.PathValue("id"), games.PlayerAction(r.PathValue("action")), r.PostFormValue("player")
	lists := htmxTarget(r) == "player-lists-body"
	act, err := s.startAction(r, request, id, authz.ConsoleExecute)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	reply, err := s.sendPlayerAction(r, request, act, action, name)
	var panel *views.PlayerListsView
	if lists {
		panel = s.playerLists(act)
	}
	act.Done()
	// The list the account check compared may be one the status query
	// vouched for in an older order, so the refusal waits for a new one for
	// the next attempt.
	if errors.Is(err, errUnconfirmed) {
		s.App.Players.CheckAndWait(r.Context(), id)
	}
	if err != nil {
		if public, ok := apperr.As(err); ok && panel != nil && public.Status != http.StatusUnauthorized {
			panel.Alert = views.PlayerAlert{Kind: "error", Message: public.Message}
			render(w, r, http.StatusOK, views.PlayerLists(*panel))
			return
		}
		s.fail(w, r, request, err)
		return
	}
	// The command released the lock, so a reading can take it.
	s.App.Players.Check(context.WithoutCancel(r.Context()), id)
	alert := playerResult(moderation(act.Container), action, name, reply)
	if panel != nil {
		panel.Alert = alert
		render(w, r, http.StatusOK, views.PlayerLists(*panel))
		return
	}
	// A ban from the online list changes the ban list.
	w.Header().Set("HX-Trigger", "player-lists-changed")
	render(w, r, http.StatusOK, views.PlayerResult(alert))
}

func (s *Server) sendPlayerAction(r *http.Request, request *Request, act *serverAction, action games.PlayerAction, name string) (string, error) {
	adapter := games.ResolveAdapter(act.Container.GameType, act.Container.Labels)
	if adapter == nil || adapter.Moderation == nil {
		return "", errNoModeration
	}
	// A typed name that holds a secret never reaches the audit log or page.
	if !games.Online(action) && players.Redacted(name, act.Observation.Secrets) {
		return "", errSecretName
	}
	reading := s.App.Players.Get(act.Logical.ID, act.Container)
	command, err := players.Command(adapter.Moderation, reading, action, name, r.PostFormValue("key"))
	if err != nil {
		return "", err
	}
	if err := act.Assert(); err != nil {
		return "", err
	}
	details := map[string]any{"action": string(action), "player": name, "containerId": act.Logical.ContainerID,
		"bindingRevision": act.Logical.BindingRevision}
	// Like a typed command, it is sent only once its audit entry is saved.
	entry := s.auditEntry(request)
	entry.Action, entry.TargetType, entry.TargetID, entry.Details = "server.player-action.started", "server", act.Logical.ID, details
	started, err := s.App.DB.WriteAuditStart(act.Ctx, entry)
	if err != nil {
		return "", apperr.New("AUDIT_UNAVAILABLE", 503, "Ludock could not record this action, so it was not sent. Try again.")
	}
	target := console.Target{ContainerID: act.Logical.ContainerID, Labels: act.Container.Labels, State: act.Container.State}
	if accounts := adapter.Moderation.Accounts; accounts != nil && games.Online(action) {
		if err := s.confirmAccount(act, target, adapter, accounts, command, reading); err != nil {
			s.finishAudit(entry, started, "server.player-action.failed")
			return "", err
		}
	}
	var reply strings.Builder
	secrets := act.Observation.Secrets
	out := console.NewRedactor(secrets, func(value string) {
		if reply.Len() < 4096 {
			reply.WriteString(value)
		}
	})
	err = s.App.Console.GameCommand(act.Ctx, target, adapter, command, console.Output{Stdout: out.Push, Complete: out.End}, act.Assert)
	if err != nil {
		s.finishAudit(entry, started, "server.player-action.failed")
		if _, public := apperr.As(err); public {
			return "", err
		}
		return "", apperr.New("GAME_COMMAND_FAILED", 502, "The game didn’t take the command, or other work on the server is running")
	}
	// The game saying no such player is a failed action, not a sent one.
	if adapter.Moderation.Outcome(action, reply.String()) == games.NoPlayer {
		s.finishAudit(entry, started, "server.player-action.failed")
	} else {
		s.finishAudit(entry, started, "server.player-action.succeeded")
	}
	return reply.String(), nil
}

var errUnconfirmed = apperr.New("PLAYER_NOT_CONFIRMED", 409,
	"The game didn’t confirm who’s online by account, so nothing was sent. Try again, or use the console.")

// confirmAccount asks the game, under the action's lock, for the online
// account names, and goes ahead only when they are exactly the names the
// reading listed, row by row. A display name a team prefix, suffix, or
// plugin changed, even into another player's name, then stops every kick
// and ban.
func (s *Server) confirmAccount(act *serverAction, target console.Target, adapter *games.Adapter, accounts *games.PlayerQuery,
	command string, reading *players.Reading) error {
	var reply strings.Builder
	err := s.App.Console.GameCommand(act.Ctx, target, adapter, accounts.Command, console.Output{Stdout: func(value string) {
		if reply.Len() < 1<<20 {
			reply.WriteString(value)
		}
	}}, act.Assert)
	if err != nil {
		return err
	}
	online, ok := accounts.Parse(reply.String())
	// Both replies list the server's players in the same order, so each row
	// must be the same account, not only the same names.
	if !ok || reading == nil || online.Online != reading.Online || len(online.IDs) != online.Online ||
		!slices.Equal(reading.IDs, online.IDs) {
		return errUnconfirmed
	}
	// Minecraft's commands end with their target.
	for _, id := range online.IDs {
		if strings.HasSuffix(command, " "+id) {
			return nil
		}
	}
	return errUnconfirmed
}

// playerLists reads the server's lists for its panel, while act holds the
// server after a command.
func (s *Server) playerLists(act *serverAction) *views.PlayerListsView {
	return s.readPlayerLists(act.Ctx, act.Context, act.Assert)
}

func (s *Server) readPlayerLists(ctx context.Context, server *servers.Context, assert func() error) *views.PlayerListsView {
	panel := &views.PlayerListsView{ServerID: server.Logical.ID, Moderation: moderation(server.Container),
		Running: server.Container.State == "running"}
	lists, err := players.ReadLists(ctx, s.App.Docker, server.Logical.ContainerID, panel.Moderation, server.Observation.Secrets)
	if err == nil {
		err = assert()
	}
	if err != nil {
		panel.Error = publicMessage(err)
		return panel
	}
	panel.Lists = lists
	return panel
}

// playerListsFragment reads the lists without the server lock, which would
// cancel a player reading for something that changes nothing.
func (s *Server) playerListsFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	resolved, err := s.App.Servers.VerifyAuthorized(ctx, request.User(), r.PathValue("id"), authz.ConsoleExecute, -1)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	panel := s.readPlayerLists(ctx, resolved, func() error {
		current, err := s.App.Auth.Recheck(ctx, request.Principal)
		if err == nil {
			_, err = s.App.Authz.Assert(ctx, &current.User, resolved.Logical.ID, authz.ConsoleExecute)
		}
		if err == nil {
			_, err = s.App.Identity.ResolveBinding(ctx, resolved.Logical.ID, resolved.Logical.BindingRevision)
		}
		return err
	})
	render(w, r, http.StatusOK, views.PlayerLists(*panel))
}

// playerResult says in plain words what the game replied.
func playerResult(moderation *games.Moderation, action games.PlayerAction, name, reply string) views.PlayerAlert {
	done := map[games.PlayerAction]string{games.Kick: name + " was kicked.", games.Ban: name + " is banned.",
		games.Unban: name + "’s ban is lifted.", games.Allow: name + " is on the allowlist.",
		games.Disallow: name + " is off the allowlist."}
	unchanged := map[games.PlayerAction]string{games.Ban: name + " was already banned.", games.Unban: name + " wasn’t banned.",
		games.Allow: name + " was already on the allowlist.", games.Disallow: name + " wasn’t on the allowlist."}
	switch moderation.Outcome(action, reply) {
	case games.Done:
		return views.PlayerAlert{Kind: "success", Message: done[action]}
	case games.Unchanged:
		return views.PlayerAlert{Kind: "info", Message: unchanged[action]}
	case games.NoPlayer:
		if action == games.Kick {
			return views.PlayerAlert{Kind: "error", Message: name + " isn’t online."}
		}
		return views.PlayerAlert{Kind: "error", Message: "The game doesn’t know a player named " + name + "."}
	}
	sent := map[games.PlayerAction]string{games.Kick: "Asked the game to kick " + name + ".",
		games.Ban: "Asked the game to ban " + name + ".", games.Unban: "Asked the game to lift " + name + "’s ban.",
		games.Allow: "Asked the game to allow " + name + ".", games.Disallow: "Asked the game to remove " + name + " from the allowlist."}
	alert := views.PlayerAlert{Kind: "success", Message: sent[action]}
	reply = strings.Join(strings.Fields(reply), " ")
	if reply != "" && reply != "Command completed with no response" {
		if utf8.RuneCountInString(reply) > 200 {
			reply = string([]rune(reply)[:200]) + "…"
		}
		alert.Reply = reply
	}
	return alert
}

// finishAudit sets a started entry to its outcome. The action already ran, so
// a lost entry is logged.
func (s *Server) finishAudit(entry db.AuditEntry, started int64, action string) {
	entry.Action = action
	if err := s.App.DB.FinishAudit(context.Background(), started, entry); err != nil {
		log.Error("Could not write an audit entry", logging.F("action", action))
	}
}
