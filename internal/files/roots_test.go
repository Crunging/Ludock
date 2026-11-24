package files

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/self"
)

var notInContainer = self.Info{Answer: self.NotFound}

func TestRootsOfferOnlySafeDataMounts(t *testing.T) {
	got := Roots("custom", map[string]string{}, []identity.Mount{
		{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true},
		{Type: "volume", Source: "nested", Destination: "/data/config", Writable: true},
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"},
		{Type: "bind", Source: "/", Destination: "/host", Writable: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", Writable: true},
		{Type: "tmpfs", Destination: "/cache", Writable: true},
	}, notInContainer)
	if !reflect.DeepEqual(got, []Root{{RootID("/data"), "data", "/data"}}) {
		t.Fatal(got)
	}
}

func TestLabelsCannotOpenUnsafeRoots(t *testing.T) {
	labels := map[string]string{"ludock.files": "/data,/etc,/secret,/socket,/arbitrary,/data/private"}
	got := Roots("custom", labels, []identity.Mount{
		{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true},
		{Type: "bind", Source: "/etc", Destination: "/etc", Writable: true},
		{Type: "bind", Source: "/srv/read-only", Destination: "/secret"},
		{Type: "bind", Source: "/srv/docker.sock", Destination: "/socket", Writable: true},
		{Type: "bind", Source: "/srv/private", Destination: "/data/private"},
	}, notInContainer)
	if !reflect.DeepEqual(got, []Root{{RootID("/data"), "data", "/data"}}) {
		t.Fatal(got)
	}
	got = Roots("custom", map[string]string{"ludock.files": "/data/world"}, []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}}, notInContainer)
	if !reflect.DeepEqual(got, []Root{{RootID("/data/world"), "world", "/data/world"}}) {
		t.Fatal(got)
	}
	if got := Roots("custom", map[string]string{"ludock.files": "/data/../etc"}, []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}}, notInContainer); got != nil {
		t.Fatal(got)
	}
	if Roots("custom", nil, nil, notInContainer) != nil {
		t.Fatal("roots without mounts")
	}
	// When Ludock can't read its own mounts, any folder could hold its data.
	if got := Roots("custom", nil, []identity.Mount{{Type: "bind", Source: "/srv/other", Destination: "/data", Writable: true}}, self.Info{Answer: self.Refused}); got != nil {
		t.Fatal("offered a folder while Ludock's own mounts are unknown", got)
	}
	game := []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}}
	// The host's root mounted into Ludock overlaps every game folder, and
	// every volume, whatever source Docker reports for it.
	hostRoot := self.Info{Answer: self.Found, Mounts: []identity.Mount{{Type: "bind", Source: "/", Destination: "/host"}}}
	volume := identity.Mount{Type: "volume", Name: "world", Source: "world", Destination: "/world", Writable: true}
	if got := Roots("custom", nil, append(game, volume), hostRoot); got != nil {
		t.Fatal("offered a folder inside Ludock's own mount of /", got)
	}
	for _, sensitive := range [][]string{{"/srv/game", "/other"}, {"/"}} {
		SetSensitivePaths(sensitive)
		t.Cleanup(func() { SetSensitivePaths(nil) })
		if got := Roots("custom", nil, game, notInContainer); got != nil {
			t.Fatal("configured sensitive source offered", sensitive, got)
		}
	}
}

// A root keeps its ID when Docker or the label lists the roots in another
// order, so a page opened before the change still names the same folder.
func TestRootIDsFollowPaths(t *testing.T) {
	mounts := []identity.Mount{{Type: "bind", Source: "/srv/a", Destination: "/a", Writable: true},
		{Type: "bind", Source: "/srv/b", Destination: "/b", Writable: true}}
	first := Roots("custom", nil, mounts, notInContainer)
	second := Roots("custom", nil, []identity.Mount{mounts[1], mounts[0]}, notInContainer)
	if len(first) != 2 || first[0].ID == first[1].ID || first[0] != second[1] || first[1] != second[0] {
		t.Fatal(first, second)
	}
	if relabeled := Roots("custom", map[string]string{"ludock.files": "/b"}, mounts, notInContainer); relabeled[0].ID != first[1].ID {
		t.Fatal(relabeled)
	}
}

// lloesche/valheim-server's README mounts the server it downloads at
// /opt/valheim beside the worlds in /config: 4 GB that every backup copied.
// escaping/core-keeper-dedicated's does the same with a 570 MB download
// beside a world of 100 KB. Files and backups take only the game's data
// unless a label names the download.
func TestRootsLeaveOutServerFiles(t *testing.T) {
	for _, game := range []struct{ gameType, data, download, label string }{
		{"valheim", "/config", "/opt/valheim/", "Valheim config"},
		{"core-keeper", "/home/steam/core-keeper-data", "/home/steam/core-keeper-dedicated", "core-keeper-data"},
	} {
		mounts := []identity.Mount{
			{Type: "bind", Source: "/srv/game/data", Destination: game.data, Writable: true},
			{Type: "volume", Source: "server", Destination: game.download, Writable: true},
		}
		if got := Roots(game.gameType, map[string]string{}, mounts, notInContainer); !reflect.DeepEqual(got, []Root{{RootID(game.data), game.label, game.data}}) {
			t.Fatal(game.gameType, got)
		}
		if got := Roots("custom", map[string]string{}, mounts, notInContainer); len(got) != 2 {
			t.Fatal(got)
		}
		labels := map[string]string{"ludock.files": game.data + "," + game.download}
		if got := Roots(game.gameType, labels, mounts, notInContainer); len(got) != 2 || got[1].Path != strings.TrimSuffix(game.download, "/") {
			t.Fatal(game.gameType, got)
		}
	}
}

// thijsvanloef/palworld-server-docker mounts one folder at /palworld, where
// it downloads the 3.5 GB server and the game keeps its world in Pal/Saved.
// Files and backups take only the saves unless a label names the mount.
func TestRootsTakeSavesInsideServerFiles(t *testing.T) {
	mounts := []identity.Mount{{Type: "volume", Source: "palworld", Destination: "/palworld", Writable: true}}
	want := []Root{{RootID("/palworld/Pal/Saved"), "Palworld saves", "/palworld/Pal/Saved"}}
	if got := Roots("palworld", map[string]string{}, mounts, notInContainer); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := Roots("palworld", map[string]string{"ludock.files": "/palworld"}, mounts, notInContainer); len(got) != 1 || got[0].Path != "/palworld" {
		t.Fatal(got)
	}
	// A save folder mounted on its own is that mount, as for any game.
	saved := []identity.Mount{{Type: "bind", Source: "/srv/pal/saved", Destination: "/palworld/Pal/Saved", Writable: true}}
	if got := Roots("palworld", map[string]string{}, saved, notInContainer); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}
