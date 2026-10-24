package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/web/views"
)

// backupsView reads the backups tab. Archive history is local metadata and
// stays available during a Docker outage.
func (s *Server) backupsView(r *http.Request, request *Request, d *views.ServerDetail) views.BackupsView {
	view := &d.Row.Server
	b := views.BackupsView{ServerID: view.ID, ServerName: view.DisplayName, Admin: d.Admin,
		CanRead: can(view, authz.BackupsRead), CanCreate: can(view, authz.BackupsCreate),
		CanRestore: can(view, authz.BackupsRestore), CanDelete: can(view, authz.BackupsDelete),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable, Active: view.ActiveOperation != nil,
		LatestBackup: view.LatestBackup}
	// A backup or restore started here shows its progress here.
	b.Operation = views.ActivityView{ServerID: view.ID, Admin: d.Admin, Tab: "backups", SelectedID: r.URL.Query().Get("operation"),
		Archives: d.Admin && can(view, authz.BackupsRead)}
	if b.Operation.SelectedID != "" {
		s.selectOperation(r, &b.Operation)
	}
	if b.Admin && b.CanRead {
		s.readBackups(r, &b)
		if id := r.URL.Query().Get("restore"); id != "" {
			for index := range b.Backups {
				if b.Backups[index].ID == id && b.Backups[index].State == "complete" {
					b.Restore = &b.Backups[index]
				}
			}
		}
	}
	return b
}

func (s *Server) readBackups(r *http.Request, b *views.BackupsView) {
	list, err := s.App.Backups.List(r.Context(), b.ServerID)
	if err != nil {
		b.HistoryError = publicMessage(err)
	}
	b.Backups = list
}

func (s *Server) backupList(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	// A successful refresh still applies binding changes before access.
	s.App.Servers.Refresh(r.Context())
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.BackupsRead); err != nil {
		s.fail(w, r, request, err)
		return
	}
	d, err := s.detail(r, request, id, "backups", false)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	b := s.backupsView(r, request, d)
	render(w, r, http.StatusOK, views.BackupList(b))
}

// resolveFresh confirms the binding and then the session's current
// capability, like every action that queues work.
func (s *Server) resolveFresh(r *http.Request, request *Request, id string, capability authz.Capability) (*servers.Context, *db.SessionUser, error) {
	resolved, err := s.App.Servers.ResolveAuthorized(r.Context(), request.User(), id, capability, -1)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.App.Authz.Assert(r.Context(), &current.User, id, capability); err != nil {
		return nil, nil, err
	}
	return resolved, &current.User, nil
}

func (s *Server) backupPreflight(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	resolved, user, err := s.resolveFresh(r, request, id, authz.BackupsCreate)
	view := views.ReadinessView{ServerID: id, Admin: request.User().Role == db.RoleAdmin}
	if err == nil {
		view.ServerName = resolved.Container.DisplayName
		preflight := s.App.Backups.Preflight(r.Context(), resolved)
		if _, err = s.App.Authz.Assert(r.Context(), user, id, authz.BackupsCreate); err == nil {
			view.Preflight = &preflight
		}
	}
	if err != nil {
		if public, ok := apperr.As(err); ok && (public.Status == http.StatusUnauthorized || public.Status == http.StatusNotFound) {
			s.fail(w, r, request, err)
			return
		}
		view.Error = publicMessage(err)
	}
	render(w, r, http.StatusOK, views.BackupReadiness(view))
}

// enqueue queues work for a confirmed server and follows it in the
// server's activity. Administrator-only work is queued only while the
// requesting session is still an administrator's: the runner later checks
// the account, not the session.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, request *Request, resolved *servers.Context, user *db.SessionUser,
	kind string, input map[string]any) {
	options := ops.EnqueueOptions{ServerID: resolved.Logical.ID, ActorID: user.ID, Kind: kind,
		BindingRevision: resolved.Logical.BindingRevision, Input: input, IdempotencyKey: requestKey(r)}
	var job *ops.Job
	var err error
	if kind == "restore" || kind == "update" {
		err = s.App.DB.AdminMutation(r.Context(), user.ID, request.Principal.TokenHash, db.Now(), func(tx *sql.Tx) error {
			job, err = s.App.Runner.Enqueue(r.Context(), tx, options)
			return err
		})
		if err == nil {
			s.App.DB.PruneAuditIfNeeded(r.Context())
			s.App.Runner.Wake()
		}
		err = authz.AdminMutationError(err)
	} else {
		job, err = s.App.Runner.Enqueue(r.Context(), nil, options)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	followOperation(w, resolved.Logical.ID, job)
	changed(w)
}

// followOperation shows queued work on the server's page, swapping the page
// body in place like its tabs do.
func followOperation(w http.ResponseWriter, serverID string, job *ops.Job) {
	var location strings.Builder
	encoder := json.NewEncoder(&location)
	encoder.SetEscapeHTML(false)
	encoder.Encode(map[string]string{"path": views.OperationPath(serverID, job.Kind, job.ID),
		"target": "#detail-body", "select": "#detail-body", "swap": "outerHTML"})
	w.Header().Set("HX-Location", strings.TrimSpace(location.String()))
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	resolved, user, err := s.resolveFresh(r, request, r.PathValue("id"), authz.BackupsCreate)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.enqueue(w, r, request, resolved, user, "backup", nil)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	resolved, user, err := s.resolveFresh(r, request, id, authz.BackupsRestore)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	confirmation, backupID := r.PostFormValue("confirmation"), r.PostFormValue("backupId")
	if confirmation != resolved.Container.DisplayName {
		s.fail(w, r, request, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm the restore"))
		return
	}
	if _, err := s.App.Backups.Get(r.Context(), id, backupID); err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.enqueue(w, r, request, resolved, user, "restore", map[string]any{"backupId": backupID, "confirmation": confirmation})
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.BackupsDelete); err != nil {
		s.fail(w, r, request, err)
		return
	}
	err := s.App.Backups.Delete(r.Context(), id, backupID, func() error {
		current, err := s.App.Auth.Recheck(context.WithoutCancel(r.Context()), request.Principal)
		if err == nil {
			_, err = s.App.Authz.Assert(context.WithoutCancel(r.Context()), &current.User, id, authz.BackupsDelete)
		}
		return err
	})
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "backup.deleted", id, map[string]any{"backupId": backupID})
	w.Header().Set("HX-Trigger", "backups-changed, servers-changed")
	render(w, r, http.StatusOK, views.Alert("success", "Backup deleted."))
}

// downloadBackup streams an archive, checking access again every second;
// a revoked session or grant cuts the download off.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	check := func() error {
		current, err := s.App.Auth.Recheck(context.WithoutCancel(ctx), request.Principal)
		if err == nil {
			err = auth.RequireAdmin(current)
		}
		if err == nil {
			_, err = s.App.Authz.Assert(context.WithoutCancel(ctx), &current.User, id, authz.BackupsRead)
		}
		return err
	}
	if _, err := s.App.Authz.Assert(ctx, request.User(), id, authz.BackupsRead); err != nil {
		s.fail(w, r, request, err)
		return
	}
	archive, err := s.App.Backups.Open(ctx, id, backupID)
	if err == nil {
		if err = check(); err != nil {
			archive.Close()
		}
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer archive.Close()
	s.audit(r, request, "backup.downloaded", id, map[string]any{"backupId": backupID})
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="ludock-`+backupID+`.tar"`)
	controller := http.NewResponseController(w)
	buffer := make([]byte, 256*1024)
	checked := time.Now()
	for {
		controller.SetWriteDeadline(time.Now().Add(time.Minute))
		n, err := archive.Read(buffer)
		// Check after the read, which can block on slow storage, so no chunk
		// is sent more than a second after access was last confirmed.
		if n > 0 && time.Since(checked) >= time.Second {
			if check() != nil {
				panic(http.ErrAbortHandler)
			}
			checked = time.Now()
		}
		if n > 0 {
			if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
				return
			}
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func (s *Server) updatePanel(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	admin := request.User().Role == db.RoleAdmin
	capability := authz.ServerRestart
	if admin {
		capability = authz.ServerUpdate
	}
	d, err := s.detail(r, request, id, "update", false)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	view := &d.Row.Server
	u := views.UpdateView{ServerID: id, ServerName: view.DisplayName, Admin: admin, CanBackup: can(view, authz.BackupsCreate),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable, Active: view.ActiveOperation != nil,
		Recreate: r.URL.Query().Get("recreate") == "1"}
	resolved, _, err := s.resolveFresh(r, request, id, capability)
	if err == nil {
		result := s.App.Updates.Capability(r.Context(), resolved, admin, func() bool {
			if s.App.Locks.ServerBusy(id) {
				return true
			}
			snapshot, err := s.App.Servers.Summary(r.Context(), request.User(), id)
			return err != nil || snapshot.Server.ActiveOperation != nil
		})
		if err = s.recheckCapability(r, request, id, capability, admin); err == nil {
			u.Capability = &result
		}
	}
	if err != nil {
		if public, ok := apperr.As(err); ok && (public.Status == http.StatusUnauthorized || public.Status == http.StatusNotFound) {
			s.fail(w, r, request, err)
			return
		}
	}
	render(w, r, http.StatusOK, views.UpdatePanel(u))
}

func (s *Server) recheckCapability(r *http.Request, request *Request, id string, capability authz.Capability, admin bool) error {
	current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
	if err != nil {
		return err
	}
	if admin {
		if err := s.recheckAdmin(r.Context(), request); err != nil {
			return err
		}
	}
	_, err = s.App.Authz.Assert(r.Context(), &current.User, id, capability)
	return err
}

// queueUpdate pulls a server's image and recreates its container
// (administrators only).
func (s *Server) queueUpdate(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	createBackup := r.PostFormValue("createBackup") == "true"
	forceRecreate := r.PostFormValue("forceRecreate") == "true"
	skip := r.PostFormValue("skipBackupConfirmation")
	capability := authz.ServerUpdate
	if forceRecreate {
		capability = authz.ServerRecreate
	}
	if r.PostFormValue("confirmed") != "true" {
		s.fail(w, r, request, apperr.New("CONFIRMATION_REQUIRED", 400, "Confirm that connected players will be disconnected"))
		return
	}
	resolved, _, err := s.resolveFresh(r, request, id, capability)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	if !createBackup && skip != resolved.Container.DisplayName {
		s.fail(w, r, request, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm skipping backup"))
		return
	}
	if _, err := s.App.Updates.Check(r.Context(), resolved); err != nil {
		s.fail(w, r, request, err)
		return
	}
	if err := s.recheckCapability(r, request, id, capability, true); err != nil {
		s.fail(w, r, request, err)
		return
	}
	current, err := s.App.Auth.Recheck(r.Context(), request.Principal)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "server.update.confirmed", id, map[string]any{"createBackup": createBackup, "forceRecreate": forceRecreate})
	updateRequest := map[string]any{"createBackup": createBackup, "forceRecreate": forceRecreate}
	if !createBackup {
		updateRequest["skipBackupConfirmation"] = skip
	}
	s.enqueue(w, r, request, resolved, &current.User, "update", map[string]any{"request": updateRequest})
}
