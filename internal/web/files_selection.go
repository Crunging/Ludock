package web

import (
	"net/http"
	"slices"

	"github.com/a-h/templ"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/web/views"
)

// moveView lists the folders of the move dialog's current folder. The
// selected entries are left out of their own folder, since nothing moves
// into itself.
func (s *Server) moveView(action *serverAction, m *views.FileMove) {
	m.ServerName = action.Container.DisplayName
	if index := slices.IndexFunc(action.Container.FileRoots, func(root files.Root) bool { return root.ID == m.Root.ID }); index >= 0 {
		m.Root = action.Container.FileRoots[index]
	}
	listing, err := s.App.Files.List(action.Ctx, fileTarget(action), m.Root.ID, m.To, action.Assert)
	if err != nil {
		m.ListError = publicMessage(err)
		return
	}
	m.To = listing.Path
	sortEntries(listing.Entries, "name-asc")
	for _, entry := range listing.Entries {
		if entry.Type == "directory" && (m.To != m.Path || !slices.Contains(m.Names, entry.Name)) {
			m.Folders = append(m.Folders, entry.Name)
		}
	}
}

// fileMoveDialog opens the move dialog, or with the dialog as its target,
// shows another folder inside it.
func (s *Server) fileMoveDialog(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	m := views.FileMove{ServerID: r.PathValue("id"), Root: files.Root{ID: query.Get("root")}, Path: query.Get("path"),
		To: query.Get("to"), Names: query["name"]}
	if !query.Has("to") {
		m.To = m.Path
	}
	if len(m.Names) == 0 || len(m.Names) > files.MaxSelection {
		s.fail(w, r, request, files.Error("INVALID_SELECTION", 400))
		return
	}
	action, err := s.startAction(r, request, m.ServerID, authz.FilesWrite)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	s.moveView(action, &m)
	if htmxTarget(r) == "file-action" {
		render(w, r, http.StatusOK, views.FileMoveContent(m))
		return
	}
	render(w, r, http.StatusOK, views.FileMoveDialog(m))
}

func (s *Server) fileMove(w http.ResponseWriter, r *http.Request, request *Request) {
	m := views.FileMove{ServerID: r.PathValue("id"), Root: files.Root{ID: r.PostFormValue("root")}, Path: r.PostFormValue("path"),
		To: r.PostFormValue("to"), Names: r.PostForm["name"]}
	action, err := s.startAction(r, request, m.ServerID, authz.FilesWrite)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	err = s.App.Files.Move(action.Ctx, fileTarget(action), m.Root.ID, m.Path, m.Names, m.To, action.Assert)
	destination := m.To
	if destination == "" {
		destination = "the top folder"
	} else {
		destination = "“" + destination + "”"
	}
	details := map[string]any{"root": m.Root.ID, "path": canonicalPath(m.Path), "names": m.Names, "destination": canonicalPath(m.To)}
	s.fileOutcome(w, r, request, action, err, "server.file.moved", details, "Moved "+views.ItemCount(len(m.Names))+" to "+destination+".",
		"the selected items", func(message string) templ.Component {
			s.moveView(action, &m)
			m.Error = message
			return views.FileMoveDialog(m)
		})
}

// fileSelectionDownload streams entries of one folder as one tar archive.
func (s *Server) fileSelectionDownload(w http.ResponseWriter, r *http.Request, request *Request) {
	query := r.URL.Query()
	root, path, names := query.Get("root"), query.Get("path"), query["name"]
	action, err := s.startAction(r, request, r.PathValue("id"), authz.FilesRead)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	download, err := s.App.Files.OpenSelectionDownload(action.Ctx, fileTarget(action), root, path, names, action.Assert)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer download.Close()
	s.audit(r, request, "server.file.downloaded", action.Logical.ID, map[string]any{"root": root, "path": canonicalPath(path), "names": names})
	s.streamDownload(w, action, download)
}
