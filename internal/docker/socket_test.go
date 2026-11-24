package docker

import (
	"testing"

	"github.com/Crunging/Ludock/internal/mountinfo"
)

// Captured from Ludock's container: the socket as Docker mounted it, and
// after a daemon with live restore and its own socket restarted.
func TestSocketReplacedMountinfo(t *testing.T) {
	const root = "2289 1264 0:103 / / ro,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n"
	mounted := root + "2305 2289 0:92 /docker.sock /run/docker.sock ro,nosuid,nodev - tmpfs tmpfs rw,size=3102748k,mode=755\n"
	replaced := root + "2376 2344 0:111 /run/docker.sock//deleted /run/docker.sock ro,relatime - overlay overlay rw,lowerdir=/l\n"
	for name, c := range map[string]struct {
		table string
		want  bool
	}{
		"mounted": {mounted, false}, "replaced": {replaced, true}, "not a mount": {root, false},
		"mounted again over it": {replaced + "2400 2289 0:92 /docker.sock /run/docker.sock ro - tmpfs tmpfs rw\n", false},
	} {
		if got := mountDeleted(mountinfo.Parse(c.table), "/run/docker.sock"); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
