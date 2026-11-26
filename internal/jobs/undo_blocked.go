package jobs

import (
	"context"

	"github.com/Crunging/Ludock/internal/servers"
)

// PendingRestoreServers lists, in one query, the servers with a restore that
// stopped partway, the only ones whose undo can be blocked.
func (j *Jobs) PendingRestoreServers(ctx context.Context) (map[string]bool, error) {
	rows, err := j.DB.QueryContext(ctx, "SELECT DISTINCT server_id FROM operations WHERE "+PendingRestore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	servers := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		servers[id] = true
	}
	return servers, rows.Err()
}

// UndoBlocked reports a restore of the server that stopped partway and that
// finishPending would refuse to undo, read without stopping anything: its
// journal no longer matches the server's data folders, or its game changed.
// Its journal is valid, since PendingRestore selects only readable ones.
func (j *Jobs) UndoBlocked(ctx context.Context, server *servers.Context) (bool, error) {
	pending, err := j.pendingRestores(ctx, server.Logical.ID)
	if err != nil {
		return false, err
	}
	for _, restore := range pending {
		state, err := restoreState(restore)
		if err != nil || state == nil || !journalMatches(state.Roots, server) || restore.Job.Recovery.Fingerprint != server.Logical.BindingFingerprint {
			return true, nil
		}
	}
	return false, nil
}
