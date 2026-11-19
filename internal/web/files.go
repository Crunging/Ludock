package web

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/textutil"
	"github.com/Crunging/Ludock/internal/web/views"
)

func fileTarget(action *serverAction) files.Target {
	return files.Target{ContainerID: action.Container.ID, Name: action.Container.Name, GameType: action.Container.GameType,
		Roots: action.Container.FileRoots}
}

// sortEntries keeps folders first; a value shown as a dash stays last in
// either direction. Names are collated once each.
func sortEntries(entries []helper.FileEntry, order string) {
	descending := strings.HasSuffix(order, "-desc")
	type keyed struct {
		entry helper.FileEntry
		key   []byte
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name
	}
	keys := textutil.NaturalKeys(names)
	sorted := make([]keyed, len(entries))
	for index, entry := range entries {
		sorted[index] = keyed{entry, keys[index]}
	}
	slices.SortStableFunc(sorted, func(leftKeyed, rightKeyed keyed) int {
		left, right := leftKeyed.entry, rightKeyed.entry
		if folders := cmp.Compare(boolInt(right.Type == "directory"), boolInt(left.Type == "directory")); folders != 0 {
			return folders
		}
		names := cmp.Or(bytes.Compare(leftKeyed.key, rightKeyed.key), strings.Compare(left.Name, right.Name))
		direction := 1
		if descending {
			direction = -1
		}
		if strings.HasPrefix(order, "name-") {
			return names * direction
		}
		value := func(entry helper.FileEntry) (float64, bool) {
			if strings.HasPrefix(order, "size-") {
				return float64(entry.Size), entry.Type == "file"
			}
			return entry.ModifiedAt, entry.ModifiedAt > 0
		}
		leftValue, leftOK := value(left)
		rightValue, rightOK := value(right)
		switch {
		case !leftOK && !rightOK:
			return names
		case !leftOK:
			return 1
		case !rightOK:
			return -1
		}
		return cmp.Or(cmp.Compare(leftValue, rightValue)*direction, names)
	})
	for index := range sorted {
		entries[index] = sorted[index].entry
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// filesView lists one folder. Discovery outages and missing access replace
// the browser with an error.
func (s *Server) filesView(r *http.Request, request *Request) views.FilesView {
	query := r.URL.Query()
	f := views.FilesView{ServerID: r.PathValue("id"), Filter: query.Get("q"), Sort: query.Get("sort"), Path: query.Get("path"),
		MaxUploadRaw: s.App.Config.MaxUploadBytes, MaxUpload: datasize.Format(s.App.Config.MaxUploadBytes)}
	if !slices.Contains([]string{"name-asc", "name-desc", "size-desc", "size-asc", "modified-desc", "modified-asc"}, f.Sort) {
		f.Sort = "name-asc"
	}
	f.Limit = views.FilePage
	if limit, err := strconv.Atoi(query.Get("limit")); err == nil && limit > views.FilePage {
		f.Limit = min(limit, 1<<20)
	}
	snapshot, err := s.App.Servers.Summary(r.Context(), request.User(), f.ServerID)
	if err != nil {
		f.Error = publicMessage(err)
		return f
	}
	view := snapshot.Server
	f.ServerName, f.State, f.Health, f.Exit = view.DisplayName, view.State, view.Health, view.Exit
	if snapshot.DiscoveryUnavailable {
		f.Error = "Unable to reach Docker to load server files. Try again when the connection is restored."
		return f
	}
	if !can(view, authz.FilesRead) {
		f.Error = "You do not have file access to this server."
		return f
	}
	f.CanManage, f.Blocked = can(view, authz.FilesWrite), view.BindingStatus != "active"
	f.Roots = view.FileRoots
	if len(f.Roots) == 0 {
		return f
	}
	f.Root = f.Roots[0]
	if index := slices.IndexFunc(f.Roots, func(root files.Root) bool { return root.ID == query.Get("root") }); index >= 0 {
		f.Root = f.Roots[index]
	}
	action, err := s.startAction(r, request, f.ServerID, authz.FilesRead)
	if err != nil {
		f.ListError = publicMessage(err)
		return f
	}
	defer action.Done()
	listing, err := s.App.Files.List(action.Ctx, fileTarget(action), f.Root.ID, f.Path, action.Assert)
	if err != nil {
		f.ListError = publicMessage(err)
		return f
	}
	f.Path = listing.Path
	f.Total = len(listing.Entries)
	search := strings.ToLower(strings.TrimSpace(f.Filter))
	for _, entry := range listing.Entries {
		if strings.Contains(strings.ToLower(entry.Name), search) {
			f.Entries = append(f.Entries, entry)
		}
	}
	sortEntries(f.Entries, f.Sort)
	// Large folders render a page at a time.
	f.Matched = len(f.Entries)
	f.Entries = f.Entries[:min(len(f.Entries), f.Limit)]
	return f
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request, request *Request) {
	f := s.filesView(r, request)
	title := f.ServerName
	if title == "" {
		title = "Server files"
	}
	page := s.page(r, request, title+" files")
	page.Path = "/"
	page.Scripts = []string{"files.js"}
	render(w, r, http.StatusOK, views.FilesPage(page, f))
}

func (s *Server) filesList(w http.ResponseWriter, r *http.Request, request *Request) {
	f := s.filesView(r, request)
	if f.Error != "" {
		f.ListError = f.Error
	}
	// Keep the address bar on the page, so reloads keep the filter.
	if htmxSource(r) != "file-list" && f.Root.ID != "" {
		values := url.Values{"root": {f.Root.ID}}
		if f.Path != "" {
			values.Set("path", f.Path)
		}
		if f.Filter != "" {
			values.Set("q", f.Filter)
		}
		if f.Sort != "name-asc" {
			values.Set("sort", f.Sort)
		}
		w.Header().Set("HX-Replace-Url", "/files/"+f.ServerID+"?"+values.Encode())
	}
	render(w, r, http.StatusOK, views.FileList(f))
}

func (s *Server) fileDialog(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	snapshot, err := s.App.Servers.Summary(r.Context(), request.User(), r.PathValue("id"))
	if err == nil && !can(snapshot.Server, authz.FilesWrite) {
		err = apperr.New("FORBIDDEN", 403, "Insufficient permissions")
	}
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	view := snapshot.Server
	d := views.FileDialog{ServerID: view.ID, ServerName: view.DisplayName, Action: query.Get("action"), Path: query.Get("path"),
		Type: query.Get("type")}
	index := slices.IndexFunc(view.FileRoots, func(root files.Root) bool { return root.ID == query.Get("root") })
	if index < 0 || !slices.Contains([]string{"create", "rename", "delete"}, d.Action) {
		s.fail(w, r, request, files.Error("ROOT_NOT_FOUND", 404))
		return
	}
	d.Root = view.FileRoots[index]
	if d.Action != "create" {
		d.Name = d.Path[strings.LastIndex(d.Path, "/")+1:]
	}
	if d.Action == "rename" {
		d.Value = d.Name
	}
	render(w, r, http.StatusOK, views.FileActionDialog(d))
}

var fileAudit = map[string]string{"create": "server.file.directory.created", "rename": "server.file.renamed", "delete": "server.file.deleted"}

// unconfirmed reports whether a failed file write may still have completed:
// the failure came after the helper could have committed it.
func unconfirmed(err error) bool {
	public, ok := apperr.As(err)
	return !ok || public.Status >= 500
}

// fileChange runs a folder creation, rename, or deletion and reports the
// result over the dialog: client errors keep it open with the draft.
func (s *Server) fileChange(kind string) func(http.ResponseWriter, *http.Request, *Request) {
	return func(w http.ResponseWriter, r *http.Request, request *Request) {
		root, path, name := r.PostFormValue("root"), r.PostFormValue("path"), r.PostFormValue("name")
		d := views.FileDialog{ServerID: r.PathValue("id"), Action: kind, Path: path, Type: r.PostFormValue("type"), Value: name}
		if kind != "create" {
			d.Name = path[strings.LastIndex(path, "/")+1:]
		}
		action, err := s.startAction(r, request, d.ServerID, authz.FilesWrite)
		if err != nil {
			s.fail(w, r, request, err)
			return
		}
		defer action.Done()
		d.ServerName = action.Container.DisplayName
		if index := slices.IndexFunc(action.Container.FileRoots, func(item files.Root) bool { return item.ID == root }); index >= 0 {
			d.Root = action.Container.FileRoots[index]
		}
		target := fileTarget(action)
		var message string
		details := map[string]any{"root": root, "path": path}
		switch kind {
		case "create":
			err = s.App.Files.Mkdir(action.Ctx, target, root, path, name, action.Assert)
			message, details["name"] = "Created “"+name+"”.", name
		case "rename":
			err = s.App.Files.Rename(action.Ctx, target, root, path, name, action.Assert)
			message, details["newName"] = "Renamed “"+d.Name+"” to “"+name+"”.", name
		case "delete":
			err = s.App.Files.Delete(action.Ctx, target, root, path, action.Assert)
			message = "Deleted “" + d.Name + "”."
		}
		if err != nil {
			public, ok := apperr.As(err)
			if ok && public.Status == http.StatusUnauthorized {
				s.fail(w, r, request, err)
				return
			}
			if !unconfirmed(err) {
				d.Error = public.Message
				render(w, r, public.Status, views.FileActionDialog(d))
				return
			}
			target := d.Name
			if kind == "create" {
				target = name
			}
			details["confirmed"] = false
			s.audit(r, request, fileAudit[kind], action.Logical.ID, details)
			message = "The result for “" + target + "” could not be confirmed. The change may have completed. Review the folder before starting another action."
			w.Header().Set("HX-Retarget", "#file-feedback")
			w.Header().Set("HX-Reswap", "innerHTML")
			w.Header().Set("HX-Trigger", "files-changed")
			render(w, r, http.StatusOK, views.Alert("error", message))
			return
		}
		s.audit(r, request, fileAudit[kind], action.Logical.ID, details)
		w.Header().Set("HX-Retarget", "#file-feedback")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("HX-Trigger", "files-changed")
		render(w, r, http.StatusOK, views.Alert("success", message))
	}
}

// filePreview shows a file inline, reading at most files.PreviewLimit, and
// with edit=1 opens it in the editor. A request from inside the dialog gets
// only the dialog's content.
func (s *Server) filePreview(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	id, root, path := r.PathValue("id"), query.Get("root"), query.Get("path")
	action, err := s.startAction(r, request, id, authz.FilesRead)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	p := s.previewView(action, id, root, path)
	if p.CanEdit, err = s.App.Authz.Has(action.Ctx, request.User(), id, authz.FilesWrite); err != nil {
		s.fail(w, r, request, err)
		return
	}
	preview, err := s.App.Files.Preview(action.Ctx, fileTarget(action), root, path, action.Assert)
	if err != nil {
		public, ok := apperr.As(err)
		if ok && public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		p.Error = publicMessage(err)
	} else {
		setPreview(&p, preview)
		p.Editing = query.Get("edit") == "1" && p.CanEdit && p.Kind == files.PreviewText && p.Editable
	}
	if htmxTarget(r) == "file-action" {
		render(w, r, http.StatusOK, views.FilePreviewContent(p))
		return
	}
	render(w, r, http.StatusOK, views.FilePreviewDialog(p))
}

func (s *Server) previewView(action *serverAction, id, root, path string) views.FilePreview {
	p := views.FilePreview{ServerID: id, Root: root, File: path, Name: path[strings.LastIndex(path, "/")+1:],
		Limit: datasize.Format(files.PreviewLimit), Download: "/files/" + id + "/download?" + url.Values{"root": {root}, "path": {path}}.Encode()}
	if index := slices.IndexFunc(action.Container.FileRoots, func(item files.Root) bool { return item.ID == root }); index >= 0 {
		p.Path = strings.TrimSuffix(action.Container.FileRoots[index].Path, "/") + "/" + path
	}
	return p
}

func setPreview(p *views.FilePreview, preview *files.Preview) {
	p.Name, p.Size, p.Kind, p.Text = preview.Name, preview.Size, preview.Kind, preview.Text
	p.Editable, p.Version, p.CRLF, p.BOM = preview.Editable, preview.Version, preview.CRLF, preview.BOM
}

// fileSave replaces a previewed text file with the editor's text, sent raw
// (application/octet-stream) with line breaks as LF, like an upload. It
// answers with the dialog's content: the new preview, or the editor with
// the submitted text and the reason it was not saved.
func (s *Server) fileSave(w http.ResponseWriter, r *http.Request, request *Request) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/octet-stream" {
		http.Error(w, "Saves must use application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	query := r.URL.Query()
	id, root, path := r.PathValue("id"), query.Get("root"), query.Get("path")
	crlf, bom := query.Get("crlf") == "1", query.Get("bom") == "1"
	// The body is read before the action rechecks access and takes the locks.
	controller := http.NewResponseController(w)
	controller.SetReadDeadline(time.Now().Add(time.Minute))
	body, err := io.ReadAll(io.LimitReader(r.Body, files.PreviewLimit+1))
	controller.SetReadDeadline(time.Time{})
	if err != nil {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Invalid save request"))
		return
	}
	// These refusals answer with an alert only, so the editor keeps the draft.
	// A truncated body is refused before normalizing could shrink it.
	tooLarge := apperr.New("TEXT_TOO_LARGE", 413, "The file would be over "+datasize.Format(files.PreviewLimit)+", so it can’t be saved here.")
	if len(body) > files.PreviewLimit {
		s.fail(w, r, request, tooLarge)
		return
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		s.fail(w, r, request, apperr.New("INVALID_TEXT", 400, "The text can’t be saved as a text file."))
		return
	}
	content := files.EditedContent(string(body), crlf, bom)
	if len(content) > files.PreviewLimit {
		s.fail(w, r, request, tooLarge)
		return
	}
	action, err := s.startAction(r, request, id, authz.FilesWrite)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	p := s.previewView(action, id, root, path)
	p.CanEdit, p.Editable, p.Editing, p.Unsaved = true, true, true, true
	p.Kind, p.Text, p.Version, p.CRLF, p.BOM, p.Size = files.PreviewText, string(body), query.Get("version"), crlf, bom, int64(len(body))
	if err := s.App.Files.Save(action.Ctx, fileTarget(action), root, path, content, p.Version, action.Assert); err != nil {
		public, ok := apperr.As(err)
		if ok && public.Status == http.StatusUnauthorized {
			s.fail(w, r, request, err)
			return
		}
		details := map[string]any{"root": root, "path": path, "size": len(content)}
		if unconfirmed(err) {
			// The draft stays in the editor in case the write did not happen.
			details["confirmed"] = false
			s.audit(r, request, "server.file.edited", action.Logical.ID, details)
			p.Error = "The save of “" + p.Name + "” could not be confirmed. It may have completed. Copy your text, then reopen the file to check it before saving again."
			render(w, r, http.StatusOK, views.FilePreviewContent(p))
			return
		}
		p.Error = public.Message
		render(w, r, public.Status, views.FilePreviewContent(p))
		return
	}
	s.audit(r, request, "server.file.edited", action.Logical.ID, map[string]any{"root": root, "path": path, "size": len(content)})
	saved, _ := files.ReadPreview(bytes.NewReader(content))
	saved.Name, saved.Size = p.Name, int64(len(content))
	p = s.previewView(action, id, root, path)
	setPreview(&p, &saved)
	p.CanEdit, p.Notice = true, "Saved “"+p.Name+"”."
	render(w, r, http.StatusOK, views.FilePreviewContent(p))
}

func contentDisposition(name string) string {
	fallback := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if len(fallback) > 180 {
		fallback = fallback[:180]
	}
	if fallback == "" {
		fallback = "download"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` + url.PathEscape(name)
}

// fileDownload streams a file, or a folder as tar. Access is rechecked
// between chunks; the locks last until the helper is removed.
func (s *Server) fileDownload(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	root, path := query.Get("root"), query.Get("path")
	action, err := s.startAction(r, request, r.PathValue("id"), authz.FilesRead)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	download, err := s.App.Files.OpenDownload(action.Ctx, fileTarget(action), root, path, action.Assert)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer download.Close()
	w.Header().Set("Content-Disposition", contentDisposition(download.Name))
	if download.Type == "directory" {
		w.Header().Set("Content-Type", "application/x-tar")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		if download.Size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(download.Size, 10))
		}
	}
	s.audit(r, request, "server.file.downloaded", action.Logical.ID, map[string]any{"root": root, "path": path})
	controller := http.NewResponseController(w)
	buffer := make([]byte, 256*1024)
	checked := time.Now()
	for {
		controller.SetWriteDeadline(time.Now().Add(time.Minute))
		n, err := download.Read(buffer)
		// Check after the read, which can block and still return buffered
		// bytes once revocation closes the stream, so no chunk is sent more
		// than a second after access was last confirmed.
		if n > 0 && (action.Ctx.Err() != nil || time.Since(checked) >= time.Second) {
			if action.Assert() != nil {
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
			// The body has started; only an aborted connection can tell the
			// browser that the download is incomplete.
			panic(http.ErrAbortHandler)
		}
	}
}

// fileUpload stores one uploaded file. The body is the raw file
// (application/octet-stream) with its exact Content-Length; the CSRF token
// comes in the X-CSRF-Token header.
func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request, request *Request) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/octet-stream" {
		http.Error(w, "Uploads must use application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	query := r.URL.Query()
	root, path, name := query.Get("root"), query.Get("path"), query.Get("name")
	size := r.ContentLength
	if size < 0 || root == "" || name == "" {
		s.fail(w, r, request, apperr.New("INVALID_REQUEST", 400, "Invalid upload request"))
		return
	}
	if limit := s.App.Config.MaxUploadBytes; size > limit {
		s.fail(w, r, request, apperr.New("UPLOAD_TOO_LARGE", 413, "File exceeds the upload size limit of "+datasize.Format(limit)))
		return
	}
	action, err := s.startAction(r, request, r.PathValue("id"), authz.FilesWrite)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	details := map[string]any{"root": root, "path": path, "name": name, "size": size}
	if err := s.App.Files.Upload(action.Ctx, fileTarget(action), root, path, name, size, r.Body, action.Assert); err != nil {
		if public, ok := apperr.As(err); (ok && public.Status == http.StatusUnauthorized) || !unconfirmed(err) {
			s.fail(w, r, request, err)
			return
		}
		// 202 tells the page the upload stopped without a known result.
		details["confirmed"] = false
		s.audit(r, request, "server.file.uploaded", action.Logical.ID, details)
		render(w, r, http.StatusAccepted, views.Alert("error", "The result for “"+name+"” could not be confirmed. The upload may have completed. Review the folder before uploading again."))
		return
	}
	s.audit(r, request, "server.file.uploaded", action.Logical.ID, details)
	w.WriteHeader(http.StatusCreated)
}
