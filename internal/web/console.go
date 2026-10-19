package web

import (
	"net/http"
	"slices"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/web/views"
)

// consolePage shows Docker logs, a game console, or an administrator shell.
// The socket authorizes on its own; the page only decides what to offer.
func (s *Server) consolePage(w http.ResponseWriter, r *http.Request, request *Request) {
	snapshot, err := s.App.Servers.Snapshot(r.Context(), request.User(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	view := snapshot.Server
	c := views.ConsoleView{ServerID: view.ID, ServerName: view.DisplayName, State: view.State, Health: view.Health, Exit: view.Exit,
		Running: view.State == "running", BindingActive: view.BindingStatus == "active" && !snapshot.DiscoveryUnavailable,
		CanStart: can(view, authz.ServerStart) && views.LifecycleAction(view.State) == "start"}
	if can(view, authz.LogsRead) {
		c.Modes = append(c.Modes, "logs")
	}
	if can(view, authz.ConsoleExecute) && view.Console != nil {
		c.Modes = append(c.Modes, "game")
		c.ConsoleName, c.Placeholder, c.Quick = view.Console.Name, view.Console.CommandPlaceholder, view.Console.Commands
	}
	if can(view, authz.ConsoleShell) {
		c.Modes = append(c.Modes, "shell")
	}
	c.Mode = r.URL.Query().Get("mode")
	if !slices.Contains(c.Modes, c.Mode) {
		switch {
		case slices.Contains(c.Modes, "game"):
			c.Mode = "game"
		case slices.Contains(c.Modes, "logs"):
			c.Mode = "logs"
		default:
			c.Mode = "shell"
		}
	}
	if c.Mode != "game" {
		c.Quick = nil
	}
	if slices.Contains(c.Modes, c.Mode) && c.BindingActive && (c.Mode == "logs" || c.Running) {
		c.Socket = "/ws/console/" + view.ID + "?mode=" + c.Mode
	}
	page := s.page(r, request, view.DisplayName+" console")
	page.Console = true
	page.Path = "/"
	page.Styles = []string{"vendor/xterm.css"}
	page.Scripts = []string{"vendor/xterm.js", "vendor/xterm-addon-fit.js", "vendor/xterm-addon-web-links.js", "console.js"}
	render(w, r, http.StatusOK, views.ConsolePage(page, c))
}
