package selfupdate_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/self"
	"github.com/Crunging/Ludock/internal/selfupdate"
	"github.com/Crunging/Ludock/internal/update"
)

// Each folder in testdata/1.0/self-update is Ludock's data folder and
// Docker's containers as a 1.0 install left them after updating itself, or
// partway through, as its README says. Ludock starts on them as cmd/ludock
// does, in the Ludock container that runs, and records how the update ended
// on the data it should have.
func TestReadsSelfUpdatesRecordedBy10(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, state, data string
		// probe is the data Ludock runs on: "before" the update's copy, or
		// "after" it.
		probe string
		// copy: the data copy stays beside the database.
		copy, leftover, onLeftover bool
	}{
		{name: "succeeded", state: selfupdate.StateSucceeded, probe: "after"},
		{name: "current", state: selfupdate.StateCurrent, probe: "before"},
		{name: "put-back-restored", state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before"},
		{name: "put-back-kept", state: selfupdate.StatePutBack, data: selfupdate.DataKept, probe: "after"},
		{name: "put-back-not-restored", state: selfupdate.StatePutBack, data: selfupdate.DataNotRestored, probe: "after", copy: true},
		{name: "failed", state: selfupdate.StateFailed, probe: "after"},
		{name: "interrupted-leftover", state: selfupdate.StateInterrupted, probe: "after", copy: true, leftover: true, onLeftover: true},
		// The new version up under Ludock's name, its helper done.
		{name: "updating-succeeded", state: selfupdate.StateSucceeded, probe: "after"},
		// The original running again on data the new version migrated, its
		// helper done: the copy from 1.0 is put back.
		{name: "updating-put-back", state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before"},
		// The helper killed mid-swap, and the owner started the original: with
		// no word from the helper, it puts the update back, data and all, and
		// removes the new container it never started.
		{name: "updating-owner-restarted", state: selfupdate.StatePutBack, data: selfupdate.DataRestored, probe: "before"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fixture := filepath.Join("testdata/1.0/self-update", c.name)
			engine, err := fakedocker.Start(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { engine.Close() })
			encoded, err := os.ReadFile(filepath.Join(fixture, "docker.json"))
			if err != nil {
				t.Fatal(err)
			}
			var containers []fakedocker.Container
			if err := json.Unmarshal(encoded, &containers); err != nil {
				t.Fatal(err)
			}
			var own fakedocker.Container
			for _, container := range containers {
				engine.AddContainer(fakedocker.GameOptions{ID: container.ID})
				engine.Update(container.ID, func(c *fakedocker.Container) { *c = container })
				if container.State.Running && container.Config.Labels["com.docker.compose.service"] == "ludock" && container.Config.Labels["ludock.internal"] == "" {
					own = container
				}
			}
			dataDir := t.TempDir()
			unpackData(t, filepath.Join(fixture, "data.tar.gz"), dataDir)

			client := docker.New(engine.Socket)
			putBack, held, _ := selfupdate.BeforeOpen(ctx, client, own.ID, dataDir, 2*time.Second)
			if held != nil {
				t.Cleanup(func() { held.Close() })
			}
			d, err := db.Open(filepath.Join(dataDir, "ludock.db"))
			if err != nil {
				t.Fatal("open:", err)
			}
			t.Cleanup(func() { d.Close() })
			svc := &selfupdate.Service{DB: d, Docker: client, Own: self.New(client, own.ID), Updates: &update.Service{Docker: client}, Locks: locks.New(),
				DataDir: dataDir, Socket: socket, MountID: own.ID, Version: map[string]string{oldImage: "1.0.0", newImage: "1.1.0"}[own.Image],
				Poll: 5 * time.Millisecond, PutBack: putBack}
			svc.Recover(ctx, ctx)
			for deadline := time.Now().Add(10 * time.Second); svc.Updating() || svc.Last(ctx).InProgress(); time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the update didn't end")
				}
			}
			record := svc.Last(ctx)
			if record.State != c.state || record.Data != c.data || record.Copy != c.copy || (record.Leftover != "") != c.leftover || record.OnLeftover != c.onLeftover ||
				c.onLeftover && record.Leftover != strings.TrimPrefix(own.Name, "/") {
				t.Errorf("recorded %+v", record)
			}
			var probe string
			if _, err := d.Setting(ctx, "probe", &probe); err != nil || probe != c.probe {
				t.Errorf("data %q %v, want %q", probe, err, c.probe)
			}
			if _, err := os.Stat(filepath.Join(dataDir, selfupdate.CopyName)); (err == nil) != c.copy {
				t.Errorf("data copy left: %v", err == nil)
			}
			// The helper goes just after the outcome is saved.
			helper := func() bool {
				for _, id := range engine.SortedIDs() {
					if info := engine.Container(id); info != nil && info.Config.Labels["ludock.internal"] == "self-update" {
						return true
					}
				}
				return false
			}
			for deadline := time.Now().Add(5 * time.Second); helper() && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			if helper() {
				t.Error("the helper is left")
			}
		})
	}
}

// unpackData extracts a gzipped tar of files and folders into folder.
func unpackData(t *testing.T, archive, folder string) {
	t.Helper()
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(zipped)
	for {
		header, err := r.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(folder, header.Name)
		if header.Typeflag == tar.TypeDir {
			err = os.MkdirAll(path, os.FileMode(header.Mode))
		} else {
			var content []byte
			if content, err = io.ReadAll(r); err == nil {
				err = os.WriteFile(path, content, os.FileMode(header.Mode))
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}
