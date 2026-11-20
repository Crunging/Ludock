package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/games"
	"github.com/Crunging/Ludock/internal/jobs"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
	"github.com/Crunging/Ludock/internal/update"
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
		LatestBackup: view.LatestBackup, NoData: view.NoDataFolders, BindingRevision: view.BindingRevision}
	// The tab follows any running work, so readiness and paused controls come
	// back when it ends.
	b.Operation = views.ActivityView{ServerID: view.ID, Admin: d.Admin, Tab: "backups", SelectedID: r.URL.Query().Get("operation"),
		Archives: d.Admin && can(view, authz.BackupsRead)}
	if b.Operation.SelectedID == "" && view.ActiveOperation != nil {
		b.Operation.SelectedID = view.ActiveOperation.ID
	}
	if b.Operation.SelectedID != "" {
		s.selectOperation(r, &b.Operation)
	}
	if b.Admin && b.CanRead {
		if id := r.URL.Query().Get("browse"); id != "" {
			b.Browse = s.backupBrowser(r, view.ID, id, s.archiveAccess(r.Context(), request, view.ID))
			b.Browse.ServerName, b.Browse.CanRestore = view.DisplayName, restoreOffered(view, d.DiscoveryUnavailable)
			s.fitBrowser(r.Context(), view, b.Browse)
			return b
		}
		s.readBackups(r, &b)
		if !b.Blocked {
			b.Fits = s.backupFits(r.Context(), view, b.Backups)
		}
		restore, keep := r.URL.Query().Get("restore"), r.URL.Query().Get("keep")
		for index := range b.Backups {
			backup := &b.Backups[index]
			if backup.State != "complete" {
				continue
			}
			if backup.ID == restore && b.Fits[backup.ID].Refusal == "" {
				b.Restore = backup
			}
			if backup.ID == keep && backup.KeptAt == 0 {
				b.Keep = backup
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
	// A removed backup that waits only for its second copy to go is gone.
	b.Backups = slices.DeleteFunc(list, func(backup backups.Backup) bool { return backup.CopyState == "removing" })
	if err := s.App.Backups.Locate(b.Backups); err != nil {
		b.FolderIssue = publicMessage(err)
	}
}

// backupFits compares each backup with the server as discovery last saw it,
// for the page only; a restore compares again with Docker before it runs.
func (s *Server) backupFits(ctx context.Context, view *servers.View, list []backups.Backup) map[string]backups.Fit {
	logical, err := s.App.Identity.Get(ctx, view.ID)
	if err != nil || logical == nil {
		return nil
	}
	roots := backups.CurrentRoots(view.FileRoots)
	fits := map[string]backups.Fit{}
	for index := range list {
		fits[list[index].ID] = backups.CompareFit(&list[index], logical.BindingFingerprint, view.GameType, roots)
	}
	return fits
}

// fitBrowser applies a backup's fit to its browser: a backup that can't be
// restored offers no restores, and one taken before the data moved says so.
func (s *Server) fitBrowser(ctx context.Context, view *servers.View, browser *views.BackupBrowser) {
	if !browser.CanRestore || browser.Backup.ID == "" {
		return
	}
	browser.Fit = s.backupFits(ctx, view, []backups.Backup{browser.Backup})[browser.Backup.ID]
	browser.BindingRevision = view.BindingRevision
	browser.CanRestore = browser.Fit.Refusal == ""
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

// followOperation shows queued work on the server's page.
func followOperation(w http.ResponseWriter, serverID string, job *ops.Job) {
	showInPage(w, views.OperationPath(serverID, job.Kind, job.ID))
}

// showInPage moves to another view of a server, swapping the page body in
// place like its tabs do.
func showInPage(w http.ResponseWriter, path string) {
	var location strings.Builder
	encoder := json.NewEncoder(&location)
	encoder.SetEscapeHTML(false)
	encoder.Encode(map[string]string{"path": path, "target": "#detail-body", "select": "#detail-body", "swap": "outerHTML"})
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
	backupID := r.PostFormValue("backupId")
	backup, err := s.App.Backups.Get(r.Context(), id, backupID)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	input, err := restoreItemInput(backup, r.PostFormValue("root"), r.PostFormValue("item"))
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	fit := backups.FitFor(backup, resolved)
	if fit.Refusal != "" {
		s.fail(w, r, request, apperr.New("BACKUP_FOLDERS_DIFFER", http.StatusConflict, fit.Refusal))
		return
	}
	// The confirmation holds only for the storage the page showed.
	if fit.Moved && (r.PostFormValue(jobs.MovedInput) != "true" ||
		r.PostFormValue("bindingRevision") != strconv.FormatInt(resolved.Logical.BindingRevision, 10)) {
		s.fail(w, r, request, apperr.New("CONFIRMATION_REQUIRED", http.StatusBadRequest,
			"This backup was taken when the server’s data lived somewhere else. Reload the page and confirm the restore again."))
		return
	}
	// A whole restore is confirmed by typing the server's name; one item, in
	// the confirmation dialog, which can ask for a backup first.
	if input == nil {
		confirmation := r.PostFormValue("confirmation")
		if confirmation != resolved.Container.DisplayName {
			s.fail(w, r, request, apperr.New("CONFIRMATION_REQUIRED", 400, "Type the server name to confirm the restore"))
			return
		}
		input = map[string]any{"confirmation": confirmation}
	} else if r.PostFormValue("createBackup") == "true" {
		input["createBackup"] = true
	}
	input["backupId"] = backupID
	if fit.Moved {
		input[jobs.MovedInput] = true
	}
	s.enqueue(w, r, request, resolved, user, "restore", input)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.BackupsDelete); err != nil {
		s.fail(w, r, request, err)
		return
	}
	// The page asks for this only after saying the archive may stay behind.
	forget := r.URL.Query().Get("folderAway") == "1"
	left, err := s.App.Backups.Delete(r.Context(), id, backupID, forget, func() error {
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
	details := map[string]any{"backupId": backupID}
	message := "Backup deleted."
	if left {
		details["archiveLeft"] = true
		message = "Backup removed from Ludock. Its archive may still be in the folder it was saved in."
	}
	s.audit(r, request, "backup.deleted", id, details)
	w.Header().Set("HX-Trigger", "backups-changed, servers-changed")
	render(w, r, http.StatusOK, views.Alert("success", message))
}

// keepBackup and releaseBackup let an administrator who can read a server's
// archives exempt one from retention, or end that.
func (s *Server) keepBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	err := s.assertArchives(r, request, id)
	if err == nil {
		err = s.App.Backups.Keep(r.Context(), id, backupID, r.PostFormValue("note"), request.User(), request.Principal.TokenHash)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "backup.kept", id, map[string]any{"backupId": backupID})
	showInPage(w, "/servers/"+id+"?tab=backups")
}

func (s *Server) releaseBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	err := s.assertArchives(r, request, id)
	if err == nil {
		err = s.App.Backups.Release(r.Context(), id, backupID, request.User(), request.Principal.TokenHash)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "backup.released", id, map[string]any{"backupId": backupID})
	w.Header().Set("HX-Trigger", "backups-changed")
	render(w, r, http.StatusOK, views.Alert("success", "Backup released. Retention can remove it like any other."))
}

func (s *Server) assertArchives(r *http.Request, request *Request, id string) error {
	return s.archiveAccess(r.Context(), request, id)()
}

// downloadBackup streams an archive, checking access again every second;
// a revoked session or grant cuts the download off.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	check := s.archiveAccess(ctx, request, id)
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
	copyChecked(w, archive, check)
}

// updatePanel shows what updating a server would do (administrators only).
func (s *Server) updatePanel(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	capability := authz.ServerUpdate
	d, err := s.detail(r, request, id, "update", false)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	view := &d.Row.Server
	u := views.UpdateView{ServerID: id, ServerName: view.DisplayName, CanBackup: can(view, authz.BackupsCreate),
		Blocked: view.BindingStatus != "active" || d.DiscoveryUnavailable, Active: view.ActiveOperation != nil,
		Recreate: r.URL.Query().Get("recreate") == "1", Warn: s.updateWarnOffer(view)}
	resolved, _, err := s.resolveFresh(r, request, id, capability)
	if err == nil {
		result := s.App.Updates.Capability(r.Context(), resolved, func() bool {
			if s.App.Locks.ServerBusy(id) {
				return true
			}
			snapshot, err := s.App.Servers.Summary(r.Context(), request.User(), id)
			return err != nil || snapshot.Server.ActiveOperation != nil
		})
		if err = s.recheckCapability(r, request, id, capability, true); err == nil {
			u.Capability = &result
			u.ChecksOff = !s.App.Checks.Enabled(r.Context())
			u.Check = s.App.Checks.Status(r.Context(), id, resolved.Container.ID)
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

// checkImage asks the registry about a server's image now and shows the
// result (administrators only).
func (s *Server) checkImage(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	resolved, _, err := s.resolveFresh(r, request, id, authz.ServerUpdate)
	if err == nil {
		err = s.recheckAdmin(r.Context(), request)
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	capability := &update.Capability{}
	if integration := games.ImageIntegration(resolved.Container.Image); integration != nil {
		capability.UpdatesOnStart = integration.UpdatesOnStart
	}
	u := views.UpdateView{ServerID: id, ChecksOff: !s.App.Checks.Enabled(r.Context()), Capability: capability}
	if !u.ChecksOff {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		u.Check = s.App.Checks.CheckNow(ctx, id, resolved.Container)
		cancel()
		// The check can wait behind another; the session may have lost the
		// administrator role meanwhile.
		if err := s.recheckAdmin(r.Context(), request); err != nil {
			s.fail(w, r, request, err)
			return
		}
		u.ChecksOff = !s.App.Checks.Enabled(r.Context())
	}
	render(w, r, http.StatusOK, views.ImageCheck(u))
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
	input := map[string]any{"request": updateRequest}
	// Unlike a restart, an update asked to warn keeps that through an empty
	// server: it stops later, after the download.
	if r.PostFormValue(jobs.WarnInput) == "true" && resolved.Container.State == "running" && jobs.CanWarn(resolved.Container) {
		input[jobs.WarnInput] = true
	}
	s.enqueue(w, r, request, resolved, &current.User, "update", input)
}
