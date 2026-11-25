# Work a 1.0 install left interrupted

Each folder is a 1.0 install killed partway through one operation, as
`TestRecoversWorkInterruptedOn10` starts it again:

- `data.tar.gz`: its data folder, `ludock.db` with its identity key. The
  database is what SQLite holds after the crash: everything committed.
- `host.tar.gz`: the host's files below the install's folder,
  `/var/tmp/ludock-1.0-interrupted/<name>`: `data/mc`, the server's data,
  mounted at `/data`, and `backups`, Ludock's backup folder. Owners and times
  are zeroed.
- `docker.json`: the fake daemon's containers, as `fakedocker.Container`,
  without Ludock's own.

They were made on 2026-11-25 from 7d004c0, the last three on review, by a throwaway test, not
committed. It ran each install in this package's harness against
`fakedocker`, with the server `mc` running and, for restores, one backup taken
and the data changed since. At the crash point (a helper step about to run or
just run, a Docker request, or, for the warning, its wait) it copied the
database with `VACUUM INTO`, the host's folder, and the daemon's containers.

| Folder | Killed |
| --- | --- |
| `restore-staging` | after unpacking into the stage, journal `staging` |
| `restore-moving-old` | after the old data moved out, journal still `moving_old` |
| `restore-old-moved` | before the new data moved in, journal `old_moved` |
| `restore-replaced` | new data in place, journal `replaced`, not committed |
| `restore-rolling-back` | a failed swap's new data removed, old data still in the stage, `rolling_back` |
| `restore-rolled-back` | old data back, stage not yet removed, `rolled_back` |
| `restore-committed` | committed, stage with the replaced data not yet removed |
| `item-old-moved`, `item-replaced` | as above, for a restore of the folder `world` |
| `undo-set-aside` | an undo, of a restore that failed when the server was started elsewhere, after it set aside what the game wrote, before journaling `kept` |
| `undo-set-aside-kept` | the same undo before it put the old data back, `kept` journaled |
| `backup-copying` | archive written, not published |
| `backup-stop-unconfirmed` | stop sent and done, not yet confirmed |
| `backup-start-sent` | backup recorded, start sent and done |
| `backup-warning` | players warned, waiting |
| `restart-stop-unconfirmed` | a restart's stop sent and done, not yet confirmed |
| `restart-backup-copying` | a restart with a backup, its archive written, not published |
| `restore-failed-pending` | nothing running: a restore that failed when the server was started elsewhere, new data in place, the server stopped since |
| `update-created` | replacement created under its temporary name, its ID not saved |
| `update-started` | replacement started, not yet verified |
| `update-original-removed` | original removed, replacement still under its temporary name |
| `update-replaced` | replacement renamed and given the original's policy, swap saved as `replaced` |

The test keeps the host's files below a temporary folder and maps the paths
helpers see onto it. Ludock opens its backup folder itself, so the test moves
that one path, in the backup rows and saved operations, to the temporary
folder, and points the settings at another folder there, as an owner who
chose a new one meanwhile would.

Never regenerate these to make a test pass. A failure means the change
breaks recovery of a 1.0 install upgraded partway through work.
