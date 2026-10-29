package web

import (
	"compress/gzip"
	"net/http"
	"strings"
	"sync"
)

var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return writer
}}

// compressPages gzips HTML for browsers that accept it. Pages mask their CSRF
// token per response (maskCSRF), so compressed sizes can't reveal it to another
// site.
func compressPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || r.Header.Get("Upgrade") != "" || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		writer := &gzipWriter{ResponseWriter: w}
		defer writer.close()
		next.ServeHTTP(writer, r)
	})
}

// gzipWriter compresses the response if its headers say it is HTML.
type gzipWriter struct {
	http.ResponseWriter
	gzip    *gzip.Writer
	decided bool
}

func (w *gzipWriter) WriteHeader(status int) {
	if !w.decided && status >= http.StatusOK {
		w.decided = true
		header := w.Header()
		if strings.HasPrefix(header.Get("Content-Type"), "text/html") && header.Get("Content-Encoding") == "" &&
			status != http.StatusNoContent && status != http.StatusNotModified {
			header.Del("Content-Length")
			header.Set("Content-Encoding", "gzip")
			header.Add("Vary", "Accept-Encoding")
			w.gzip = gzipWriters.Get().(*gzip.Writer)
			w.gzip.Reset(w.ResponseWriter)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.gzip != nil {
		return w.gzip.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

// Flush sends what is compressed so far; event streams pass through.
func (w *gzipWriter) Flush() {
	if w.gzip != nil {
		w.gzip.Flush()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gzipWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipWriter) close() {
	if w.gzip != nil {
		w.gzip.Close()
		gzipWriters.Put(w.gzip)
		w.gzip = nil
	}
}
