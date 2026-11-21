package helper

import (
	"encoding/json"
	"testing"
)

// Restore-extract's request and records are the bytes Ludock sends its
// helper: names keep JSON's escaping, and a record is accepted only with
// whole numbers in range and a file or directory type, so a value past 2^53
// can't be rounded into another.
func TestExtractWireFormat(t *testing.T) {
	gid, uid, size, mode, mtime := int64(1000), int64(1000), int64(8), int64(0o644), 1700000000.5
	record, _ := json.Marshal(ExtractRecord{GID: &gid, Mode: &mode, Mtime: &mtime, Name: `a <b> & "c"/ü\x`, Size: &size, Type: "file", UID: &uid})
	if want := `{"gid":1000,"mode":420,"mtime":1700000000.5,"name":"a \u003cb\u003e \u0026 \"c\"/ü\\x","size":8,"type":"file","uid":1000}`; string(record) != want {
		t.Fatalf("record\n%s\n%s", record, want)
	}
	reserve := int64(1 << 20)
	request, _ := json.Marshal(ExtractRequest{MaxBytes: 100, ReserveBytes: &reserve, Root: "/srv/a <b> & ü", Stage: StageName("0f9619ff-8b86-4011-b42d-00c04fc964ff")})
	if want := `{"maxBytes":100,"reserveBytes":1048576,"root":"/srv/a \u003cb\u003e \u0026 ü","stage":".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"}`; string(request) != want {
		t.Fatalf("request\n%s\n%s", request, want)
	}

	zero, _ := json.Marshal(ExtractRequest{ReserveBytes: new(int64), Root: "/", Stage: StageName("0f9619ff-8b86-4011-b42d-00c04fc964ff")})
	if want := `{"maxBytes":0,"reserveBytes":0,"root":"/","stage":".ludock-restore-0f9619ff-8b86-4011-b42d-00c04fc964ff"}`; string(zero) != want {
		t.Fatalf("request\n%s\n%s", zero, want)
	}

	base := `"name":"w","type":"file","uid":1,"gid":2,"mode":420,"mtime":1700000000.5`
	for line, want := range map[string][4]int64{
		`{` + base + `,"size":8}`:                                               {8, 1, 2, 420},
		`{` + base + `,"size":-0}`:                                              {0, 1, 2, 420},
		`{` + base + `,"size":9007199254740991}`:                                {1<<53 - 1, 1, 2, 420},
		`{"name":"w","type":"file","size":1,"uid":4294967294,"gid":0,"mode":0}`: {1, 4294967294, 0, 0},
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":-9007199254740991,"mtime":null}`: {0, 0, 0, -(1<<53 - 1)},
	} {
		var r ExtractRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(line, err)
		}
		if size, uid, gid, mode, ok := r.checked(); !ok || [4]int64{size, uid, gid, mode} != want {
			t.Errorf("%s: %v %v %v %v %v", line, size, uid, gid, mode, ok)
		}
	}
	for _, line := range []string{
		`{` + base + `,"size":9007199254740992}`, `{` + base + `,"size":9223372036854775808}`, `{` + base + `,"size":-1}`,
		`{` + base + `,"size":1.5}`, `{` + base + `,"size":8.0}`, `{` + base + `,"size":8e0}`, `{` + base + `,"size":1e400}`,
		`{` + base + `,"size":"8"}`, `{` + base + `,"size":null}`, `{` + base + `}`, `{` + base + `,"size":1,"size":null}`,
		`{"name":"w","type":"file","size":1,"uid":4294967295,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":-1,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":0,"gid":4294967295,"mode":0}`,
		`{"name":"w","type":"file","size":1,"uid":0,"mode":0}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":-9007199254740992}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0,"mode":9007199254740992}`,
		`{"name":"w","type":"directory","size":0,"uid":0,"gid":0}`,
		`{"name":"w","type":"symlink","size":0,"uid":0,"gid":0,"mode":0}`,
		`{"name":"w","size":0,"uid":0,"gid":0,"mode":0}`,
		`{"name":"w","type":"file","size":0,"uid":0,"gid":0,"mode":0,"mtime":"x"}`,
		`[]`, `null`,
	} {
		var r ExtractRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			if _, _, _, _, ok := r.checked(); ok {
				t.Errorf("accepted %s", line)
			}
		}
	}

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
