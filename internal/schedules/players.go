package schedules

import (
	"context"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/servers"
)

// Players is how runs that wait for an empty server read who is online.
type Players interface {
	// Online is how many players the server has and whether that is known,
	// after confirming the user's capability and the binding revision.
	Online(ctx context.Context, user *db.SessionUser, serverID string, capability authz.Capability, revision int64) (int, bool, error)
	// Lists reports whether Ludock can read the server's players.
	Lists(ctx context.Context, user *db.SessionUser, serverID string) (bool, error)
	// Check asks for a fresh reading without waiting for it.
	Check(ctx context.Context, serverID string)
}

// ServerPlayers reads players through the players service.
type ServerPlayers struct {
	Servers *servers.Service
	Players *players.Service
}

func (p ServerPlayers) Online(ctx context.Context, user *db.SessionUser, serverID string, capability authz.Capability, revision int64) (int, bool, error) {
	server, err := p.Servers.VerifyAuthorized(ctx, user, serverID, capability, revision)
	if err != nil {
		return 0, false, err
	}
	switch server.Container.State {
	case "running", "paused", "restarting":
	default:
		// Nobody plays on a stopped server.
		return 0, true, nil
	}
	if !p.Players.Source.Lists(server.Container) {
		return 0, false, nil
	}
	online, known := p.Players.Online(serverID, server.Container)
	return online, known, nil
}

func (p ServerPlayers) Lists(ctx context.Context, user *db.SessionUser, serverID string) (bool, error) {
	server, err := p.Servers.VerifyAuthorized(ctx, user, serverID, authz.ServerView, -1)
	if err != nil {
		return false, err
	}
	return p.Players.Source.Lists(server.Container), nil
}

func (p ServerPlayers) Check(ctx context.Context, serverID string) { p.Players.Check(ctx, serverID) }
