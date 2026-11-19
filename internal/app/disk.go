package app

import (
	"context"
	"errors"
	"time"

	"github.com/Crunging/Ludock/internal/files"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/monitor"
	"github.com/Crunging/Ludock/internal/servers"
)

var errMeasureSkipped = errors.New("disk measurement skipped")

// checkDisks measures monitored servers' disks beside the tick, once at a
// time, since each folder starts a file helper.
func (a *App) checkDisks(ctx context.Context, current map[string]servers.Container, now int64) {
	if current == nil || !a.measuring.TryLock() {
		return
	}
	a.background.Add(1)
	go func() {
		defer a.background.Done()
		defer a.measuring.Unlock()
		if err := a.Monitor.CheckDisk(ctx, current, now); err != nil {
			log.Warn("Disk space checks are temporarily unavailable")
		}
	}()
}

// measureDisk measures the filesystem under each of a server's data folders
// through read-only file helpers, as background work that anything needing
// the server cancels.
func (a *App) measureDisk(ctx context.Context, server identity.Server) ([]monitor.DiskSpace, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	release, err := a.Locks.Acquire([]string{"server:" + server.ID}, cancel)
	if err != nil {
		return nil, err
	}
	defer release()
	observed, _, err := a.Observer.Inspect(ctx, server.ContainerID)
	if err != nil {
		return nil, err
	}
	if _, err := a.Identity.AssertObservedBinding(ctx, server.ID, observed.Observation, server.BindingRevision); err != nil {
		return nil, err
	}
	container := observed.Container
	target := files.Target{ContainerID: container.ID, Name: container.Name, GameType: container.GameType, Roots: container.FileRoots}
	assertBinding := func() error {
		_, err := a.Identity.ResolveBinding(ctx, server.ID, server.BindingRevision)
		return err
	}
	spaces := []monitor.DiskSpace{}
	for _, root := range target.Roots {
		space, err := a.Files.Space(ctx, target, root.ID, assertBinding)
		// Work that needed the server cancelled this, or Ludock is stopping:
		// neither says anything about the disk.
		if ctx.Err() != nil {
			return nil, errMeasureSkipped
		}
		if err != nil {
			spaces = append(spaces, monitor.DiskSpace{Path: root.Path, Failed: true})
			continue
		}
		spaces = append(spaces, monitor.DiskSpace{Path: root.Path, Total: space.Total, Available: space.Available, Used: space.Used})
	}
	return spaces, nil
}
