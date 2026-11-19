package files

import (
	"strconv"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
)

// Forged selections name entries outside their folder, repeat one, or are
// too large; forged moves target the same folder or a selected folder.
// Each is refused before any helper starts.
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
}
