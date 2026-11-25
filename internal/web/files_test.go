package web

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/auth"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/launch"
)

func TestFilesFollowGrants(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	viewer := p.user("u-viewer", "viewer", db.RoleMember)
	if page := p.do("GET", "/files/"+p.server, viewer, nil, nil).Body.String(); strings.Contains(page, "valheim") {
		t.Fatal("hidden server named")
	}
	p.grant(viewer, authz.ServerView)
	if page := p.do("GET", "/files/"+p.server, viewer, nil, nil).Body.String(); !strings.Contains(page, "You can’t open this server’s files.") {
		t.Fatal(page)
	}
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView, authz.FilesRead)
	page := p.do("GET", "/files/"+p.server, operator, nil, nil).Body.String()
	if !strings.Contains(page, "No folders to show") || strings.Contains(page, "data-dropzone") {
		t.Fatal(page)
	}
	// Changes the page never offers a reader are refused when forged.
	for _, action := range []string{"create", "delete", "delete-selection", "unpack", "move"} {
		target := "/files/" + p.server + "/" + action
		form := url.Values{"_csrf": {CSRFToken(operator.Token)}, "root": {"0"}, "path": {"world"}, "name": {"x"}, "to": {""}}
		response := p.do("POST", target, operator, strings.NewReader(form.Encode()), map[string]string{"HX-Request": "true"})
		if response.Code != http.StatusForbidden {
			t.Fatal(target, response.Code)
		}
	}
	for _, dialog := range []string{"/dialog?action=delete&root=0&path=x", "/dialog?action=unpack&root=0&path=x.zip",
		"/dialog?action=delete-selection&root=0&name=x", "/move?root=0&name=x"} {
		if response := p.do("GET", "/fragments/files/"+p.server+dialog, operator, nil,
			map[string]string{"HX-Request": "true"}); response.Code != http.StatusForbidden {
			t.Fatal(dialog, response.Code)
		}
	}
}

// A user who may read files but not write them forges a save, which the
// preview never offers them.
func TestFileSaveNeedsWriteGrant(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	operator := p.user("u-operator", "operator", db.RoleMember)
	p.grant(operator, authz.ServerView, authz.FilesRead)
	version := "3-" + strings.Repeat("0", 64)
	response := p.do("PUT", "/files/"+p.server+"/save?root=0&path=a.txt&version="+version, operator, strings.NewReader("hi\n"),
		map[string]string{"Content-Type": "application/octet-stream", "X-CSRF-Token": CSRFToken(operator.Token), "HX-Request": "true"})
	if response.Code != http.StatusForbidden {
		t.Fatal(response.Code, response.Body.String())
	}
}

// Forged uploads: without the CSRF token, as another type, with a file over
// the size limit, which the page never sends, or to a server ID that isn't
// one.
func TestUploadChecks(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	server := p.server
	upload := func(headers map[string]string, body string) *httptest.ResponseRecorder {
		request, _ := http.NewRequest("PUT", "http://panel.example/files/"+server+"/upload?root=0", strings.NewReader(body))
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: p.admin.Token})
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		p.handler.ServeHTTP(response, request)
		return response
	}
	csrf := CSRFToken(p.admin.Token)
	if code := upload(map[string]string{"Content-Type": "application/octet-stream"}, "hi").Code; code != http.StatusForbidden {
		t.Fatal("no csrf", code)
	}
	if code := upload(map[string]string{"Content-Type": "text/plain", "X-CSRF-Token": csrf}, "hi").Code; code != http.StatusUnsupportedMediaType {
		t.Fatal("type", code)
	}
	p.app.Advanced.Env.MaxUploadBytes = 3
	response := upload(map[string]string{"Content-Type": "application/octet-stream", "X-CSRF-Token": csrf}, `{"files":[{"name":"a.txt","size":4}]}`+"\nfour")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"failed","message":"File exceeds the upload size limit of 3 B"`) {
		t.Fatal("size", response.Code, response.Body.String())
	}
	// A route ID that is no server's is refused like a hidden server, and its
	// denial is audited without it.
	server = strings.Repeat("x", 5000)
	if code := upload(map[string]string{"Content-Type": "application/octet-stream", "X-CSRF-Token": csrf}, "").Code; code != http.StatusNotFound {
		t.Fatal("forged server", code)
	}
	var longest int
	p.app.DB.QueryRow(`SELECT COALESCE(MAX(LENGTH(target_id)), 0) FROM audit_log WHERE action = 'authorization.denied'`).Scan(&longest)
	if longest > 36 {
		t.Fatal("audited a forged server ID of", longest, "bytes")
	}
	// A destination that would put a selected name past the bound is said
	// in the move dialog, which keeps its way back.
	deep := p.do("GET", "/fragments/files/"+p.server+"/move?root=0&name=selected&to="+strings.Repeat("d", 4090), p.admin, nil,
		map[string]string{"HX-Request": "true", "HX-Target": "file-action"})
	if body := deep.Body.String(); deep.Code != http.StatusOK || !strings.Contains(body, "Invalid file path") || !strings.Contains(body, "Cancel") {
		t.Fatal("move dialog lost its controls", deep.Code, body)
	}
	// Dialogs refuse a path past the bound, composed as the change would.
	folder := strings.Repeat("d", 4090)
	for _, dialog := range []string{"/dialog?action=delete&root=0&path=" + strings.Repeat("d", 4097),
		"/dialog?action=delete-selection&root=0&path=" + folder + "&name=selected", "/move?root=0&path=" + folder + "&name=selected"} {
		if code := p.do("GET", "/fragments/files/"+p.server+dialog, p.admin, nil, map[string]string{"HX-Request": "true"}).Code; code != http.StatusBadRequest {
			t.Fatal("opened a dialog for a path past the bound", dialog[:30], code)
		}
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

// Reading files confirms its container directly instead of discovering the
// whole fleet for every listing.
func TestFileReadsSkipDiscovery(t *testing.T) {
	t.Parallel()
	p := newPages(t)
	p.engine.Update(p.docker, func(c *fakedocker.Container) { c.State.Running, c.State.Status = true, "running" })
	before := len(p.engine.Requests)
	request := httptest.NewRequest("GET", "/files/"+p.server, nil)
	action, err := p.web.startAction(request, &Request{Principal: p.admin}, p.server, authz.FilesRead)
	if err != nil {
		t.Fatal(err)
	}
	action.Done()
	for _, call := range p.engine.Requests[before:] {
		if strings.HasSuffix(call, "/containers/json") {
			t.Fatal("discovered the fleet", call)
		}
	}
}

// uploadPages is a server whose data folder the fake daemon's helpers
// really write, with a member who may write its files.
func uploadPages(t *testing.T) (p *pages, member *auth.Principal, root, data string) {
	data = filepath.Join(t.TempDir(), "data")
	os.MkdirAll(data, 0o755)
	p = newPagesWith(t, []docker.MountPoint{{Type: "bind", Source: data, Destination: "/config", RW: true}})
	p.engine.RunHelpers(nil)
	helper.HostRoot = "/"
	t.Cleanup(func() { helper.HostRoot = "/host" })
	const image = "ghcr.io/crunging/ludock@sha256:abababababababababababababababababababababababababababababababab"
	p.engine.AddImage(image)
	p.app.Files.Images = &launch.Images{Configured: image}
	member = p.user("u-uploader", "uploader", db.RoleMember)
	p.grant(member, authz.ServerView, authz.FilesRead, authz.FilesWrite)
	views, err := p.app.Servers.List(context.Background(), &p.admin.User)
	if err != nil || len(views[0].FileRoots) != 1 {
		t.Fatal(views, err)
	}
	return p, member, views[0].FileRoots[0].ID, data
}

func manifest(names ...string) string {
	var files []string
	for _, name := range names {
		files = append(files, `{"name":"`+name+`","size":5}`)
	}
	return `{"files":[` + strings.Join(files, ",") + "]}\n"
}

// A batch of files goes through one helper, which writes for a member only
// while they may still write: a grant removed while a batch is sent stops its
// next file, and the helper is removed with it; without the grant, the list
// of files isn't even read. An upload first removes staging files cut-off
// uploads left in its folder, and the audit records the folder it used.
func TestUploadFollowsGrant(t *testing.T) {
	p, member, root, data := uploadPages(t)
	server := httptest.NewServer(p.handler)
	defer server.Close()
	folder := ".//."
	send := func(body io.Reader, length int64) (int, string) {
		request, _ := http.NewRequest("PUT", server.URL+"/files/"+p.server+"/upload?root="+url.QueryEscape(root)+"&path="+url.QueryEscape(folder), body)
		request.ContentLength = length
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: member.Token})
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("X-CSRF-Token", CSRFToken(member.Token))
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		answer, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(answer)
	}
	helpers := func() (started, running int) {
		for _, call := range p.engine.Requests {
			if strings.HasSuffix(call, "/containers/create") {
				started++
			}
		}
		for _, id := range p.engine.SortedIDs() {
			if c := p.engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] != "" {
				running++
			}
		}
		return started, running
	}
	stale := filepath.Join(data, ".ludock-upload-6f9619ff-8b86-4011-b42d-00c04fc964ff.tmp")
	os.WriteFile(stale, []byte("cut off"), 0o600)
	os.Chtimes(stale, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	var batch []string
	for index := range 20 {
		batch = append(batch, fmt.Sprintf("mod-%d.jar", index))
	}
	body := manifest(batch...) + strings.Repeat("hello", len(batch))
	if code, answer := send(strings.NewReader(body), int64(len(body))); code != 200 || strings.Contains(answer, "failed") {
		t.Fatal(code, answer)
	}
	// The helper and the validator that proved its mount.
	if started, _ := helpers(); started != 2 {
		t.Fatal(started, "containers started for 20 files")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("left a cut-off upload's staging file", err)
	}
	var details string
	p.app.DB.QueryRow(`SELECT details_json FROM audit_log WHERE action = 'server.file.uploaded' LIMIT 1`).Scan(&details)
	if !strings.Contains(details, `"path":""`) {
		t.Fatal("audited the folder as sent", details)
	}

	head := manifest("d.txt", "e.txt")
	reader, writer := io.Pipe()
	go func() {
		writer.Write([]byte(head + "hello"))
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(data, "d.txt")); err == nil {
				break
			}
		}
		p.grant(member, authz.ServerView, authz.FilesRead)
		writer.Write([]byte("hello"))
		writer.Close()
	}()
	code, answer := send(reader, int64(len(head)+10))
	if code != 200 || !strings.Contains(answer, `{"name":"d.txt","status":"uploaded"}`) ||
		!strings.Contains(answer, `{"name":"e.txt","status":"failed"`) || !strings.Contains(answer, `"stop":true`) {
		t.Fatal(code, answer)
	}
	if _, err := os.Stat(filepath.Join(data, "e.txt")); !os.IsNotExist(err) {
		t.Fatal("wrote a file after the grant was removed", err)
	}
	if _, running := helpers(); running != 0 {
		t.Fatal(running, "helpers left")
	}
	// The list never arrives, and isn't waited for.
	never, _ := io.Pipe()
	started := time.Now()
	if code, _ := send(never, 1000); code != http.StatusForbidden || time.Since(started) > 5*time.Second {
		t.Fatal("waited for the list of someone who can't write", code, time.Since(started))
	}
	if entries, _ := os.ReadDir(data); len(entries) != 21 {
		t.Fatal(len(entries), "files")
	}
}

// A client that stops sending partway is given up once nothing arrives for
// uploadStall, long before the helper's own minute, and a refused file is
// never read past for long, however large it claims to be and however fast
// it comes: a batch past what the page sends is refused unread, and a lone
// file the helper refuses ends its command. Each time the connection
// closes, and neither a partial file nor a helper is left.
func TestUploadStallReleasesEverything(t *testing.T) {
	previous := uploadStall
	uploadStall = 300 * time.Millisecond
	t.Cleanup(func() { uploadStall = previous })
	p, member, root, data := uploadPages(t)
	os.Mkdir(filepath.Join(data, "world"), 0o755)
	server := httptest.NewServer(p.handler)
	defer server.Close()
	for _, test := range []struct {
		files  string
		size   int64
		stream bool
		want   string
	}{
		{`{"name":"big.bin","size":100000}`, 100000, false, `"status":"failed"`},
		{`{"name":".ludock-mine","size":1099511627776},{"name":"a.txt","size":5}`, 1099511627781, true, "Upload at most 100 files"},
		{`{"name":"world","size":67108864}`, 64 << 20, true, `"status":"failed","message":"The path changed, is unsafe, or cannot be accessed"}],"stop":false`},
	} {
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		head := `{"files":[` + test.files + "]}\n"
		fmt.Fprintf(conn, "PUT /files/%s/upload?root=%s HTTP/1.1\r\nHost: panel.example\r\nCookie: %s=%s\r\nX-CSRF-Token: %s\r\n"+
			"Content-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n%sstart",
			p.server, url.QueryEscape(root), auth.SessionCookie, member.Token, CSRFToken(member.Token), int64(len(head))+test.size, head)
		started := time.Now()
		conn.SetDeadline(started.Add(20 * time.Second))
		sent := make(chan int64, 1)
		go func() {
			total := int64(0)
			for chunk := make([]byte, 64*1024); test.stream && total < test.size; total += int64(len(chunk)) {
				if _, err := conn.Write(chunk); err != nil {
					break
				}
			}
			sent <- total
		}()
		reader := bufio.NewReader(conn)
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(test.files, err)
		}
		answer, _ := io.ReadAll(response.Body)
		if !strings.Contains(string(answer), test.want) || time.Since(started) > 10*time.Second {
			t.Fatal(test.files, string(answer), time.Since(started))
		}
		if _, err := reader.ReadByte(); err != io.EOF {
			t.Fatal(test.files, "the connection stayed open", err)
		}
		if read := <-sent; read > 32<<20 {
			t.Fatal(test.files, "read", read, "bytes past a refused file")
		}
		conn.Close()
		if entries, _ := os.ReadDir(data); len(entries) != 1 {
			t.Fatal(test.files, "left", entries)
		}
		for _, id := range p.engine.SortedIDs() {
			if c := p.engine.Container(id); c != nil && c.Config.Labels["ludock.internal"] != "" {
				t.Fatal(test.files, "helper left")
			}
		}
	}
}
