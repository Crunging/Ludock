package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/helper"
)

func TestFilesFollowGrants(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleViewer)
	if page := p.do("GET", "/files/"+p.server, viewer, nil, nil).Body.String(); strings.Contains(page, "valheim") {
		t.Fatal("hidden server named")
	}
	p.grant(viewer, authz.ServerView)
	if page := p.do("GET", "/files/"+p.server, viewer, nil, nil).Body.String(); !strings.Contains(page, "You do not have file access to this server.") {
		t.Fatal(page)
	}
	operator := p.user("u-operator", "operator", db.RoleOperator)
	p.grant(operator, authz.ServerView, authz.FilesRead)
	page := p.do("GET", "/files/"+p.server, operator, nil, nil).Body.String()
	if !strings.Contains(page, "File access is not configured") || strings.Contains(page, "data-dropzone") {
		t.Fatal(page)
	}
	for _, target := range []string{"/files/" + p.server + "/create", "/files/" + p.server + "/delete"} {
		form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "root": {"0"}, "path": {"world"}, "name": {"x"}}
		response := p.do("POST", target, operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"})
		if response.Code != http.StatusForbidden {
			t.Fatal(target, response.Code)
		}
	}
	if response := p.do("GET", "/fragments/files/"+p.server+"/dialog?action=delete&root=0&path=x", operator, nil,
		map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
		t.Fatal("dialog", response.Code)
	}
}

func TestUploadChecks(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	upload := func(headers map[string]string, body string) int {
		request, _ := http.NewRequest("PUT", "http://panel.example/files/"+p.server+"/upload?root=0&name=a.txt", strings.NewReader(body))
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: p.admin.Token})
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		p.handler.ServeHTTP(response, request)
		return response.Code
	}
	csrf := CSRFToken(p.admin.Token)
	if code := upload(map[string]string{"Content-Type": "application/octet-stream"}, "hi"); code != http.StatusForbidden {
		t.Fatal("no csrf", code)
	}
	if code := upload(map[string]string{"Content-Type": "text/plain", "X-CSRF-Token": csrf}, "hi"); code != http.StatusUnsupportedMediaType {
		t.Fatal("type", code)
	}
	p.app.Config.MaxUploadBytes = 3
	if code := upload(map[string]string{"Content-Type": "application/octet-stream", "X-CSRF-Token": csrf}, "four"); code != http.StatusRequestEntityTooLarge {
		t.Fatal("size", code)
	}
}

// File sort keeps folders first and dashes last.
func TestFileSort(t *testing.T) {
	t.Parallel()
	entries := []helper.FileEntry{{Name: "map10", Type: "file", Size: 5, ModifiedAt: 3}, {Name: "link", Type: "symlink"},
		{Name: "mods", Type: "directory", ModifiedAt: 1}, {Name: "map2", Type: "file", Size: 50, ModifiedAt: 2}}
	names := func() string {
		var list []string
		for _, entry := range entries {
			list = append(list, entry.Name)
		}
		return strings.Join(list, ",")
	}
	for order, want := range map[string]string{"name-asc": "mods,link,map2,map10", "name-desc": "mods,map10,map2,link",
		"size-desc": "mods,map2,map10,link", "size-asc": "mods,map10,map2,link", "modified-desc": "mods,map10,map2,link"} {
		sortEntries(entries, order)
		if names() != want {
			t.Errorf("%s: %s", order, names())
		}
	}
}
