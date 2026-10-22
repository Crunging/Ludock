package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/web/views"
)

//go:embed assets
var embedded embed.FS

var (
	assetsFS, _  = fs.Sub(embedded, "assets")
	assetVersion sync.Map // name -> short content hash
	assetGzip    sync.Map // name -> gzipped content
)

// gzipped compresses a text asset once. Assets hold no secrets, so
// compressing them cannot leak one; pages are left uncompressed.
func gzipped(name string, data []byte) []byte {
	if value, ok := assetGzip.Load(name); ok {
		return value.([]byte)
	}
	var out bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	writer.Write(data)
	writer.Close()
	assetGzip.Store(name, out.Bytes())
	return out.Bytes()
}

func versionOf(name string) string {
	if value, ok := assetVersion.Load(name); ok {
		return value.(string)
	}
	data, err := fs.ReadFile(assetsFS, name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	version := hex.EncodeToString(sum[:6])
	assetVersion.Store(name, version)
	return version
}

func init() {
	views.Asset = func(name string) string { return "/assets/" + name + "?v=" + versionOf(name) }
}

// serveAsset serves embedded files. Versioned URLs are cached for a year.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/assets/")
	if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(assetsFS, name)
	if err != nil || strings.HasPrefix(path.Base(name), "LICENSE") {
		http.NotFound(w, r)
		return
	}
	kind := mime.TypeByExtension(path.Ext(name))
	if kind != "" {
		w.Header().Set("Content-Type", kind)
	}
	if r.URL.Query().Get("v") == versionOf(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Unversioned URLs, such as fonts the stylesheet names, revalidate.
		w.Header().Set("Cache-Control", "no-cache")
	}
	tag := `"` + versionOf(name) + `"`
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.HasPrefix(kind, "text/") || strings.Contains(kind, "javascript") || strings.Contains(kind, "svg") {
		w.Header().Set("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(gzipped(name, data))
			return
		}
	}
	w.Write(data)
}

// acceptsGzip reports whether an Accept-Encoding value allows gzip: listed,
// or covered by "*", with a nonzero quality.
func acceptsGzip(header string) bool {
	allowed := false
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "gzip" && name != "*" {
			continue
		}
		quality := 1.0
		for _, param := range strings.Split(params, ";") {
			if key, value, ok := strings.Cut(strings.TrimSpace(param), "="); ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = parsed
				}
			}
		}
		// An explicit gzip entry overrides "*".
		if name == "gzip" {
			return quality > 0
		}
		allowed = quality > 0
	}
	return allowed
}
