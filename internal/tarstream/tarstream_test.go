package tarstream

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The fixture was written by v0.5.0's encoder (testdata/generate.ts.txt).
func TestReadsAndReproducesV050Archives(t *testing.T) {
	archive, err := os.ReadFile("testdata/v050-backup.tar")
	if err != nil {
		t.Fatal(err)
	}
	var rebuilt bytes.Buffer
	var headers []*Header
	bodies := map[string]string{}
	err = Walk(bytes.NewReader(archive), func(header *Header, body io.Reader) error {
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
	long := "snapshot/root-0/" + strings.Repeat("ü", 80) + "/deep.txt"
	if bodies[long] != "abc" || headers[3].MtimeSeconds != 4102444800 {
		t.Fatal(bodies)
	}
	if !bytes.Equal(rebuilt.Bytes(), archive) {
		t.Fatal("re-encoded archive differs from v0.5.0 bytes")
	}
	// Standard readers agree.
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(header.Name, "deep.txt") && header.Name != long {
			t.Fatal(header.Name)
		}
	}
}

func TestRejectsMalformedArchives(t *testing.T) {
	archive, _ := os.ReadFile("testdata/v050-backup.tar")
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
