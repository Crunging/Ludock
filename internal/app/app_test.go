package app

import (
	"context"
	"strings"
	"testing"

	"github.com/Crunging/Ludock/internal/config"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
)

// A page read just before the background tick does not stand in for the
// tick's own discovery: monitoring and schedules see Docker as it is now.
func TestTickDiscoversAfterARecentRead(t *testing.T) {
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true})
	a, err := New(config.Config{DockerSocket: engine.Socket}, dbtest.Open(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	listings := func() int {
		n := 0
		for _, request := range engine.Requests {
			if strings.Contains(request, "/containers/json") {
				n++
			}
		}
		return n
	}
	if _, err := a.Servers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := listings()
	a.Tick(context.Background())
	a.background.Wait()
	if listings() == before {
		t.Fatal("the tick reused a page read's discovery")
	}
}
