package files

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Crunging/Ludock/internal/db/dbtest"
)

const (
	stageID     = "0f8fad5b-d9cb-469f-a165-70867728950e"
	lookalikeID = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

// An unpack Ludock stopping cut off is recorded, so startup removes its
// staging folder and record, keeps a result it had already published, and
// leaves a look-alike folder nothing recorded.
func TestRemoveStagesAtStartup(t *testing.T) {
	f := setup(t)
	f.service.DB = dbtest.Open(t)
	f.target.ServerID = "server-1"
	stage := filepath.Join(f.data, "World", ".ludock-unpack-"+stageID)
	os.MkdirAll(filepath.Join(stage, "half", "region"), 0o700)
	os.WriteFile(filepath.Join(stage, "half", "level.dat"), []byte("half"), 0o600)
	os.MkdirAll(filepath.Join(f.data, "World", "Skyblock"), 0o755)
	lookalike := filepath.Join(f.data, "World", ".ludock-unpack-"+lookalikeID)
	os.MkdirAll(filepath.Join(lookalike, "game data"), 0o700)
	if err := f.service.recordStage(ctx, Stage{Server: "server-1", Root: f.data, Archive: "World/Skyblock.zip", ID: stageID}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RemoveStages(ctx, func(context.Context, string) (*Target, error) { return &f.target, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("startup kept the staging folder")
	}
	if _, err := os.Stat(filepath.Join(f.data, "World", "Skyblock")); err != nil {
		t.Fatal("startup removed the published result")
	}
	if _, err := os.Stat(filepath.Join(lookalike, "game data")); err != nil {
		t.Fatal("startup removed a folder Ludock didn't record")
	}
	if stages, err := f.service.Stages(ctx); err != nil || len(stages) != 0 {
		t.Fatal(stages, err)
	}
	f.helpersLeft(t)
}

// A recorded staging folder that can't be removed never fails startup and is
// given up after stageAttempts startups; one whose server is gone is
// dropped at once.
func TestRemoveStagesGivesUp(t *testing.T) {
	f := setup(t)
	f.service.DB = dbtest.Open(t)
	f.service.recordStage(ctx, Stage{Server: "unreachable", Root: f.data, Archive: "a.zip", ID: stageID})
	f.service.recordStage(ctx, Stage{Server: "gone", Root: f.data, Archive: "b.zip", ID: lookalikeID})
	resolve := func(_ context.Context, id string) (*Target, error) {
		if id == "gone" {
			return nil, nil
		}
		return nil, errors.New("docker unavailable")
	}
	for start := 1; start <= stageAttempts; start++ {
		if err := f.service.RemoveStages(ctx, resolve); err != nil {
			t.Fatal(err)
		}
		stages, _ := f.service.Stages(ctx)
		want := 1
		if start == stageAttempts {
			want = 0
		}
		if len(stages) != want || (want == 1 && (stages[0].ID != stageID || stages[0].Attempts != start)) {
			t.Fatal(start, stages)
		}
	}
}
