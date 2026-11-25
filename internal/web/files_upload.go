package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/datasize"
	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/helper"
	"github.com/Crunging/Ludock/internal/launch"
)

// uploadStall is how long an upload may go without a byte arriving before
// it is given up; tests shorten it. The whole upload has no such bound, so a
// large file over a slow link still arrives.
var uploadStall = 30 * time.Second

// manifestLimit bounds an upload's list of files: 100 paths of up to
// files.MaxPath bytes fit, each escaped.
const manifestLimit = 1 << 20

// manifestFile is a file of an upload: its path below the upload's folder,
// and whether it may replace a file there.
type manifestFile struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Replace bool   `json:"replace,omitempty"`
}

type uploadResult struct {
	Name string `json:"name"`
	// Status is "uploaded", "failed", "exists": the name is taken and the
	// upload wasn't confirmed to replace it, or "unconfirmed": the file may
	// have been written.
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// fileUpload stores a batch of files below the folder path, making the
// folders they need. The body (application/octet-stream) is a JSON line
// listing each file's path below it, size, and whether it may replace a
// file, then each file's bytes in that order, with the exact
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
	root := query.Get("root")
	folder, err := files.NormalizeRelativePath(query.Get("path"))
	if err == nil && root == "" {
		err = invalid
	}
	// A page from before files carried their own paths would put a folder's
	// files in the folder shown.
	if err == nil && (query.Has("into") || query.Has("replace")) {
		err = apperr.New("UPLOAD_PAGE_OUTDATED", 400, "Ludock was updated since this page loaded. Reload Files, then upload again.")
	}
	// The list is read only for someone who may write here; startAction
	// rechecks it, with the locks, once the list is in.
	if err == nil {
		_, err = s.App.Authz.Assert(r.Context(), request.User(), r.PathValue("id"), authz.FilesWrite)
	}
	// The list must arrive within one uploadStall in all.
	body := &progressBody{body: r.Body, controller: http.NewResponseController(w), until: time.Now().Add(uploadStall)}
	defer body.close()
	// Writing a response reads what is left of a short body first, which
	// waits on a stalled client unless reading has ended.
	fail := func(err error) {
		body.close()
		s.fail(w, r, request, err)
	}
	if err != nil {
		fail(err)
		return
	}
	reader := bufio.NewReader(body)
	line, err := readManifest(reader)
	var manifest []manifestFile
	if err == nil {
		manifest, err = decodeManifest(line)
	}
	if errors.Is(err, files.ErrBatchTooLarge) {
		fail(err)
		return
	}
	if err != nil || len(manifest) == 0 {
		fail(invalid)
		return
	}
	body.unbound()
	sizes, names := make([]int64, len(manifest)), make([]string, len(manifest))
	for index, file := range manifest {
		if file.Size < 0 || file.Size > 1<<53-1 {
			fail(invalid)
			return
		}
		sizes[index], names[index] = file.Size, file.Name
	}
	// Refused files are read past, so a batch past what the page sends is
	// refused before any of it is read.
	if !files.BatchFits(sizes) || files.BatchFolders(names) > helper.UploadBatchFolders {
		fail(files.ErrBatchTooLarge)
		return
	}
	limit, _ := s.App.Advanced.UploadLimit(r.Context())
	entries := make([]files.UploadEntry, len(manifest))
	total := int64(len(line))
	for index, file := range manifest {
		entries[index] = files.UploadEntry{Name: file.Name, Size: file.Size, Replace: file.Replace}
		if limit > 0 && file.Size > limit {
			entries[index].Err = apperr.New("UPLOAD_TOO_LARGE", 413, "File exceeds the upload size limit of "+datasize.Format(limit))
		}
		total += file.Size
	}
	if r.ContentLength != total {
		fail(invalid)
		return
	}
	action, err := s.startAction(r, request, r.PathValue("id"), authz.FilesWrite)
	if err != nil {
		fail(err)
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
		// The audit names the file in the folder it went in.
		inside, name := path.Split(entry.Name)
		details := map[string]any{"root": root, "path": path.Join(folder, inside), "name": name, "size": entry.Size}
		switch failure := results[index]; {
		case errors.Is(failure, launch.ErrNameTaken):
			result.Status = "exists"
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
		fail(err)
		return
	}
	body.close()
	writeJSON(w, http.StatusOK, answer)
}

// decodeManifest reads {"files":[…]}, stopping at the first file past a
// batch.
func decodeManifest(line []byte) ([]manifestFile, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))
	expect := func(want json.Token) error {
		token, err := decoder.Token()
		if err == nil && token != want {
			err = errors.New("unexpected upload list")
		}
		return err
	}
	if err := errors.Join(expect(json.Delim('{')), expect("files"), expect(json.Delim('['))); err != nil {
		return nil, err
	}
	var list []manifestFile
	for decoder.More() {
		if len(list) == helper.UploadBatchFiles {
			return nil, files.ErrBatchTooLarge
		}
		var file manifestFile
		if err := decoder.Decode(&file); err != nil {
			return nil, err
		}
		list = append(list, file)
	}
	if err := errors.Join(expect(json.Delim(']')), expect(json.Delim('}'))); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected upload list")
	}
	return list, nil
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
	// until, when set, is the latest any read may end.
	until time.Time
}

func (b *progressBody) unbound() {
	b.mu.Lock()
	b.until = time.Time{}
	b.mu.Unlock()
}

var errUploadClosed = errors.New("upload request closed")

func (b *progressBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, errUploadClosed
	}
	deadline := time.Now().Add(uploadStall)
	if !b.until.IsZero() && b.until.Before(deadline) {
		deadline = b.until
	}
	b.controller.SetReadDeadline(deadline)
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
