package selfupdate_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/selfupdate"
)

const operation10 = "0d3e7b1c-5a2f-4c6e-9b8d-1f2a3b4c5d6e"

// testdata/1.0 holds what a 1.0 Ludock sends when it hands an update over,
// with placeholders for what differs per host: the helper it creates, the
// update record it saved beside its data save, and the readiness marker it
// writes once it opens its data. What a 1.0 Ludock reads back from a helper,
// the markers' labels, the restore-data command and its exit codes, and the
// settled file, are the literals below. A helper of any later version, run
// through the same argument parsing as `ludock self-update`, must finish
// such an update forward and put it back. The data save's own format is
// pinned by TestReadsDataWrittenBy10 in internal/jobs.
//
// Never change these to make this test pass: a failure means the change
// breaks updates started by 1.0.
func TestServesAnUpdateStartedBy10(t *testing.T) {
	ctx := context.Background()
	for _, putBack := range []bool{false, true} {
		t.Run(map[bool]string{false: "forward", true: "put back"}[putBack], func(t *testing.T) {
			g := newRig(t)
			var restore []string
			settled := ""
			if putBack {
				g.onStart = func(c *fakedocker.Container) {
					if isNew(c) {
						health(g, c, "unhealthy")
					}
				}
				g.step = func(cmd []string) (int, bool) {
					if cmd[0] == "restore-data" {
						restore = slices.Clone(cmd)
					}
					return 0, false
				}
				g.at["start old"] = func(*http.Request) {
					content, _ := os.ReadFile(filepath.Join(g.dataDir, ".ludock-update-"+operation10, "settled"))
					settled = string(content)
				}
			}
			old := g.boot(g.old, oldImage, "")
			g.client.Pull(ctx, tag, "", "")
			var record map[string]any
			if err := json.Unmarshal([]byte(fixture(t, "record.json", g, "")), &record); err != nil {
				t.Fatal(err)
			}
			if err := old.d.SetSetting(ctx, nil, "ludock.update", record); err != nil {
				t.Fatal(err)
			}
			snapshot, err := old.d.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			savePath := filepath.Join(g.dataDir, "ludock-update-"+operation10+".tar")
			sum := sha256.New()
			file, err := os.Create(savePath)
			if err == nil {
				_, err = io.Copy(io.MultiWriter(file, sum), snapshot)
				file.Close()
			}
			snapshot.Close()
			if err != nil {
				t.Fatal(err)
			}
			save := hex.EncodeToString(sum.Sum(nil))
			var helper struct {
				Name string
				Body json.RawMessage
			}
			if err := json.Unmarshal([]byte(fixture(t, "helper.json", g, save)), &helper); err != nil {
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
			base := map[string]string{"ludock.enable": "false", "ludock.update.operation": operation10}
			marker := func(kind string, extra map[string]string) {
				t.Helper()
				want := maps.Clone(base)
				want["ludock.internal"] = kind
				maps.Copy(want, extra)
				for _, create := range g.engine.Creates {
					if create.Labels["ludock.internal"] == kind {
						if !reflect.DeepEqual(create.Labels, want) {
							t.Errorf("%s labels\n got %v\nwant %v", kind, create.Labels, want)
						}
						return
					}
				}
				t.Errorf("no %s marker", kind)
			}
			marker("self-update-state", map[string]string{"ludock.update.name": "ludock",
				"ludock.update.policy": `{"MaximumRetryCount":0,"Name":"unless-stopped"}`, "ludock.update.save": save})
			last := after.svc.Last(ctx)
			if !putBack {
				if running.Image != newImage || running.ID == g.old || last.State != "succeeded" || last.From != "1.0.0" || last.To != "1.1.0" {
					t.Errorf("Ludock runs %s in %s, recorded %+v", running.Image, running.ID, last)
				}
				marker("self-update-healthy", map[string]string{"ludock.update.container": running.ID})
				return
			}
			if running.ID != g.old || last.State != "put_back" || last.Reason != "the new version reported itself unhealthy" {
				t.Errorf("Ludock runs %s in %s, recorded %+v", running.Image, running.ID, last)
			}
			marker("self-update-undo", map[string]string{"ludock.update.reason": "unhealthy", "ludock.update.restore": "true"})
			if want := []string{"restore-data", "--sha256", save, savePath}; !slices.Equal(restore, want) {
				t.Errorf("restore\n got %q\nwant %q", restore, want)
			}
			if settled != "restored\n" {
				t.Errorf("settled %q", settled)
			}
		})
	}

	t.Run("readiness and restore-data", func(t *testing.T) {
		g := newRig(t)
		var ready struct {
			Name string
			Body map[string]any
		}
		json.Unmarshal([]byte(fixture(t, "ready.json", g, "")), &ready)
		name, body := selfupdate.ReadyMarker(operation10, g.old, "2026-11-25T08:48:24.854299518Z", oldImage)
		encoded, _ := json.Marshal(body)
		var written map[string]any
		json.Unmarshal(encoded, &written)
		if name != ready.Name || !reflect.DeepEqual(written, ready.Body) {
			t.Errorf("readiness marker\n got %s %v\nwant %s %v", name, written, ready.Name, ready.Body)
		}
		// 0 restored, 3 a copy this version can't restore, 4 a folder that
		// already holds data.
		damaged := filepath.Join(t.TempDir(), "damaged.tar")
		os.WriteFile(damaged, []byte("not an archive"), 0o600)
		for folder, want := range map[string]int{t.TempDir(): 3, g.dataDir: 4} {
			file, digest, ok := db.ParseRestoreArgs([]string{"--sha256", strings.Repeat("a", 64), damaged})
			if _, err := db.RestoreSnapshot(file, filepath.Join(folder, "ludock.db"), digest); !ok || db.RestoreExitCode(err) != want {
				t.Errorf("restore-data exits %d, want %d", db.RestoreExitCode(err), want)
			}
		}
	})
}

// fixture is a file of testdata/1.0 for this host.
func fixture(t *testing.T, name string, g *rig, save string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata/1.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer("{{image}}", newImage, "{{container}}", g.old, "{{old-image}}", oldImage, "{{socket}}", socket,
		"{{socket-source}}", g.engine.Socket, "{{save}}", save, "{{started}}", "2026-11-25T08:48:24.854299518Z").Replace(string(content))
}
