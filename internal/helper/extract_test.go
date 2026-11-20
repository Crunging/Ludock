package helper

import (
	"encoding/json"
	"math"
	"testing"
)

// Ludock once sent restore-extract's request and records as maps. The typed
// ones encode to the same bytes, from the smallest values to the largest the
// extractor takes, and with names JSON escapes.
func TestExtractEncodingMatchesMaps(t *testing.T) {
	for _, r := range []struct {
		name, kind     string
		size, uid, gid int64
		mode           int64
		mtime          float64
	}{
		{"world/level.dat", "file", 8, 1000, 1000, 0o644, 1700000000.5},
		{"world", "directory", 0, 0, 0, 0o755, 0},
		{`a <b> & "c"/ü\x`, "file", 1<<53 - 1, 4294967294, 4294967294, 1<<53 - 1, 8.64e12},
		{"x", "file", 0, 0, 0, -(1<<53 - 1), -1.25},
		{"y", "file", 1, 1, 1, -1, -8.64e12},
	} {
		old, _ := json.Marshal(map[string]any{"name": r.name, "type": r.kind, "mode": r.mode,
			"uid": r.uid, "gid": r.gid, "mtime": r.mtime, "size": r.size})
		typed, _ := json.Marshal(ExtractRecord{GID: &r.gid, Mode: &r.mode, Mtime: &r.mtime, Name: r.name, Size: &r.size,
			Type: r.kind, UID: &r.uid})
		if string(old) != string(typed) {
			t.Fatalf("record\n%s\n%s", old, typed)
		}
	}
	for _, r := range []struct {
		max, reserve int64
		root         string
	}{{0, 0, "/data"}, {1 << 62, 1<<53 - 1, "/srv/a <b> & ü"}, {-1, 1 << 20, "/"}} {
		stage := StageName("0f9619ff-8b86-4011-b42d-00c04fc964ff")
		old, _ := json.Marshal(map[string]any{"root": r.root, "stage": stage, "maxBytes": r.max, "reserveBytes": r.reserve})
		typed, _ := json.Marshal(ExtractRequest{MaxBytes: r.max, ReserveBytes: &r.reserve, Root: r.root, Stage: stage})
		if string(old) != string(typed) {
			t.Fatalf("request\n%s\n%s", old, typed)
		}
	}
}

// The extractor accepted record numbers through float64 pointers. The typed
// record accepts and rejects the same lines, with the same values, except
// integers spelled as decimals or exponents, which Ludock never writes and
// the typed record now rejects.
func TestExtractRecordChecksMatchFloats(t *testing.T) {
	type floats struct {
		Name  string   `json:"name"`
		Type  string   `json:"type"`
		Size  *float64 `json:"size"`
		UID   *float64 `json:"uid"`
		GID   *float64 `json:"gid"`
		Mode  *float64 `json:"mode"`
		Mtime *float64 `json:"mtime"`
	}
	safe := func(value *float64, low, high float64) (int64, bool) {
		if value == nil || *value != math.Trunc(*value) || *value < low || *value > high || math.Abs(*value) > 1<<53-1 {
			return 0, false
		}
		return int64(*value), true
	}
	before := func(line string) (values [4]int64, ok bool) {
		var r floats
		if json.Unmarshal([]byte(line), &r) != nil {
			return values, false
		}
		size, okSize := safe(r.Size, 0, 1<<53-1)
		uid, okUID := safe(r.UID, 0, 4294967294)
		gid, okGID := safe(r.GID, 0, 4294967294)
		mode, okMode := safe(r.Mode, -(1<<53 - 1), 1<<53-1)
		ok = (r.Type == "file" || r.Type == "directory") && okSize && okUID && okGID && okMode
		return [4]int64{size, uid, gid, mode}, ok
	}
	after := func(line string) (values [4]int64, ok bool) {
		var r ExtractRecord
		if json.Unmarshal([]byte(line), &r) != nil {
			return values, false
		}
		size, uid, gid, mode, ok := r.checked()
		return [4]int64{size, uid, gid, mode}, ok
	}
	base := `"name":"w","type":"file","uid":1,"gid":2,"mode":420,"mtime":1700000000.5`
	lines := []string{
		`{` + base + `,"size":8}`,
		`{` + base + `,"size":0}`,
		`{` + base + `,"size":-0}`,
		`{` + base + `,"size":9007199254740991}`,
		`{` + base + `,"size":9007199254740992}`,
		`{` + base + `,"size":9223372036854775807}`,
		`{` + base + `,"size":9223372036854775808}`,
		`{` + base + `,"size":-1}`,
		`{` + base + `,"size":1.5}`,
		`{` + base + `,"size":1e400}`,
		`{` + base + `,"size":"8"}`,
		`{` + base + `,"size":null}`,
		`{` + base + `}`,
		`{` + base + `,"size":1,"size":null}`,
		`{"name":"w","type":"file","size":1,"uid":4294967294,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":4294967295,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":-1,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":0,"gid":4294967295,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":0,"mode":0}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":-9007199254740991,"mtime":null}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":-9007199254740992}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":9007199254740992}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0}`,
		`{"name":"w","type":"symlink","size":0,"uid":0,"gid":0,"mode":0}`,
		`{"name":"w","size":0,"uid":0,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":0,"uid":0,"gid":0,"mode":0,"mtime":"x"}`,
		`[]`,
		`null`,
	}
	for _, line := range lines {
		old, oldOK := before(line)
		typed, typedOK := after(line)
		if oldOK != typedOK || oldOK && old != typed {
			t.Errorf("%s: %v %v, now %v %v", line, old, oldOK, typed, typedOK)
		}
	}
	for _, line := range []string{`{` + base + `,"size":8.0}`, `{` + base + `,"size":8e0}`} {
		if _, ok := before(line); !ok {
			t.Fatal("floats rejected", line)
		}
		if _, ok := after(line); ok {
			t.Fatal("accepted a decimal integer", line)
		}
	}
}

// The request's reserve was a float64 pointer too: missing, null, negative
// or past 2^53 fails the same way, and a missing maxBytes is still 0.
func TestExtractRequestReserveMatchesFloats(t *testing.T) {
	stage := `"root":"/data","stage":"` + StageName("0f9619ff-8b86-4011-b42d-00c04fc964ff") + `"`
	for line, want := range map[string]bool{
		`{` + stage + `,"maxBytes":100,"reserveBytes":0}`:                true,
		`{` + stage + `,"reserveBytes":9007199254740991}`:                true,
		`{` + stage + `,"maxBytes":100,"reserveBytes":9007199254740992}`: false,
		`{` + stage + `,"maxBytes":100,"reserveBytes":-1}`:               false,
		`{` + stage + `,"maxBytes":100,"reserveBytes":null}`:             false,
		`{` + stage + `,"maxBytes":100}`:                                 false,
	} {
		var request ExtractRequest
		err := json.Unmarshal([]byte(line), &request)
		if _, ok := inRange(request.ReserveBytes, 0, 1<<53-1); err != nil || ok != want || !validStage(request.Stage) {
			t.Error(line, ok, err)
		}
	}
}
