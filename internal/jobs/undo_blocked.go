package jobs

import (
	"context"

	"github.com/Crunging/Ludock/internal/servers"
)

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
