package fakedocker

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/Crunging/Ludock/internal/docker"
)

var (
	versioned    = regexp.MustCompile(`^/v1\.\d+(/.*)$`)
	container    = regexp.MustCompile(`^/containers/([^/]+)(/[a-z]+)?$`)
	execPath     = regexp.MustCompile(`^/exec/([^/]+)/(start|json)$`)
	volume       = regexp.MustCompile(`^/volumes/([^/]+)$`)
	image        = regexp.MustCompile(`^/images/(.+)/json$`)
	distribution = regexp.MustCompile(`^/distribution/(.+)/json$`)
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]string{"message": "fake docker error"})
}

func (e *Engine) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/fake/") {
		e.control(w, r)
		return
	}
	e.mu.Lock()
	e.Requests = append(e.Requests, r.Method+" "+r.URL.Path)
	injected, hang := e.faultLocked(r.Method+" "+r.URL.Path), e.Hang
	e.mu.Unlock()
	if hang != nil && hang(r.Method+" "+r.URL.Path) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	}
	if injected != nil && injected.stall {
		<-r.Context().Done()
		return
	}
	if injected != nil && injected.status != 0 {
		fail(w, injected.status)
		return
	}
	switch path {
	case "/_ping":
		w.Write([]byte("OK"))
		return
	case "/version":
		writeJSON(w, 200, map[string]string{"ApiVersion": e.APIVersion, "MinAPIVersion": "1.24"})
		return
	}
	match := versioned.FindStringSubmatch(path)
	if match == nil {
		fail(w, 404)
		return
	}
	path = match[1]
	switch {
	case path == "/containers/json" && r.Method == "GET":
		e.list(w, r)
	case path == "/containers/create" && r.Method == "POST":
		e.create(w, r)
	case path == "/events":
		e.streamEvents(w, r)
	case path == "/volumes" && r.Method == "GET":
		e.mu.Lock()
		volumes := []docker.VolumeInspect{}
		for _, info := range e.volumes {
			volumes = append(volumes, info)
		}
		e.mu.Unlock()
		writeJSON(w, 200, map[string]any{"Volumes": volumes})
	case path == "/volumes/create" && r.Method == "POST":
		var body struct{ Name string }
		json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, 201, e.addVolume(body.Name))
	case path == "/images/create" && r.Method == "POST":
		reference := r.URL.Query().Get("fromImage")
		e.mu.Lock()
		hook := e.OnPull
		e.mu.Unlock()
		if hook != nil {
			hook(reference)
		}
		e.mu.Lock()
		pulled, ok := e.registry[reference]
		if !ok {
			pulled = "sha256:" + strings.Repeat("1", 64)
		}
		e.images[reference] = pulled
		e.mu.Unlock()
		w.Write([]byte(`{"status":"Pulled"}` + "\n"))
	case container.MatchString(path):
		parts := container.FindStringSubmatch(path)
		e.containerRequest(w, r, parts[1], parts[2])
	case execPath.MatchString(path):
		parts := execPath.FindStringSubmatch(path)
		e.execRequest(w, r, parts[1], parts[2])
	case volume.MatchString(path):
		name := volume.FindStringSubmatch(path)[1]
		e.mu.Lock()
		info, ok := e.volumes[name]
		if ok && r.Method == "DELETE" {
			delete(e.volumes, name)
		}
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else if r.Method == "DELETE" {
			w.WriteHeader(204)
		} else {
			writeJSON(w, 200, info)
		}
	case image.MatchString(path):
		reference := image.FindStringSubmatch(path)[1]
		e.mu.Lock()
		id, ok := e.images[reference]
		// Every image ID a container reports exists.
		if !ok && strings.HasPrefix(reference, "sha256:") {
			id, ok = reference, true
		}
		config := e.imageConfigs[id]
		if variant, ok := e.imageConfigs[id+" "+r.URL.Query().Get("platform")]; ok {
			config = variant
		}
		repoDigests := e.repoDigestsLocked(id)
		e.mu.Unlock()
		if !ok {
			fail(w, 404)
		} else {
			writeJSON(w, 200, map[string]any{"Id": id, "Config": config, "RepoDigests": repoDigests})
		}
	case distribution.MatchString(path):
		reference := distribution.FindStringSubmatch(path)[1]
		e.mu.Lock()
		e.RegistryAuth = append(e.RegistryAuth, r.Header.Get("X-Registry-Auth"))
		status, id := e.registryFail[reference], e.tagLocked(reference)
		e.mu.Unlock()
		if status == 0 && id == "" {
			// Docker answers 401 both for refused credentials and unknown tags.
			status = 401
		}
		if status != 0 {
			fail(w, status)
			return
		}
		writeJSON(w, 200, map[string]any{
			"Descriptor": map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": ManifestDigest(id)},
			"Platforms":  []docker.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}})
	default:
		fail(w, 404)
	}
}

// Frame encodes one multiplexed stream frame.
func Frame(channel byte, data []byte) []byte {
	frame := make([]byte, 8+len(data))
	frame[0] = channel
	binary.BigEndian.PutUint32(frame[4:], uint32(len(data)))
	copy(frame[8:], data)
	return frame
}
