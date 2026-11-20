package web

import (
	"net/http"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/history"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/web/views"
)

// cancelOperation calls off an operation that is still waiting. Of a cancel
// and the operation's own next step, exactly one wins; the response says
// which.
func (s *Server) cancelOperation(w http.ResponseWriter, r *http.Request, request *Request) {
	current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
	if err == nil {
		err = s.App.Jobs.Cancel(r.Context(), &current.User, r.PathValue("id"), r.PathValue("operation"))
	}
	if err != nil {
		w.Header().Set("HX-Trigger", "servers-changed")
		s.fail(w, r, request, err)
		return
	}
	cancelled(w, r, "Cancelled. Nothing was changed.")
}

// cancelWait ends a scheduled run's wait for players to leave.
func (s *Server) cancelWait(w http.ResponseWriter, r *http.Request, request *Request) {
	current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
	if err == nil {
		err = s.App.Schedules.CancelWait(r.Context(), &current.User, r.PathValue("id"), r.PathValue("schedule"))
	}
	if err != nil {
		w.Header().Set("HX-Trigger", "servers-changed")
		s.fail(w, r, request, err)
		return
	}
	cancelled(w, r, "Cancelled. The schedule’s next run is unchanged.")
}

func cancelled(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("HX-Trigger", "servers-changed")
	render(w, r, http.StatusOK, views.Alert("success", message))
}

// cancellable reads which of operations the viewer may cancel now.
func (s *Server) cancellable(r *http.Request, user *db.SessionUser, operations []history.Operation) map[string]bool {
	result := map[string]bool{}
	for _, operation := range operations {
		if !views.ActiveStatus(operation.Status) {
			continue
		}
		if job, err := s.App.Runner.Get(r.Context(), operation.ID); err == nil && job != nil && s.App.Jobs.CanCancel(r.Context(), user, job) {
			result[operation.ID] = true
		}
	}
	return result
}

// canceller is who cancelled job, if anyone did.
func (s *Server) canceller(r *http.Request, job *ops.Job) *history.Actor {
	id, _ := job.Result["cancelledBy"].(string)
	if job.Status != ops.Cancelled || id == "" {
		return nil
	}
	name := ""
	if user, err := s.App.DB.FindUserByID(r.Context(), id); err == nil && user != nil {
		name = user.Username
	}
	return history.PublicActor(id, name)
}
