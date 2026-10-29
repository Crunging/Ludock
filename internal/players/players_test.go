package players

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/console"
	"github.com/Crunging/Ludock/internal/db"
	"github.com/Crunging/Ludock/internal/db/dbtest"
	"github.com/Crunging/Ludock/internal/docker"
	"github.com/Crunging/Ludock/internal/docker/fakedocker"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/locks"
	"github.com/Crunging/Ludock/internal/servers"
)

// A console command changes who is online while a reading is still running.
// The reading asked for after the command runs once that one ends, so the
// change shows without waiting for the next poll.
func TestCheckDuringReadingReadsAgain(t *testing.T) {
	ctx := context.Background()
	engine, err := fakedocker.Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	answered, release := make(chan struct{}), make(chan struct{})
	online := "Mara"
	port, err := fakedocker.ServeRCON("secret", func(string) string {
		name := online
		if name == "Mara" {
			answered <- struct{}{}
			<-release
		}
		return "There are 1 of a max of 10 players online: " + name
	})
	if err != nil {
		t.Fatal(err)
	}
	engine.AddContainer(fakedocker.GameOptions{Name: "mc", Image: "itzg/minecraft-server", Running: true,
		Labels: map[string]string{"ludock.console.host": "127.0.0.1", "ludock.console.port": fmt.Sprint(port)},
		Env:    []string{"RCON_PASSWORD=secret"}})
	d := dbtest.Open(t)
	client := docker.New(engine.Socket)
	store := &identity.Store{DB: d}
	observer := &servers.Observer{Docker: client}
	service := &servers.Service{DB: d, Identity: store, Authz: &authz.Service{DB: d, Servers: store}, Observer: observer}
	admin := &db.SessionUser{ID: "admin", Username: "admin", Role: db.RoleAdmin}
	d.CreateUser(ctx, db.UserRecord{User: db.User{ID: admin.ID, Username: admin.Username, Role: admin.Role, CreatedAt: 1}, PasswordHash: "x"})
	views, err := service.List(ctx, admin)
	if err != nil || len(views) != 1 {
		t.Fatal(views, err)
	}
	players := &Service{Servers: service, Identity: store, Observer: observer, Runtime: &console.Runtime{Docker: client},
		Locks: locks.New()}
	t.Cleanup(players.Stop)

	players.Check(ctx, views[0].ID)
	<-answered
	online = "Theo"
	players.Check(ctx, views[0].ID)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		players.mu.Lock()
		reading := players.readings[views[0].ID]
		players.mu.Unlock()
		if reading != nil && len(reading.Names) == 1 && reading.Names[0] == "Theo" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the second check was dropped:", reading)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
