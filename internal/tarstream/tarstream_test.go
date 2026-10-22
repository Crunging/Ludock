package tarstream

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

var long = "snapshot/root-0/" + strings.Repeat("ü", 80) + "/deep.txt"

// sample encodes a directory, two files (one with base-256 IDs and a
// fractional mtime), and a long Unicode path that needs PAX records.
func sample(t *testing.T) []byte {
	t.Helper()
	var archive bytes.Buffer
	for _, entry := range []struct {
		header Header
		body   string
	}{
		{Header{Name: "snapshot/", Type: "directory", Mode: 0o755, MtimeSeconds: 1700000000}, ""},
		{Header{Name: "snapshot/root-0/", Type: "directory", Mode: 0o755, MtimeSeconds: 1700000000}, ""},
		{Header{Name: "snapshot/root-0/world.dat", Type: "file", Mode: 0o644, UID: 4294967294, GID: 3000000000,
			MtimeSeconds: 1700000000.5}, "world"},
		{Header{Name: long, Type: "file", Mode: 0o600, MtimeSeconds: 4102444800}, "abc"},
	} {
		entry.header.Size = int64(len(entry.body))
		encoded, err := EncodeHeader(entry.header)
		if err != nil {
			t.Fatal(err)
		}
		archive.Write(encoded)
		archive.WriteString(entry.body)
		archive.Write(Padding(entry.header.Size))
	}
	archive.Write(End())
	return archive.Bytes()
}

func TestRoundTripsArchives(t *testing.T) {
	archive := sample(t)
	var rebuilt bytes.Buffer
	var headers []*Header
	bodies := map[string]string{}
	err := Walk(bytes.NewReader(archive), func(header *Header, body io.Reader) error {
		headers = append(headers, header)
		data, _ := io.ReadAll(body)
		bodies[header.Name] = string(data)
		encoded, err := EncodeHeader(*header)
		if err != nil {
			return err
		}
		rebuilt.Write(encoded)
		rebuilt.Write(data)
		rebuilt.Write(Padding(int64(len(data))))
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt.Write(End())
	if len(headers) != 4 || headers[2].UID != 4294967294 || headers[2].GID != 3000000000 || headers[2].MtimeSeconds != 1700000000.5 {
		t.Fatalf("%+v", headers[2])
	}
	if bodies[long] != "abc" || headers[3].MtimeSeconds != 4102444800 {
		t.Fatal(bodies)
	}
	if !bytes.Equal(rebuilt.Bytes(), archive) {
		t.Fatal("re-encoded archive differs")
	}
	// Standard readers agree.
	reader := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		if header.Name == "snapshot/root-0/world.dat" && (header.Uid != 4294967294 || header.Gid != 3000000000) {
			t.Fatalf("%+v", header)
		}
	}
	if len(names) != 4 || names[3] != long {
		t.Fatal(names)
	}
}

func TestRejectsMalformedArchives(t *testing.T) {
	archive := sample(t)
	walk := func(data []byte) error {
		return Walk(bytes.NewReader(data), func(*Header, io.Reader) error { return nil }, nil)
	}
	corrupt := bytes.Clone(archive)
	corrupt[600] ^= 1
	truncated := archive[:len(archive)-1500]
	trailing := append(bytes.Clone(archive), 'x')
	for name, data := range map[string][]byte{"checksum": corrupt, "truncated": truncated, "trailing": trailing, "single-zero": make([]byte, 512)} {
		if err := walk(data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Visitors see types; a symlink entry parses so callers can reject it.
	var kinds []string
	Walk(bytes.NewReader(archive), func(h *Header, _ io.Reader) error { kinds = append(kinds, h.Type); return nil }, nil)
	if kinds[0] != "directory" || kinds[2] != "file" {
		t.Fatal(kinds)
	}
	var seen int
	Walk(bytes.NewReader(archive), func(*Header, io.Reader) error { return nil }, func(chunk []byte) error { seen += len(chunk); return nil })
	if seen != len(archive) {
		t.Fatal("onChunk missed bytes", seen)
	}
}
