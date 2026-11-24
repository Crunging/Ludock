package backups

import (
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/files"
)

// A backup restores as usual onto the binding it was taken from, needs a
// confirmation when only the host storage behind the same folders changed,
// and is refused for another game or other folders, saying which.
func TestCompareFit(t *testing.T) {
	data, world := Root{ID: files.RootID("/data"), Path: "/data"}, Root{ID: files.RootID("/world"), Path: "/world"}
	config := Root{ID: files.RootID("/config"), Path: "/config"}
	backup := &Backup{Roots: []Root{data, world}, GameType: "minecraft", fingerprint: "taken"}
	for _, test := range []struct {
		name, fingerprint, game string
		roots                   []Root
		moved                   bool
		refusal                 string
	}{
		{"same binding", "taken", "minecraft", []Root{world, data}, false, ""},
		{"same paths, other sources", "now", "minecraft", []Root{data, world}, true, ""},
		{"a folder missing", "now", "minecraft", []Root{data}, false, "This backup has the folder /world, which the server no longer has."},
		{"an extra folder", "now", "minecraft", []Root{data, world, config}, false, "The server now has the folder /config, which this backup doesn’t."},
		{"another game", "now", "valheim", []Root{data, world}, false, "This backup holds Minecraft data, but the server now runs Valheim."},
		{"a forged root ID", "now", "minecraft", []Root{data, {ID: world.ID, Path: "/elsewhere"}}, false,
			"This backup has the folder /world, which the server no longer has. The server now has the folder /elsewhere, which this backup doesn’t."},
	} {
		fit := CompareFit(backup, test.fingerprint, test.game, test.roots)
		if fit.Moved != test.moved || fit.Refusal != test.refusal {
			t.Errorf("%s: %+v", test.name, fit)
		}
	}
	bedrock := &Backup{Roots: []Root{data}, GameType: "bedrock"}
	for _, game := range []string{"minecraft-bedrock", "Minecraft: Bedrock Edition"} {
		if fit := CompareFit(bedrock, "now", game, []Root{data}); !fit.Moved || fit.Refusal != "" {
			t.Errorf("bedrock as %s: %+v", game, fit)
		}
	}
	unknown := &Backup{Roots: []Root{data}, fingerprint: "taken"}
	if fit := CompareFit(unknown, "taken", "minecraft", []Root{data}); fit != (Fit{}) {
		t.Errorf("same binding without a recorded game: %+v", fit)
	}
	if fit := CompareFit(unknown, "now", "minecraft", []Root{data}); fit.Moved || !strings.Contains(fit.Refusal, "can’t tell whether this backup and the server are the same game") {
		t.Errorf("moved without a recorded game: %+v", fit)
	}
	// Every unrecognized image is "unknown", so that proves no game the same.
	unknown.GameType = "Unknown"
	if fit := CompareFit(unknown, "now", "unknown", []Root{data}); fit.Moved || fit.Refusal == "" {
		t.Errorf("moved between unrecognized games: %+v", fit)
	}
}
