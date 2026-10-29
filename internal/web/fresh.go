package web

import (
	"bytes"
	"hash/maphash"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// maxTaggedFragment is the largest fragment buffered to tag; larger
// responses stream untagged.
const maxTaggedFragment = 4 << 20

var fragmentBuffers = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// fragmentSeed keys fragment tags. Tags only need to match within one run:
// after a restart every region refreshes in full once.
var fragmentSeed = maphash.MakeSeed()

// tagFragments lets live regions skip refreshes that changed nothing. An htmx
// GET answered with HTML gets an ETag of its body and htmx headers; a region
// sends the tag it shows as If-None-Match, and an unchanged fragment gets a
// bodiless 304. Responses are no-store, so browsers never answer from a cache.
func tagFragments(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("HX-Request") != "true" {
			next.ServeHTTP(w, r)
			return
		}
		buffer := fragmentBuffers.Get().(*bytes.Buffer)
		writer := &taggedWriter{ResponseWriter: w, body: buffer}
		next.ServeHTTP(writer, r)
		writer.finish(r.Header.Get("If-None-Match"))
		if buffer.Cap() <= maxTaggedFragment {
			buffer.Reset()
			fragmentBuffers.Put(buffer)
		}
	})
}

// taggedWriter holds a successful HTML response until it can be tagged.
// Anything else, or a body past maxTaggedFragment, passes through.
type taggedWriter struct {
	http.ResponseWriter
	body      *bytes.Buffer
	status    int
	streaming bool
}

func (w *taggedWriter) WriteHeader(status int) {
	if status < http.StatusOK {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	if status != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		w.stream()
	}
}

func (w *taggedWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming {
		return w.ResponseWriter.Write(data)
	}
	if w.body.Len()+len(data) > maxTaggedFragment {
		w.stream()
		return w.ResponseWriter.Write(data)
	}
	return w.body.Write(data)
}

// stream sends what is held and passes the rest through.
func (w *taggedWriter) stream() {
	if w.streaming {
		return
	}
	w.streaming = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.ResponseWriter.WriteHeader(w.status)
	if w.body.Len() > 0 {
		w.ResponseWriter.Write(w.body.Bytes())
	}
}

// Flush passes through once streaming. templ flushes after every render,
// so a held fragment stays held; no fragment is streamed on purpose.
func (w *taggedWriter) Flush() {
	if !w.streaming {
		return
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *taggedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish tags a held response, and answers 304 if the region has it.
func (w *taggedWriter) finish(shown string) {
	if w.streaming {
		return
	}
	if w.status == 0 {
		// Nothing was written: an empty 200, sent as is.
		w.stream()
		return
	}
	tag := fragmentTag(w.Header(), w.body.Bytes())
	w.Header().Set("ETag", tag)
	// A proxy that re-encodes bodies may weaken the tag.
	if strings.TrimPrefix(shown, "W/") == tag {
		w.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(http.StatusNotModified)
		return
	}
	w.ResponseWriter.WriteHeader(http.StatusOK)
	w.ResponseWriter.Write(w.body.Bytes())
}

// fragmentTag hashes a body with the htmx headers sent with it, which
// can trigger events or move the page.
func fragmentTag(header http.Header, body []byte) string {
	var hash maphash.Hash
	hash.SetSeed(fragmentSeed)
	var names []string
	for name := range header {
		if strings.HasPrefix(name, "Hx-") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		for _, value := range header[name] {
			hash.WriteString(name + ": " + value + "\n")
		}
	}
	hash.WriteByte(0)
	hash.Write(body)
	return `"` + strconv.FormatUint(hash.Sum64(), 36) + `"`
}
