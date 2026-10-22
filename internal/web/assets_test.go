package web

import (
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAssetsRevalidateAndCompressText(t *testing.T) {
	get := func(path string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		serveAsset(response, request)
		return response
	}
	// The stylesheet names its fonts without a version: they revalidate
	// instead of downloading again.
	font := "/assets/fonts/archivo-latin-wdth-normal.woff2"
	first := get(font, nil)
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" || first.Header().Get("Content-Encoding") != "" {
		t.Fatal(first.Code, first.Header())
	}
	if again := get(font, map[string]string{"If-None-Match": tag}); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
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
	want, _ := fs.ReadFile(assetsFS, "ludock.css")
	if string(plain) != string(want) || style.Body.Len() >= len(want) {
		t.Fatal("compressed stylesheet differs or is not smaller")
	}
	if raw := get("/assets/ludock.css", nil); raw.Header().Get("Content-Encoding") != "" || raw.Body.String() != string(want) {
		t.Fatal("compressed for a client that did not ask")
	}
}
