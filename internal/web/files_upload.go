package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/files"
)

// uploadStall is how long an upload may go without a byte arriving before
// it is given up; tests shorten it. The whole upload has no such bound, so a
// large file over a slow link still arrives.
var uploadStall = 30 * time.Second

// manifestLimit bounds an upload's list of files.
const manifestLimit = 512 * 1024

type uploadManifest struct {
	Files []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

type uploadResult struct {
	Name string `json:"name"`
	// Status is "uploaded", "failed", or "unconfirmed": the file may have
	// been written.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// fileUpload stores a batch of files in one folder. The body
// (application/octet-stream) is a JSON line listing each file's name and
// size, then each file's bytes in that order, with the exact
// Content-Length; the CSRF token comes in the X-CSRF-Token header. It
// answers with each file's result, and stop when the files after a failure
// were not tried.
func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request, request *Request) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != "application/octet-stream" {
		http.Error(w, "Uploads must use application/octet-stream", http.StatusUnsupportedMediaType)
		return
	}
	invalid := apperr.New("INVALID_REQUEST", 400, "Invalid upload request")
	query := r.URL.Query()
	root, folder := query.Get("root"), query.Get("path")
	body := &progressBody{body: r.Body, controller: http.NewResponseController(w)}
	defer body.close()
	reader := bufio.NewReader(body)
	line, err := readManifest(reader)
	var manifest uploadManifest
	if err != nil || root == "" || json.Unmarshal(line, &manifest) != nil || len(manifest.Files) == 0 || len(manifest.Files) > files.MaxBatch {
		s.fail(w, r, request, invalid)
		return
	}
	limit, _ := s.App.Advanced.UploadLimit(r.Context())
	entries := make([]files.UploadEntry, len(manifest.Files))
	total := int64(len(line))
	for index, file := range manifest.Files {
		if file.Size < 0 || file.Size > 1<<53-1 {
			s.fail(w, r, request, invalid)
			return
		}
		entries[index] = files.UploadEntry{Name: file.Name, Size: file.Size}
		if limit > 0 && file.Size > limit {
			entries[index].Err = apperr.New("UPLOAD_TOO_LARGE", 413, "File exceeds the upload size limit of "+datasize.Format(limit))
		}
		total += file.Size
	}
	if r.ContentLength != total {
		s.fail(w, r, request, invalid)
		return
	}
	action, err := s.startAction(r, request, r.PathValue("id"), authz.FilesWrite)
	if err != nil {
		s.fail(w, r, request, err)
		return
	}
	defer action.Done()
	results, err := s.App.Files.UploadFiles(action.Ctx, fileTarget(action), root, folder, entries, reader, action.Assert)
	answer := struct {
		Results []uploadResult `json:"results"`
		Stop    bool           `json:"stop"`
	}{Results: make([]uploadResult, len(entries)), Stop: err != nil}
	for index, entry := range entries {
		result := uploadResult{Name: entry.Name, Status: "uploaded"}
		details := map[string]any{"root": root, "path": folder, "name": entry.Name, "size": entry.Size}
		switch failure := results[index]; {
		case errors.Is(failure, files.ErrUploadUnconfirmed):
			result.Status, result.Message = "unconfirmed", "The result for “"+entry.Name+"” could not be confirmed. The upload may have completed. Review the folder before uploading again."
			details["confirmed"] = false
			s.audit(r, request, "server.file.uploaded", action.Logical.ID, details)
		case failure != nil:
			result.Status, result.Message = "failed", publicMessage(failure)
		default:
			s.audit(r, request, "server.file.uploaded", action.Logical.ID, details)
		}
		answer.Results[index] = result
	}
	if public, ok := apperr.As(err); ok && public.Status == http.StatusUnauthorized {
		s.fail(w, r, request, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func readManifest(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > manifestLimit {
			return nil, errors.New("upload manifest too long")
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// progressBody gives up a request body that receives nothing for
// uploadStall. Its reads can outlast the handler, which must call close
// before it returns.
type progressBody struct {
	body       io.Reader
	controller *http.ResponseController
	mu         sync.Mutex
	closed     bool
	ended      bool
}

var errUploadClosed = errors.New("upload request closed")

func (b *progressBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, errUploadClosed
	}
	b.controller.SetReadDeadline(time.Now().Add(uploadStall))
	b.mu.Unlock()
	n, err := b.body.Read(p)
	if errors.Is(err, io.EOF) {
		b.mu.Lock()
		// Once the body is read, Go watches the connection for the client
		// leaving, and a passing deadline would cancel the request.
		if !b.closed {
			b.ended = true
			b.controller.SetReadDeadline(time.Time{})
		}
		b.mu.Unlock()
	}
	return n, err
}

// close ends any read still waiting, so a failed upload releases its
// connection at once instead of when the client next sends.
func (b *progressBody) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if !b.ended {
		b.controller.SetReadDeadline(time.Now())
	}
}
