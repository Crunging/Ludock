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
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/web/views"
)

//go:embed assets
var embedded embed.FS

var (
	assetsFS, _ = fs.Sub(embedded, "assets")
	assets      sync.Map // name -> *asset
)

// asset is an embedded file as served, read once.
type asset struct {
	data    []byte
	version string // short content hash
	kind    string
	gzip    func() []byte
}

var stylesheetURL = regexp.MustCompile(`url\("([a-z0-9][a-z0-9/._-]*)"\)`)

// load reads an asset once. A stylesheet names the files it uses with their
// versions, so they are cached as long as it is, and its own version covers
// theirs.
func load(name string) (*asset, bool) {
	if value, ok := assets.Load(name); ok {
		return value.(*asset), true
	}
	data, err := fs.ReadFile(assetsFS, name)
	if err != nil {
		return nil, false
	}
	if path.Ext(name) == ".css" {
		data = stylesheetURL.ReplaceAllFunc(data, func(match []byte) []byte {
			target := path.Join(path.Dir(name), string(stylesheetURL.FindSubmatch(match)[1]))
			referenced, ok := load(target)
			if !ok {
				return match
			}
			return []byte(`url("` + string(stylesheetURL.FindSubmatch(match)[1]) + "?v=" + referenced.version + `")`)
		})
	}
	sum := sha256.Sum256(data)
	a := &asset{data: data, version: hex.EncodeToString(sum[:6]), kind: mime.TypeByExtension(path.Ext(name))}
	// Assets hold no secrets, so compressing them cannot leak one; pages are
	// left uncompressed.
	a.gzip = sync.OnceValue(func() []byte {
		var out bytes.Buffer
		writer, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
		writer.Write(a.data)
		writer.Close()
		return out.Bytes()
	})
	value, _ := assets.LoadOrStore(name, a)
	return value.(*asset), true
}

func versionOf(name string) string {
	if a, ok := load(name); ok {
		return a.version
	}
	return ""
}

func init() {
	views.Asset = func(name string) string { return "/assets/" + name + "?v=" + versionOf(name) }
}

// serveAsset serves embedded files. Versioned URLs are cached for a year.
func serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/assets/")
	if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") || strings.HasPrefix(path.Base(name), "LICENSE") {
		http.NotFound(w, r)
		return
	}
	a, ok := load(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if a.kind != "" {
		w.Header().Set("Content-Type", a.kind)
	}
	if r.URL.Query().Get("v") == a.version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Unversioned URLs revalidate.
		w.Header().Set("Cache-Control", "no-cache")
	}
	tag := `"` + a.version + `"`
	w.Header().Set("ETag", tag)
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if strings.HasPrefix(a.kind, "text/") || strings.Contains(a.kind, "javascript") || strings.Contains(a.kind, "svg") {
		w.Header().Set("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(a.gzip())
			return
		}
	}
	w.Write(a.data)
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
