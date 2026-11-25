package selfupdate_test

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/selfupdate"
)

// testdata/1.0 holds what a 1.0 Ludock sends when it hands an update over:
// the helper it creates, with these placeholders for what differs per host,
// and the update record it saved beside its data save. A helper of any
// later version must finish that update, and the Ludock that comes up must
// read the record. restore-data's exit codes are read by such a helper too.
//
// Never change these to make this test pass: a failure means the change
// breaks updates started by 1.0.
func TestServesAnUpdateStartedBy10(t *testing.T) {
	ctx := context.Background()
	g := newRig(t)
	old := g.boot(g.old, oldImage, "")
	fill := strings.NewReplacer("{{image}}", newImage, "{{container}}", g.old, "{{old-image}}", oldImage, "{{socket}}", socket,
		"{{socket-source}}", g.engine.Socket)
	read := func(name string) string {
		content, err := os.ReadFile(filepath.Join("testdata/1.0", name))
		if err != nil {
			t.Fatal(err)
		}
		return fill.Replace(string(content))
	}

	g.client.Pull(ctx, tag, "", "")
	var record map[string]any
	if err := json.Unmarshal([]byte(read("record.json")), &record); err != nil {
		t.Fatal(err)
	}
	if err := old.d.SetSetting(ctx, nil, "ludock.update", record); err != nil {
		t.Fatal(err)
	}
	snapshot, err := old.d.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	save, err := os.Create(filepath.Join(g.dataDir, selfupdate.SaveName(record["id"].(string))))
	if err == nil {
		_, err = io.Copy(save, snapshot)
		save.Close()
	}
	snapshot.Close()
	if err != nil {
		t.Fatal(err)
	}
	var helper struct {
		Name string
		Body json.RawMessage
	}
	if err := json.Unmarshal([]byte(read("helper.json")), &helper); err != nil {
		t.Fatal(err)
	}
	id, err := g.client.Create(ctx, helper.Name, "", helper.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.client.Start(ctx, id); err != nil {
		t.Fatal(err)
	}

	after, running := g.settle()
	if running.Image != newImage || running.ID == g.old {
		t.Errorf("Ludock runs %s in %s", running.Image, running.ID)
	}
	if last := after.svc.Last(ctx); last.State != selfupdate.StateSucceeded || last.From != "1.0.0" || last.To != "1.1.0" {
		t.Errorf("recorded %+v", last)
	}

	// restore-data's exit codes: a copy it can't restore, and a folder that
	// already holds data.
	damaged := filepath.Join(t.TempDir(), "damaged.tar")
	os.WriteFile(damaged, []byte("not an archive"), 0o600)
	if _, err := db.RestoreSnapshot(damaged, filepath.Join(t.TempDir(), "ludock.db")); db.RestoreExitCode(err) != selfupdate.RestoreFinal {
		t.Errorf("a damaged copy exits %d", db.RestoreExitCode(err))
	}
	if _, err := db.RestoreSnapshot(damaged, filepath.Join(g.dataDir, "ludock.db")); db.RestoreExitCode(err) != selfupdate.RestoreExists {
		t.Errorf("a folder with data exits %d", db.RestoreExitCode(err))
	}
}
