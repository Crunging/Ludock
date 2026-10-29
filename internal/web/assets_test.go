package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsRevalidateAndCompressText(t *testing.T) {
	t.Parallel()
	get := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		serveAsset(response, request)
		return response
	}
	// The stylesheet names its fonts with their versions, so they are cached
	// like it; an unversioned request revalidates instead.
	font := "fonts/archivo-latin-wdth-normal.woff2"
	stylesheet, _ := load("ludock.css")
	if !strings.Contains(string(stylesheet.data), `url("`+font+"?v="+versionOf(font)+`")`) {
		t.Fatal("stylesheet names an unversioned font")
	}
	if versioned := get("/assets/"+font+"?v="+versionOf(font), nil); !strings.Contains(versioned.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("versioned font not cached", versioned.Header())
	}
	first := get("/assets/"+font, nil)
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" || first.Header().Get("Content-Encoding") != "" || first.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal(first.Code, first.Header())
	}
	if again := get("/assets/"+font, map[string]string{"If-None-Match": tag}); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatal("refetched an unchanged font", again.Code)
	}
	style := get("/assets/ludock.css", map[string]string{"Accept-Encoding": "gzip, br"})
	if style.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("stylesheet not compressed")
	}
	reader, err := gzip.NewReader(style.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(reader)
	want := stylesheet.data
	if string(plain) != string(want) || style.Body.Len() >= len(want) {
		t.Fatal("compressed stylesheet differs or is not smaller")
	}
	if raw := get("/assets/ludock.css", nil); raw.Header().Get("Content-Encoding") != "" || raw.Body.String() != string(want) {
		t.Fatal("compressed for a client that did not ask")
	}
	for header, want := range map[string]bool{"gzip;q=0, identity": false, "*;q=0.5": true, "*, gzip;q=0": false,
		"GZIP; Q=0.8": true, "*;q=0": false, "br": false} {
		if acceptsGzip(header) != want {
			t.Error(header, "accepted:", !want)
		}
	}
}
