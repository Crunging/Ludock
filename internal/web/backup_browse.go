package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/web/views"
)

// backupBrowser lists one folder of a backup for an administrator who can
// read the server's archives.
func (s *Server) backupBrowser(r *http.Request, serverID, backupID string) *views.BackupBrowser {
	query := r.URL.Query()
	browser := &views.BackupBrowser{ServerID: serverID, Path: query.Get("path"), Limit: views.FilePage}
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > views.FilePage {
		browser.Limit = min(limit, 1<<20)
	}
	backup, err := s.App.Backups.Get(r.Context(), serverID, backupID)
	if err != nil {
		browser.Error = publicMessage(err)
		return browser
	}
	browser.Backup, browser.Roots = *backup, backup.Roots
	if len(backup.Roots) == 0 {
		browser.Error = "This backup holds no data folders."
		return browser
	}
	browser.Root = backup.Roots[0]
	if index := slices.IndexFunc(backup.Roots, func(root backups.Root) bool { return root.ID == query.Get("root") }); index >= 0 {
		browser.Root = backup.Roots[index]
	}
	entries, err := s.App.Backups.Storage.ListFolder(backup.Destination(), backup.ID, backup.Roots, browser.Root.ID, browser.Path)
	if err != nil {
		browser.Error = publicMessage(err)
		return browser
	}
	browser.Total = len(entries)
	browser.Entries = entries[:min(len(entries), browser.Limit)]
	if pick := query.Get("pick"); pick != "" {
		if index := slices.IndexFunc(entries, func(entry backups.ArchiveEntry) bool { return entry.Name == pick }); index >= 0 {
			browser.Pick = &entries[index]
		}
	}
	return browser
}

// backupFolder is a folder of a backup on its own, for showing more of it.
func (s *Server) backupFolder(w http.ResponseWriter, r *http.Request, request *Request) {
	id := r.PathValue("id")
	if _, err := s.App.Authz.Assert(r.Context(), request.User(), id, authz.BackupsRead); err != nil {
		s.fail(w, r, request, err)
		return
	}
	browser := s.backupBrowser(r, id, r.PathValue("backup"))
	render(w, r, http.StatusOK, views.BackupFolder(*browser))
}

// downloadBackupFile streams one file out of a backup, rechecking access as
// a whole-archive download does.
func (s *Server) downloadBackupFile(w http.ResponseWriter, r *http.Request, request *Request) {
	ctx := r.Context()
	id, backupID := r.PathValue("id"), r.PathValue("backup")
	query := r.URL.Query()
	root, item := query.Get("root"), query.Get("path")
	check := s.archiveAccess(ctx, request, id)
	if _, err := s.App.Authz.Assert(ctx, request.User(), id, authz.BackupsRead); err != nil {
		s.fail(w, r, request, err)
		return
	}
	backup, err := s.App.Backups.Get(ctx, id, backupID)
	if err == nil {
		err = check()
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	started := false
	err = s.App.Backups.Storage.ReadFile(backup.Destination(), backup.ID, backup.Roots, root, item, func(file backups.ArchiveItem, body io.Reader) error {
		started = true
		w.Header().Set("Content-Disposition", contentDisposition(path.Base(item)))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
		s.audit(r, request, "backup.file_downloaded", id, map[string]any{"backupId": backupID, "root": root, "path": item})
		copyChecked(w, body, check)
		return nil
	})
	if err != nil {
		if started {
			panic(http.ErrAbortHandler)
		}
		s.fail(w, r, request, err)
	}
}

// archiveAccess confirms the session still belongs to an administrator who
// can read the server's backups.
func (s *Server) archiveAccess(ctx context.Context, request *Request, id string) func() error {
	return func() error {
		current, err := s.App.Auth.Recheck(context.WithoutCancel(ctx), request.Principal)
		if err == nil {
			err = auth.RequireAdmin(current)
		}
		if err == nil {
			_, err = s.App.Authz.Assert(context.WithoutCancel(ctx), &current.User, id, authz.BackupsRead)
		}
		return err
	}
}

// copyChecked streams a download, checking access again every second; a
// revoked session or grant cuts it off.
func copyChecked(w http.ResponseWriter, source io.Reader, check func() error) {
	controller := http.NewResponseController(w)
	buffer := make([]byte, 256*1024)
	checked := time.Now()
	for {
		controller.SetWriteDeadline(time.Now().Add(time.Minute))
		n, err := source.Read(buffer)
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

// restoreItemInput checks a selective restore's root and item against the
// backup; empty values mean a whole restore.
func restoreItemInput(backup *backups.Backup, root, item string) (map[string]any, error) {
	if root == "" && item == "" {
		return nil, nil
	}
	if clean, err := backups.CleanPath(item); err != nil || clean == "" ||
		!slices.ContainsFunc(backup.Roots, func(candidate backups.Root) bool { return candidate.ID == root }) {
		return nil, apperr.New("UNSAFE_PATH", http.StatusBadRequest, "That path isn’t allowed.")
	}
	return map[string]any{"root": root, "item": item}, nil
}
