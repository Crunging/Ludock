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

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/textutil"
	"github.com/Crunging/Ludock/internal/web/views"
)

// fileTarget is the confirmed container of a file action.
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
	if r.Header.Get("HX-Trigger") != "file-list" && f.Root.ID != "" {
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

// fileDialog opens a new-folder, rename, or delete dialog.
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
			if ok && public.Status < 500 {
				d.Error = public.Message
				render(w, r, public.Status, views.FileActionDialog(d))
				return
			}
			// A write may have completed before it failed to report.
			target := d.Name
			if kind == "create" {
				target = name
			}
			message = "The result for “" + target + "” could not be confirmed. The change may have completed. Review the folder before starting another action."
			w.Header().Set("HX-Retarget", "#file-feedback")
			w.Header().Set("HX-Reswap", "innerHTML")
			w.Header().Set("HX-Trigger", "files-changed")
			render(w, r, http.StatusOK, views.Alert("error", message))
			return
		}
		audit := map[string]string{"create": "server.file.directory.created", "rename": "server.file.renamed", "delete": "server.file.deleted"}[kind]
		s.audit(r, request, audit, action.Logical.ID, details)
		w.Header().Set("HX-Retarget", "#file-feedback")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("HX-Trigger", "files-changed")
		render(w, r, http.StatusOK, views.Alert("success", message))
	}
}

// filePreview shows a file inline, reading at most files.PreviewLimit.
func (s *Server) filePreview(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	id, root, path := r.PathValue("id"), query.Get("root"), query.Get("path")
	action, err := s.startAction(r, request, id, authz.FilesRead)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	p := views.FilePreview{ServerID: id, Name: path[strings.LastIndex(path, "/")+1:], Limit: datasize.Format(files.PreviewLimit),
		Download: "/files/" + id + "/download?" + url.Values{"root": {root}, "path": {path}}.Encode()}
	if index := slices.IndexFunc(action.Container.FileRoots, func(item files.Root) bool { return item.ID == root }); index >= 0 {
		p.Path = strings.TrimSuffix(action.Container.FileRoots[index].Path, "/") + "/" + path
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
		p.Name, p.Size, p.Kind, p.Text = preview.Name, preview.Size, preview.Kind, preview.Text
	}
	render(w, r, http.StatusOK, views.FilePreviewDialog(p))
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
	if err := s.App.Files.Upload(action.Ctx, fileTarget(action), root, path, name, size, r.Body, action.Assert); err != nil {
		s.fail(w, r, request, err)
		return
	}
	s.audit(r, request, "server.file.uploaded", action.Logical.ID, map[string]any{"root": root, "path": path, "name": name, "size": size})
	w.WriteHeader(http.StatusCreated)
}
