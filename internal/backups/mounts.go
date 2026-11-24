package backups

import (
	"context"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/servers"
)

// AssertOwnMounts refuses, before the server stops, data folders a backup
// can't take because the host has another mount on or inside one, and
// returns the restore stage folders the backup will leave out. It starts a
// read-only helper labelled with the operation.
func (s *Service) AssertOwnMounts(ctx context.Context, server *servers.Context, operationID string) ([]StageFolder, error) {
	data, err := s.Helpers.NewDataHelper(ctx, server, true, operationID)
	if err != nil {
		return nil, err
	}
	defer data.Cleanup()
	return s.CheckOwnMounts(ctx, server, data, "", nil)
}

// CheckOwnMounts is AssertOwnMounts through a helper already running. The
// outcome is remembered for the server's readiness, which starts no helper.
func (s *Service) CheckOwnMounts(ctx context.Context, server *servers.Context, data *DataHelper, item string, assertAccess func() error) ([]StageFolder, error) {
	stages, err := s.Helpers.AssertOwnMounts(ctx, server, data, item, assertAccess)
	s.NoteHostMount(server, err)
	return stages, err
}

// NoteHostMount remembers whether work on the server last found a mount the
// host added in its data. Anything else learns nothing.
func (s *Service) NoteHostMount(server *servers.Context, err error) {
	switch {
	case apperr.Is(err, ErrHostMount.Code):
		s.mountedData.Store(server.Logical.ID, server.Logical.BindingRevision)
	case err == nil:
		s.mountedData.Delete(server.Logical.ID)
	}
}

// HostMounted reports that the last check of the server's data, as Docker
// defines it at revision, found a mount the host added in it; a changed
// definition is checked afresh.
func (s *Service) HostMounted(serverID string, revision int64) bool {
	found, ok := s.mountedData.Load(serverID)
	return ok && found == revision
}

func (s *Service) hostMountIssue(server *servers.Context) *Issue {
	if !s.HostMounted(server.Logical.ID, server.Logical.BindingRevision) {
		return nil
	}
	return &Issue{Code: ErrHostMount.Code, Message: ErrHostMount.Message + " Choose Check again once it is fixed."}
}

// ForgetHostMount drops the remembered finding, as when someone asks for the
// readiness to be checked again after fixing it.
func (s *Service) ForgetHostMount(serverID string) { s.mountedData.Delete(serverID) }
