# Updates of Ludock a 1.0 install recorded

Each folder is a 1.0 install's data folder and Docker's containers after
Ludock updated itself from 1.0.0 to 1.1.0, or partway through, as
`TestReadsSelfUpdatesRecordedBy10` starts it again:

- `data.tar.gz`: the data folder: `ludock.db` with its identity key and, where
  the update left it, `ludock.db.before-update`, the copy of the database
  Ludock wrote before handing over. The database is what SQLite holds as it
  was taken: everything committed. Its `ludock.update` setting is the record,
  and its `probe` setting reads `before` in the copy and `after` once Ludock
  wrote to its data after the copy.
- `docker.json`: the fake daemon's containers, as `fakedocker.Container`:
  Ludock's, the update's helper and new container where they were left, and
  a game. Ludock's data folder was `/var/tmp/ludock-1.0-self-update/<name>/data`.

They were made on 2026-11-26 from a00b4e37 by a throwaway test, not
committed, that ran `TestSelfUpdateOrder`'s rig with that data folder. The
settled ones were taken once the Ludock under Ludock's name had recorded the
outcome; the `updating-` ones as the next Ludock would find them, with every
Docker request held while they were taken.

| Folder | Recorded |
| --- | --- |
| `succeeded` | `succeeded` |
| `current` | `current`: the tag still pointed at the running image |
| `put-back-restored` | `put_back`, data `restored`: the new version migrated the data and reported itself unhealthy |
| `put-back-kept` | `put_back`, data `kept`: the new container didn't start, and the copy was gone |
| `put-back-not-restored` | `put_back`, data `not_restored`: the new container didn't start, and Docker couldn't show the helper while the original waited for it, so the copy stayed |
| `failed` | `failed`: Docker refused to create the new container |
| `interrupted-leftover` | `interrupted`: the original couldn't be removed, so the new version runs under its temporary name, with the copy |
| `updating-succeeded` | `updating`, the helper done, the new version under Ludock's name, the copy beside the database |
| `updating-put-back` | `updating`, the helper done, the original started again on data the new version migrated, the copy beside it |
| `updating-owner-restarted` | `updating`, the helper killed mid-swap, the owner starting the original, the copy beside the database |

Never regenerate these to make a test pass. A failure means the change
breaks how Ludock reads an update a 1.0 install recorded or left partway.
