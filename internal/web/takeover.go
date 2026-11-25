package web

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/attention"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/web/views"
)

// takeOverOffer is what a missing server's page and Needs attention offer:
// the active server Ludock found after it with the same game and data
// folders, which is very likely it under a new name, or else the active
// servers of its game. It is nil when nothing is plausible.
func (s *Server) takeOverOffer(ctx context.Context, from *identity.Server, all []identity.Server) *views.TakeOver {
	if from == nil || from.Status != identity.StatusMissing {
		return nil
	}
	offer := &views.TakeOver{From: from.ID, FromName: from.DisplayName}
	var likely []views.TakeOverTarget
	for _, server := range all {
		if server.ID == from.ID || server.HostID != from.HostID || server.Status != identity.StatusActive || server.ContainerID == "" {
			continue
		}
		target := views.TakeOverTarget{ID: server.ID, Name: server.DisplayName, Revision: server.BindingRevision}
		if s.App.Jobs.SameData(from, &server) && server.FirstSeenAt > from.FirstSeenAt {
			likely = append(likely, target)
		} else if jobs.SameGame(from.GameType, server.GameType) {
			offer.Others = append(offer.Others, target)
		}
	}
	if len(likely) == 1 {
		offer.Likely = &likely[0]
	} else {
		offer.Others = append(likely, offer.Others...)
	}
	if offer.Likely == nil && len(offer.Others) == 0 {
		return nil
	}
	err := s.App.DB.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM backups WHERE server_id=?1 AND state='complete'),
    (SELECT COUNT(*) FROM schedules WHERE server_id=?1), (SELECT COUNT(*) FROM server_grants WHERE server_id=?1)`, from.ID).
		Scan(&offer.Backups, &offer.Schedules, &offer.People)
	if err != nil {
		return nil
	}
	return offer
}

// takeOvers are the offers for Needs attention's missing servers.
func (s *Server) takeOvers(ctx context.Context, items []attention.Item) map[string]*views.TakeOver {
	all, err := s.App.Identity.List(ctx, "")
	if err != nil {
		return nil
	}
	offers := map[string]*views.TakeOver{}
	for _, item := range items {
		if index := slices.IndexFunc(all, func(server identity.Server) bool { return server.ID == item.ServerID }); index >= 0 && item.Kind == "binding" {
			if offer := s.takeOverOffer(ctx, &all[index], all); offer != nil && offer.Likely != nil {
				offers[item.ServerID] = offer
			}
		}
	}
	return offers
}

// takeOver moves a missing server's backups, schedules, sharing, monitoring
// choices, and history to the server an administrator says it now is, and
// then removes the missing one.
func (s *Server) takeOver(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	from, to := r.PathValue("id"), r.PostFormValue("to")
	// A suggestion is for the target as the page showed it; a pick has none.
	revision, err := strconv.ParseInt(cmp.Or(r.PostFormValue("bindingRevision"), "-1"), 10, 64)
	if err != nil || !identity.ValidID(from) || !identity.ValidID(to) {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Choose the server it is now."))
		return
	}
	done, err := s.App.Jobs.TakeOver(ctx, request.User(), request.Principal.TokenHash, from, to, revision, request.Client.IP)
	if err != nil {
		s.fail(w, r, request, authz.AdminMutationError(err))
		return
	}
	w.Header().Set("HX-Trigger", "servers-changed")
	if fromDetail(r) {
		render(w, r, http.StatusOK, views.TakeOverOnPage(*done))
		return
	}
	render(w, r, http.StatusOK, views.TakeOverResult(*done))
}
