package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// htmx GETs answered with HTML are tagged, and a request that names the tag
// gets 304 with no body, through compression or a proxy that weakened the
// tag. Changed htmx headers change the tag. Pages, posts, errors, other
// types, and fragments too large to hold go out untagged and unchanged.
func TestTagFragments(t *testing.T) {
	body := "<div id=\"servers\">" + strings.Repeat("row ", 100) + "</div>"
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/error":
			html(w)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, body)
			return
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
		case "/large":
			html(w)
			chunk := strings.Repeat("x", 1<<20)
			for range 5 {
				io.WriteString(w, chunk)
			}
			return
		default:
			html(w)
		}
		if trigger := r.URL.Query().Get("trigger"); trigger != "" {
			w.Header().Set("HX-Trigger", trigger)
		}
		// templ flushes after each render; the fragment is still held.
		io.WriteString(w, body[:10])
		w.(http.Flusher).Flush()
		io.WriteString(w, body[10:])
		w.(http.Flusher).Flush()
	}
	server := compressPages(tagFragments(http.HandlerFunc(handler)))
	get := func(method, path, tag string, htmx, compressed bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, nil)
		if htmx {
			request.Header.Set("HX-Request", "true")
		}
		if tag != "" {
			request.Header.Set("If-None-Match", tag)
		}
		if compressed {
			request.Header.Set("Accept-Encoding", "gzip")
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	first := get(http.MethodGet, "/fragment", "", true, false)
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || first.Body.String() != body || !strings.HasPrefix(tag, `"`) {
		t.Fatalf("first fragment: %d %q %q", first.Code, tag, first.Body.String())
	}
	for _, compressed := range []bool{false, true} {
		shown := tag
		if compressed {
			shown = "W/" + tag
		}
		same := get(http.MethodGet, "/fragment", shown, true, compressed)
		if same.Code != http.StatusNotModified || same.Body.Len() != 0 || same.Header().Get("ETag") != tag || same.Header().Get("Content-Encoding") != "" {
			t.Fatalf("unchanged fragment (gzip %v): %d %v %q", compressed, same.Code, same.Header(), same.Body.String())
		}
	}
	zipped := get(http.MethodGet, "/fragment", `"other"`, true, true)
	reader, err := gzip.NewReader(zipped.Body)
	if err != nil {
		t.Fatal(err)
	}
	if unzipped, _ := io.ReadAll(reader); zipped.Code != http.StatusOK || string(unzipped) != body || zipped.Header().Get("ETag") != tag {
		t.Fatalf("stale tag: %d %q", zipped.Code, unzipped)
	}
	triggered := get(http.MethodGet, "/fragment?trigger=servers-changed", tag, true, false)
	if triggered.Code != http.StatusOK || triggered.Header().Get("ETag") == tag {
		t.Fatalf("htmx headers left the tag alone: %d", triggered.Code)
	}

	for _, untagged := range []struct {
		name, method, path string
		htmx               bool
		status             int
	}{
		{"page", http.MethodGet, "/fragment", false, http.StatusOK},
		{"post", http.MethodPost, "/fragment", true, http.StatusOK},
		{"error", http.MethodGet, "/error", true, http.StatusNotFound},
		{"text", http.MethodGet, "/text", true, http.StatusOK},
		{"large", http.MethodGet, "/large", true, http.StatusOK},
	} {
		response := get(untagged.method, untagged.path, tag, untagged.htmx, false)
		if response.Code != untagged.status || response.Header().Get("ETag") != "" || response.Body.Len() == 0 {
			t.Errorf("%s: %d %v", untagged.name, response.Code, response.Header())
		}
		if untagged.name == "large" && response.Body.Len() != 5<<20 {
			t.Errorf("large fragment lost bytes: %d", response.Body.Len())
		}
	}
}
