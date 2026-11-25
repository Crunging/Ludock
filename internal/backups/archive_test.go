package backups

import (
	"io"
	"os"
	"testing"

	"github.com/Crunging/Ludock/internal/apperr"
	"github.com/Crunging/Ludock/internal/tarstream"
)

// testdata/acl holds the same folder archived with --acls by GNU tar 1.35,
// with and without --numeric-owner, and by bsdtar 3.8.5, whose named
// entries also carry their IDs. Ludock reads each ACL into the form it
// stores, except a named entry GNU tar gave only a name.
func TestEntryACLsFromGNUTarAndBSDTar(t *testing.T) {
	file := "user::rw-,user:0:rw-,user:4242:rw-,group::r--,group:0:r--,group:4343:r--,mask::rw-,other::---"
	folder := [2]string{"user::rwx,user:4242:rwx,group::rwx,mask::rwx,other::r-x",
		"user::rwx,user:4242:rwx,group::rwx,group:4343:r-x,mask::rwx,other::r-x"}
	for archive, want := range map[string]map[string][2]string{
		"gnu.tar":         {"world/": folder, "world/level.dat": {"refused"}},
		"gnu-numeric.tar": {"world/": folder, "world/level.dat": {file}},
		"bsdtar.tar":      {"world/": folder, "world/level.dat": {file}},
	} {
		input, err := os.Open("testdata/acl/" + archive)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][2]string{}
		err = tarstream.Walk(input, func(header *tarstream.Header, _ io.Reader) error {
			access, def, err := EntryACLs(header)
			if apperr.Is(err, "UNSAFE_ARCHIVE") {
				access = "refused"
			} else if err != nil {
				return err
			}
			got[header.Name] = [2]string{access, def}
			return nil
		}, nil)
		input.Close()
		if err != nil || len(got) != len(want) {
			t.Fatal(archive, got, err)
		}
		for name, acls := range want {
			if got[name] != acls {
				t.Errorf("%s %s:\n%q\n%q", archive, name, got[name], acls)
			}
		}
	}
}
