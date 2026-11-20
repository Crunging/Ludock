package schedules

import (
	"context"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/players"
	"github.com/Crunging/Ludock/internal/servers"
)

// Occupancy is who is online on a server. A stopped server has nobody.
type Occupancy struct {
	Online         int
	Known, Stopped bool
	// ReadAt is when a known count was read, in Unix milliseconds.
	ReadAt int64
}

// Players is how runs that wait for an empty server read who is online.
type Players interface {
	// Online reads the server's occupancy after confirming the user's
	// capability and the binding revision.
	Online(ctx context.Context, user *db.SessionUser, serverID string, capability authz.Capability, revision int64) (Occupancy, error)
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

func (p ServerPlayers) Online(ctx context.Context, user *db.SessionUser, serverID string, capability authz.Capability, revision int64) (Occupancy, error) {
	server, err := p.Servers.VerifyAuthorized(ctx, user, serverID, capability, revision)
	if err != nil {
		return Occupancy{}, err
	}
	switch server.Container.State {
	case "running", "paused", "restarting":
	default:
		return Occupancy{Known: true, Stopped: true}, nil
	}
	if !p.Players.Source.Lists(server.Container) {
		return Occupancy{}, nil
	}
	online, readAt, known := p.Players.ListedAt(serverID, server.Container)
	return Occupancy{Online: online, Known: known, ReadAt: readAt}, nil
}

func (p ServerPlayers) Lists(ctx context.Context, user *db.SessionUser, serverID string) (bool, error) {
	server, err := p.Servers.VerifyAuthorized(ctx, user, serverID, authz.ServerView, -1)
	if err != nil {
		return false, err
	}
	return p.Players.Source.Lists(server.Container), nil
}

func (p ServerPlayers) Check(ctx context.Context, serverID string) { p.Players.Check(ctx, serverID) }
