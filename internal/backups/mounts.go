package backups

import (
	"context"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/servers"
)

// AssertOwnMounts refuses, before the server stops, data folders a backup
// can't take because the host has another mount on or inside one. It starts
// a read-only helper labelled with the operation.
func (s *Service) AssertOwnMounts(ctx context.Context, server *servers.Context, operationID string) error {
	data, err := s.Helpers.NewDataHelper(ctx, server, true, operationID)
	if err != nil {
		return err
	}
	defer data.Cleanup()
	return s.CheckOwnMounts(ctx, server, data, "", nil)
}

// CheckOwnMounts is AssertOwnMounts through a helper already running. The
// outcome is remembered for the server's readiness, which starts no helper.
func (s *Service) CheckOwnMounts(ctx context.Context, server *servers.Context, data *DataHelper, item string, assertAccess func() error) error {
	err := s.Helpers.AssertOwnMounts(ctx, server, data, item, assertAccess)
	s.NoteHostMount(server, err)
	return err
}

// NoteHostMount remembers whether work on the server last found a mount the
// host added in its data. Anything else learns nothing.
func (s *Service) NoteHostMount(server *servers.Context, err error) {
	switch {
	case apperr.Is(err, ErrHostMount.Code):
		s.mountedData.Store(server.Logical.ID, server.Logical.BindingFingerprint)
	case err == nil:
		s.mountedData.Delete(server.Logical.ID)
	}
}

// HostMounted reports that the last check of the server's data found a mount
// the host added in it.
func (s *Service) HostMounted(serverID string) bool {
	_, found := s.mountedData.Load(serverID)
	return found
}

// hostMountIssue is the remembered finding for the server as Docker shows it
// now; a changed definition is checked afresh.
func (s *Service) hostMountIssue(server *servers.Context) *Issue {
	fingerprint, found := s.mountedData.Load(server.Logical.ID)
	if !found || fingerprint != server.Logical.BindingFingerprint {
		return nil
	}
	return &Issue{Code: ErrHostMount.Code, Message: ErrHostMount.Message + " Choose Check again once it is fixed."}
}

// ForgetHostMount drops the remembered finding, as when someone asks for the
// readiness to be checked again after fixing it.
func (s *Service) ForgetHostMount(serverID string) { s.mountedData.Delete(serverID) }
