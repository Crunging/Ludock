# Work a 1.0 install left interrupted

Each folder is an install killed partway through one operation, as
`TestRecoversWorkInterruptedOn10` starts it again. Each was captured from a
commit before the 1.0 tag, named below; a set will be captured again from the
tagged binary.

- `data.tar.gz`: its data folder, `ludock.db` with its identity key. The
  database is what SQLite holds after the crash: everything committed.
- `host.tar.gz`: the host's files below the install's folder,
  `/var/tmp/ludock-1.0-interrupted/<name>`: `data/mc`, the server's data,
  mounted at `/data`, and `backups`, Ludock's backup folder. Owners and times
  are zeroed.
- `docker.json`: the fake daemon's containers, as `fakedocker.Container`,
  without Ludock's own.

Unless named below, they were made on 2026-11-25 from 7d004c0
(`restart-backup-copying`, `restore-failed-pending` and `update-replaced` on
review) by a throwaway test, not committed. It ran each install in this package's harness against
`fakedocker`, with the server `mc` running and, for restores, one backup taken
and the data changed since. At the crash point (a helper step about to run or
just run, a Docker request, or, for the warning, its wait) it copied the
database with `VACUUM INTO`, the host's folder, and the daemon's containers.
`restart-recovery-start-sent` was made from 3486aee4, the first commit whose
recovery starts an interrupted restart's server: a second install on the
same data recovered a restart killed as in `restart-stop-unconfirmed`, and
was killed in turn. `backup-warning-resend`, `backup-started-then-exited`,
`restore-committed-pending` and `schedule-waiting` were made from 5a6b4609:
for the second, the game exits as soon as Ludock's start takes; for the
third, removing the stage fails twice and the server is stopped once the
restore has failed; the fourth has no operation, only a schedule's run
waiting for players to leave.
`restart-stop-confirmed`, `undo-finished`, `undo-stage-missing`,
`needs-administrator`, `command-scheduled` and `recovery-restart` were made
on 2026-11-26 from a00b4e37, to hold fields no other install saved. For the
undos the server was started elsewhere, failing the restore, then stopped;
for `undo-stage-missing` the stage folder was moved out of `data/mc` first.
For `needs-administrator` an administrator first set an ntfy destination,
the address players connect to, the upload limit, verbose logging, trusted
proxies, and a container left out of Ludock; a restore was killed as in
`restore-replaced`, the server's container was recreated, and a second
install recovered it. `command-scheduled` has a console reachable over RCON
and monitoring that saved the game's last answer; `recovery-restart` has
automatic restarts on, and monitoring found the game silent past its wait.

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
| `backup-copying` | archive written, not published |
| `backup-stop-unconfirmed` | stop sent and done, not yet confirmed |
| `backup-start-sent` | backup recorded, start sent and done |
| `backup-warning` | players warned, waiting |
| `backup-warning-resend` | the warning not yet sent, its console unreachable |
| `backup-started-then-exited` | backup recorded, start taken, the game exited before Ludock confirmed it |
| `restart-stop-unconfirmed` | a restart's stop sent and done, not yet confirmed |
| `restart-recovery-start-sent` | that restart's recovery (from 3486aee4), its stop confirmed and its start saved and taken, not yet confirmed |
| `restart-backup-copying` | a restart with a backup, its archive written, not published |
| `restore-committed-pending` | nothing running: a restore that committed but couldn't remove its stage folder, the server stopped since |
| `schedule-waiting` | nothing running: a daily restart waiting for players to leave, saved as `Waiting for players to leave` |
| `restore-failed-pending` | nothing running: a restore that failed when the server was started elsewhere, new data in place, the server stopped since |
| `restart-stop-confirmed` | a restart's start sent, its stop confirmed and the run it stopped saved (`stoppedRun`) |
| `undo-finished` | an undo, removing its helper after it saved `undoFinished`, the restore journaled `putBack` |
| `undo-stage-missing` | the same undo with the stage gone, the restore journaled `stageMissing` |
| `needs-administrator` | nothing running: a restore a second install marked `needsAdministrator`, with the settings above |
| `command-scheduled` | a scheduled console command as it was sent, its input the schedule and revision, and the saved last answer |
| `recovery-restart` | an automatic recovery restart's stop sent, its input the run and answer judged hung, and the attempt monitoring saved |
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
