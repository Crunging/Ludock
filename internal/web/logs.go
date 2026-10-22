package web

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/Crunging/Ludock/internal/logging"
	"github.com/Crunging/Ludock/internal/web/views"
)

// logsView filters the process's recent log buffer. Entries were redacted
// when they were written.
func logsView(r *http.Request) views.LogsView {
	query := r.URL.Query()
	l := views.LogsView{Search: query.Get("q"), Level: query.Get("level"), Component: query.Get("component"),
		Paused: query.Get("paused") == "1"}
	generation, entries := logging.List(0, 1000, "")
	l.Total, l.Generation = len(entries), generation
	if len(entries) > 0 {
		l.Newest = entries[len(entries)-1].ID
	}
	search := strings.ToLower(strings.TrimSpace(l.Search))
	components := map[string]bool{}
	if l.Component != "" {
		components[l.Component] = true
	}
	for _, entry := range entries {
		components[entry.Component] = true
		context := ""
		if len(entry.Context) > 0 {
			context = string(logging.EncodeFields(entry.Context))
		}
		if (l.Level != "" && entry.Level != l.Level) || (l.Component != "" && entry.Component != l.Component) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(strings.Join([]string{entry.Message, entry.Component, entry.Level, context}, " ")), search) {
			continue
		}
		l.Entries = append(l.Entries, views.LogEntry{Entry: entry, ContextJSON: context})
	}
	for component := range components {
		l.Components = append(l.Components, component)
	}
	slices.Sort(l.Components)
	return l
}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request, request *Request) {
	render(w, r, http.StatusOK, views.LogsPage(s.page(r, request, "Ludock logs"), logsView(r)))
}

func (s *Server) logsFragment(w http.ResponseWriter, r *http.Request, request *Request) {
	// A poll of an unchanged buffer keeps what the page shows.
	if r.Header.Get("HX-Trigger") == "application-logs" {
		generation, newest := logging.Latest()
		if query := r.URL.Query(); query.Get("generation") == generation && query.Get("since") == strconv.FormatInt(newest, 10) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	l := logsView(r)
	if r.Header.Get("HX-Trigger") != "application-logs" {
		w.Header().Set("HX-Replace-Url", strings.TrimSuffix("/logs?"+l.Query(), "?"))
	}
	render(w, r, http.StatusOK, views.LogEntries(l))
}
