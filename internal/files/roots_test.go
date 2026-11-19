package files

import (
	"reflect"
	"testing"

	"github.com/Crunging/Ludock/internal/identity"
)

func TestRootsOfferOnlySafeDataMounts(t *testing.T) {
	got := Roots("custom", map[string]string{}, []identity.Mount{
		{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true},
		{Type: "volume", Source: "nested", Destination: "/data/config", Writable: true},
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"},
		{Type: "bind", Source: "/", Destination: "/host", Writable: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", Writable: true},
		{Type: "tmpfs", Destination: "/cache", Writable: true},
	})
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
	})
	if !reflect.DeepEqual(got, []Root{{RootID("/data"), "data", "/data"}}) {
		t.Fatal(got)
	}
	got = Roots("custom", map[string]string{"ludock.files": "/data/world"}, []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}})
	if !reflect.DeepEqual(got, []Root{{RootID("/data/world"), "world", "/data/world"}}) {
		t.Fatal(got)
	}
	if got := Roots("custom", map[string]string{"ludock.files": "/data/../etc"}, []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}}); got != nil {
		t.Fatal(got)
	}
	if Roots("custom", nil, nil) != nil {
		t.Fatal("roots without mounts")
	}
	SetSensitivePaths([]string{"/srv/game", "/other"})
	t.Cleanup(func() { SetSensitivePaths(nil) })
	if got := Roots("custom", nil, []identity.Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", Writable: true}}); got != nil {
		t.Fatal("configured sensitive source offered", got)
	}
}

// A root keeps its ID when Docker or the label lists the roots in another
// order, so a page opened before the change still names the same folder.
func TestRootIDsFollowPaths(t *testing.T) {
	mounts := []identity.Mount{{Type: "bind", Source: "/srv/a", Destination: "/a", Writable: true},
		{Type: "bind", Source: "/srv/b", Destination: "/b", Writable: true}}
	first := Roots("custom", nil, mounts)
	second := Roots("custom", nil, []identity.Mount{mounts[1], mounts[0]})
	if len(first) != 2 || first[0].ID == first[1].ID || first[0] != second[1] || first[1] != second[0] {
		t.Fatal(first, second)
	}
	if relabeled := Roots("custom", map[string]string{"ludock.files": "/b"}, mounts); relabeled[0].ID != first[1].ID {
		t.Fatal(relabeled)
	}
}
