package jobs

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/Crunging/Ludock/internal/authz"
	"github.com/Crunging/Ludock/internal/backups"
	"github.com/Crunging/Ludock/internal/identity"
	"github.com/Crunging/Ludock/internal/ops"
	"github.com/Crunging/Ludock/internal/servers"
)

// createStoppedBackup writes a backup of a stopped server. The caller holds
// every data and storage lock and owns restoring the running state.
func (j *Jobs) createStoppedBackup(ctx context.Context, server *servers.Context, job *ops.Context) (*backups.Backup, error) {
	settings, err := j.Backups.ForBackup(ctx)
	if err != nil {
		return nil, err
	}
	if err := j.Backups.AssertSeparateDestination(ctx, server.Observation, settings); err != nil {
		return nil, err
	}
	if err := j.assertStopped(ctx, server, job); err != nil {
		return nil, err
	}
	if err := j.Backups.Storage.AssertSpace(settings.Destination, settings.ReserveBytes, 0); err != nil {
		return nil, err
	}
	used, err := j.Backups.ArchiveBytes(ctx)
	if err != nil {
		return nil, err
	}
	available, err := backups.Remaining(settings, used)
	if err != nil {
		return nil, err
	}
	id := identity.NewUUID()
	if err := job.Progress("backing_up", func(r *ops.Recovery) {
		r.BackupID, r.BackupDestination, r.BackupComplete = id, settings.Destination, false
	}, nil); err != nil {
		return nil, err
	}
	var data *backups.DataHelper
	recorded := false
	defer func() {
		if data != nil {
			data.Cleanup()
		}
		if err := job.Progress(job.Job.Phase, func(r *ops.Recovery) { r.DataHelperID = "" }, nil); err != nil {
			log.Warn("Could not save a backup helper's removal; recovery will look for it again")
		}
	}()
	backup, err := func() (*backups.Backup, error) {
		if err := j.dataAuthority(ctx, server, job); err != nil {
			return nil, err
		}
		data, err = j.Backups.Helpers.NewDataHelper(ctx, server, true, job.Job.ID)
		if err != nil {
			return nil, err
		}
		if err := job.Progress("backing_up", func(r *ops.Recovery) { r.DataHelperID = data.Container }, nil); err != nil {
			return nil, err
		}
		var mu sync.Mutex
		var stoppedFailure error
		assertCopy := func() error {
			mu.Lock()
			failure := stoppedFailure
			mu.Unlock()
			if failure != nil {
				return failure
			}
			if err := j.assertStopped(ctx, server, job); err != nil {
				return err
			}
			return j.dataAuthority(ctx, server, job)
		}
		watch, stop := context.WithCancel(ctx)
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			ticker := time.NewTicker(j.poll())
			defer ticker.Stop()
			for {
				select {
				case <-watch.Done():
					return
				case <-ticker.C:
					if err := assertCopy(); err != nil {
						mu.Lock()
						if stoppedFailure == nil {
							stoppedFailure = err
						}
						mu.Unlock()
						// Stop the copy at once; cleanup also runs later.
						data.Cleanup()
						return
					}
				}
			}
		}()
		size, checksum, err := j.Backups.Helpers.WriteSnapshot(ctx, server, data, settings, id, available, assertCopy,
			func() error { return j.dataAuthority(ctx, server, job) })
		stop()
		<-watched
		mu.Lock()
		failure := stoppedFailure
		mu.Unlock()
		if failure != nil {
			return nil, failure
		}
		if err != nil {
			return nil, err
		}
		if _, err := j.Backups.Storage.Validate(settings.Destination, id, data.Roots, settings.MaxBytes, checksum); err != nil {
			return nil, err
		}
		if err := assertCopy(); err != nil {
			return nil, err
		}
		backup := &backups.Backup{ID: id, ServerID: server.Logical.ID, Roots: data.Roots, Size: size, Checksum: checksum,
			CreatedAt: time.Now().UnixMilli(), State: "complete"}
		if err := j.Backups.Record(ctx, *backup, server.Logical.BindingFingerprint, settings.Destination); err != nil {
			return nil, err
		}
		recorded = true
		// Saved only after the row commits; backupCompleted trusts it.
		if err := job.Progress("backup_complete", func(r *ops.Recovery) { r.BackupComplete = true }, nil); err != nil {
			log.Warn("Could not save a backup's completion marker; its record is already saved")
		}
		keep, _ := text(job.Job.Input["backupId"])
		if err := j.Backups.Prune(ctx, server.Logical.ID, settings.RetentionCount, keep); err != nil {
			return nil, err
		}
		return backup, nil
	}()
	if err != nil && !recorded {
		removeErr := j.Backups.Storage.Remove(settings.Destination, id, nil)
		if removeErr == nil {
			removeErr = j.Backups.Storage.RemovePartial(settings.Destination, id)
		}
		// Record an archive that could not be removed as deleting, so Prune and
		// Delete retry it.
		if removeErr != nil {
			if recordErr := j.Backups.RecordDeleting(context.WithoutCancel(ctx), id, server.Logical.ID, server.Logical.BindingFingerprint,
				settings.Destination); recordErr != nil {
				log.Warn("Could not remove or record a failed backup's archive; it may use backup storage until removed")
			}
		}
	}
	return backup, err
}

func (j *Jobs) runBackup(ctx context.Context, job *ops.Context) (map[string]any, error) {
	server, err := j.resolve(ctx, job, authz.BackupsCreate, true)
	if err != nil {
		return nil, err
	}
	return job.WithLocks(append(slices.Clone(server.LockKeys), "backups:storage"), func() (result map[string]any, err error) {
		if err := j.assertCapability(ctx, job, authz.BackupsCreate); err != nil {
			return nil, err
		}
		if err := j.Backups.AssertReady(ctx, server); err != nil {
			return nil, err
		}
		defer func() {
			if restoreErr := j.restoreInitialState(context.WithoutCancel(ctx), server, job); restoreErr != nil && err == nil {
				err = restoreErr
			}
		}()
		backup, err := func() (*backups.Backup, error) {
			if err := j.stopForData(ctx, server, job); err != nil {
				return nil, err
			}
			return j.createStoppedBackup(ctx, server, job)
		}()
		if err != nil {
			j.notify("backup-failed:"+job.Job.ID, "Backup failed for "+server.Container.DisplayName+". Review its operation details.")
			return nil, err
		}
		return map[string]any{"backupId": backup.ID, "size": backup.Size, "checksum": backup.Checksum}, nil
	})
}

// backupCompleted trusts the completion marker, which is saved only after the
// backup row commits, and otherwise checks the row. A failed lookup counts as
// incomplete, which keeps the server stopped.
func (j *Jobs) backupCompleted(ctx context.Context, job *ops.Context) bool {
	if job.Job.Recovery.BackupComplete {
		return true
	}
	if job.Job.Recovery.BackupID == "" {
		return false
	}
	complete, err := j.Backups.Exists(ctx, job.Job.Recovery.BackupID, true)
	return err == nil && complete
}

func (j *Jobs) removeUnpublished(ctx context.Context, job *ops.Context) error {
	id, destination := job.Job.Recovery.BackupID, job.Job.Recovery.BackupDestination
	if id == "" || destination == "" {
		return nil
	}
	// Only a backup known to have no row is unpublished.
	if recorded, err := j.Backups.Exists(ctx, id, false); err != nil || recorded {
		return err
	}
	if err := j.Backups.Storage.Remove(destination, id, nil); err != nil {
		return err
	}
	return j.Backups.Storage.RemovePartial(destination, id)
}

func (j *Jobs) recoverBackup(ctx context.Context, job *ops.Context, startServer bool) error {
	return j.withRecoveryLocks(ctx, job, func(server *servers.Context) error {
		if err := j.removeUnpublished(ctx, job); err != nil {
			return err
		}
		if startServer {
			return j.restoreInitialState(ctx, server, job)
		}
		return j.keepStoppedAfterFailedBackup(ctx, server, job)
	})
}
