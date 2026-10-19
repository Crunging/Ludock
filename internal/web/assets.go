package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/Crunging/Ludock/internal/web/views"
)

//go:embed assets
var embedded embed.FS

var (
	assetsFS, _  = fs.Sub(embedded, "assets")
	assetVersion sync.Map // name -> short content hash
)

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
	if kind := mime.TypeByExtension(path.Ext(name)); kind != "" {
		w.Header().Set("Content-Type", kind)
	}
	if r.URL.Query().Get("v") == versionOf(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Write(data)
}
