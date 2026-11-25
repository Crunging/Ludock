package files

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
)

// Forged selections name entries outside their folder, repeat one, or are
// too large; forged moves target the same folder or a selected folder;
// forged new folders sit in a parent that leaves and re-enters its folder,
// or is longer than any path. Each is refused before any helper starts.
func TestSelectionRefusesForgedRequests(t *testing.T) {
	target := Target{Roots: []Root{{ID: RootID("/data"), Name: "data", Path: "/data"}}}
	root := target.Roots[0].ID
	many := make([]string, MaxSelection+1)
	for index := range many {
		many[index] = "file-" + strconv.Itoa(index)
	}
	for _, test := range []struct {
		folder string
		names  []string
		code   string
	}{
		{"logs", []string{"../server.properties"}, "INVALID_NAME"},
		{"logs", []string{"a/b"}, "INVALID_NAME"},
		{"logs", []string{".."}, "INVALID_NAME"},
		{"../etc", []string{"passwd"}, "INVALID_PATH"},
		{"logs", []string{"a", "a"}, "INVALID_SELECTION"},
		{"logs", nil, "INVALID_SELECTION"},
		{"logs", many, "INVALID_SELECTION"},
	} {
		if _, _, _, err := selection(target, root, test.folder, test.names); !apperr.Is(err, test.code) {
			t.Errorf("%s %v: %v", test.folder, len(test.names), err)
		}
	}
	if _, _, _, err := selection(target, "root-0", "", []string{"a"}); !apperr.Is(err, "ROOT_NOT_FOUND") {
		t.Fatal(err)
	}
	service := &Service{}
	for destination, code := range map[string]string{"logs": "MOVE_SAME_FOLDER", "logs/old": "MOVE_INTO_ITSELF",
		"logs/old/deeper": "MOVE_INTO_ITSELF", "/etc": "INVALID_PATH", "../x": "INVALID_PATH"} {
		if err := service.Move(ctx, target, root, "logs", []string{"old"}, destination, nil); !apperr.Is(err, code) {
			t.Errorf("%s: %v", destination, err)
		}
	}
	for _, parent := range []string{"logs/..", strings.Repeat("a/", MaxPath)} {
		if err := service.Mkdir(ctx, target, root, parent, "x", nil); !apperr.Is(err, "INVALID_PATH") {
			t.Errorf("%.20s: %v", parent, err)
		}
	}
	// The path each change uses, composed from its parts, is at most MaxPath
	// bytes; one at the bound gets as far as Docker, and one only spelled
	// longer is fine.
	folder := strings.Repeat("d", 200) + strings.Repeat("/"+strings.Repeat("d", 200), 19)
	for _, test := range []struct {
		name   string
		change func(name string) error
	}{
		{"mkdir", func(name string) error { return service.Mkdir(ctx, target, root, folder, name, nil) }},
		{"rename", func(name string) error { return service.Rename(ctx, target, root, folder+"/old", name, nil) }},
		{"selection", func(name string) error {
			return service.DeleteSelection(ctx, target, root, folder, []string{name}, nil)
		}},
		{"move", func(name string) error { return service.Move(ctx, target, root, "logs", []string{name}, folder, nil) }},
		{"upload", func(name string) error {
			results, err := service.UploadFiles(ctx, target, root, folder, "", []UploadEntry{{Name: name}}, strings.NewReader(""), nil)
			return errors.Join(err, results[0])
		}},
	} {
		fits := strings.Repeat("n", MaxPath-len(folder)-1)
		if err := test.change(fits); apperr.Is(err, "INVALID_PATH") {
			t.Error(test.name, "refused a path at the bound")
		}
		if err := test.change(fits + "n"); !apperr.Is(err, "INVALID_PATH") {
			t.Error(test.name, "allowed a path past the bound:", err)
		}
	}
	if err := service.Mkdir(ctx, target, root, strings.Repeat("./", MaxPath)+"logs", "x", nil); apperr.Is(err, "INVALID_PATH") {
		t.Error("refused a path only spelled long")
	}
}
