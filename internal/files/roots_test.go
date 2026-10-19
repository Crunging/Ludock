package files

import (
	"reflect"
	"testing"
)

func TestRootsOfferOnlySafeDataMounts(t *testing.T) {
	got := Roots("custom", map[string]string{}, []Mount{
		{Type: "bind", Source: "/srv/game", Destination: "/data", RW: true},
		{Type: "volume", Source: "nested", Destination: "/data/config", RW: true},
		{Type: "bind", Source: "/etc/localtime", Destination: "/etc/localtime"},
		{Type: "bind", Source: "/", Destination: "/host", RW: true},
		{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", RW: true},
		{Type: "tmpfs", Destination: "/cache", RW: true},
	})
	if !reflect.DeepEqual(got, []Root{{"root-0", "data", "/data"}}) {
		t.Fatal(got)
	}
}

func TestLabelsCannotOpenUnsafeRoots(t *testing.T) {
	labels := map[string]string{"ludock.files": "/data,/etc,/secret,/socket,/arbitrary,/data/private"}
	got := Roots("custom", labels, []Mount{
		{Type: "bind", Source: "/srv/game", Destination: "/data", RW: true},
		{Type: "bind", Source: "/etc", Destination: "/etc", RW: true},
		{Type: "bind", Source: "/srv/read-only", Destination: "/secret"},
		{Type: "bind", Source: "/srv/docker.sock", Destination: "/socket", RW: true},
		{Type: "bind", Source: "/srv/private", Destination: "/data/private"},
	})
	if !reflect.DeepEqual(got, []Root{{"root-0", "data", "/data"}}) {
		t.Fatal(got)
	}
	got = Roots("custom", map[string]string{"ludock.files": "/data/world"}, []Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", RW: true}})
	if !reflect.DeepEqual(got, []Root{{"root-0", "world", "/data/world"}}) {
		t.Fatal(got)
	}
	if got := Roots("custom", map[string]string{"ludock.files": "/data/../etc"}, []Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", RW: true}}); got != nil {
		t.Fatal(got)
	}
	if Roots("custom", nil, nil) != nil {
		t.Fatal("roots without mounts")
	}
	t.Setenv("LUDOCK_SENSITIVE_PATHS", "/srv/game:/other")
	if got := Roots("custom", nil, []Mount{{Type: "bind", Source: "/srv/game", Destination: "/data", RW: true}}); got != nil {
		t.Fatal("configured sensitive source offered", got)
	}
}
