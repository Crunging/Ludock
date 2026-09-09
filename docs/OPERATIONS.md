# Operations

Ludock v2 manages one Docker host from one backend instance. Use the Linux
container distribution for Compose project access. Docker is the source of
live container state; owning managers retain their configuration.

## Deployment and fresh storage

The [example Compose file](../compose.yaml) publishes port 3000, mounts the
Docker socket, and stores Ludock's database in a new `ludock-data-v2` named volume
mounted at `/data`. Back up and
retain any v1 application directory separately. v2 rejects incompatible
application databases; it does not migrate or delete them. Do not remove game
containers or game-data volumes when setting up fresh Ludock storage.

Create the first administrator within five minutes of starting an empty
installation. Restart Ludock to reopen an expired setup window. Passwords must
be between 15 and 128 characters.

Use HTTPS for remote access. A reverse proxy must preserve the public host,
forward the external protocol, and support WebSocket upgrades. Browser API
requests and WebSockets use same-origin session cookies. The Docker socket
remains a host-level administrative interface even when mounted `:ro`.

## Environment settings

Copy [`.env.example`](../.env.example) to `.env` beside the Compose file. Its
optional `env_file` entry passes these settings into Ludock. Recreate the
service after changing deployment environment or mounts:

```bash
docker compose up -d --force-recreate ludock
```

| Variable | Purpose / default |
| --- | --- |
| `LOG_LEVEL` | `error`, `warn`, `info` (default), or `debug` |
| `LUDOCK_DB_PATH` | Application database; `/data/ludock.db` in the image |
| `DOCKER_SOCKET` | Socket path inside Ludock; `/var/run/docker.sock` |
| `LUDOCK_API_TOKEN` | Optional full-administrator API token, at least 32 characters; shorter values are ignored |
| `LUDOCK_COMPOSE_ROOTS` | Approved directories containing Compose inputs; empty disables project access |
| `LUDOCK_BACKUP_ROOTS` | Approved mounted backup directories; empty disables backup destination configuration |
| `LUDOCK_DOCKER_CONFIG` | Optional read-only Docker client configuration directory for private-registry credentials; otherwise `/nonexistent` |
| `LUDOCK_SELF_CONTAINER` | Ludock's container ID or name if its hostname cannot identify it for backup mount verification |
| `LUDOCK_SENSITIVE_PATHS` | Additional protected filesystem paths; `FILE_SENSITIVE_PATHS` is an older alias |
| `FILE_HELPER_IMAGE` | File and backup helper image; `node:24-alpine` |
| `MAX_UPLOAD_BYTES` | Maximum file upload; 2 GiB by default |
| `AUDIT_LOG_MAX_ROWS` | Retained audit entries; 100,000 by default |
| `PORT` | Backend listener; 3000 in the image, 3001 during development |

Root lists use the platform path separator: `:` in the Linux container. Use
dedicated absolute directories, not `/` or system directories. Environment
settings select the allowed boundary; administrators select resources inside
that boundary through Settings.

## Server identity and access

API server IDs are Ludock UUIDs, not Docker container IDs. Compose servers bind
to project/service/replica identity; standalone servers bind to their container
name. Ordinary verified recreation preserves the UUID and grants. A standalone
rename creates a new identity. Missing servers retain history; ambiguous
identities cannot be mutated.

Material changes to game configuration, writable mounts, or project binding
require administrator review. Non-administrator access is suspended. **Accept
binding** restores grants for the reviewed server; schedules whose data binding
changed remain suspended and must be reviewed and recreated separately. Old
backups must still match the accepted data binding to be restored.

Roles set permission ceilings. Operators and viewers additionally require
explicit per-server grants, including `server.view`:

| Action | Administrator | Operator with grant | Viewer with grant |
| --- | --- | --- | --- |
| Status / metadata | All eligible servers | `server.view` | `server.view` |
| Start, stop, restart | Yes | Each granted independently | No |
| Logs | Yes | `logs.read` | `logs.read` |
| Game console | Yes | `console.execute` | No |
| File browse/download | Yes | `files.read` | `files.read` |
| File changes | Yes | `files.write` plus `files.read` | No |
| Create backup | Yes | `backups.create` | No |
| Own schedules | Yes | `schedules.manage` plus each scheduled action | No |
| Restore, archive download/delete, update/recreate | Yes | No | No |
| Container shell, grants, system configuration, diagnostics/audit | Yes | No | No |

A backup grant permits the temporary stop/restart required by the configured
backup policy, without granting the general lifecycle endpoints. Scheduled
work runs with its owner's current rights; disabled accounts, revoked rights,
and deleted schedules prevent queued actions. Operators cannot edit or delete
another owner's schedules. Revocation also closes affected live streams.

## File access

Safe writable bind mounts and named volumes become file roots automatically.
`ludock.files` can restrict them, and an empty value disables access. Container
writable-layer paths are not supported. Host roots, broad system directories,
Docker sockets, read-only mounts, sensitive paths, and symlink traversal are
blocked. See [Game servers](./GAME-SERVERS.md#file-roots).

Every file operation uses a temporary `node:24-alpine` helper, whether the game
is running or stopped. It mounts only approved data, has no network, uses a
read-only root filesystem and `no-new-privileges`, drops capabilities, and adds
only required data-access capabilities. Helpers run as UID 0 to access game
files; this does not remove the Docker-host trust requirement. File requests
revalidate their paths and mount bindings. Stop a game before manually replacing
an active world or related configuration files.

Approved nested data mounts remain accessible through their parent file root.
Excluded nested mounts are hidden and cannot be traversed. Downloading, deleting,
or renaming an ancestor that contains an excluded mount is also rejected.
Downloads and overwrites reject hard-linked files; directory downloads reject
symlinks and special files anywhere in the archive. Deleting a symlink removes
the link itself.

Directory listings accept at most 10,000 entries. Uploads and downloads have a
30-minute execution limit; uploads also stop after 60 seconds without input.
Ordinary file commands have a 55-second limit. Temporary file helpers are removed
after use and automatically expire after 35 minutes if Ludock is interrupted.
Their non-job mount validators also expire and remove themselves.

Bind mounts also require a private, read-only host validator. It checks only
source-directory metadata and holds the verified directory open while the scoped
helper runs; file endpoints cannot access the validator. Symlinked source paths,
Docker Desktop host aliases, and hosts without recursive read-only bind support
can make bind access unavailable. Named volumes must use the local driver without
host-remapping options.

The default helper must be available for the Docker host's architecture. A
custom `FILE_HELPER_IMAGE` must supply Node.js 24, `/bin/sh`, and `sleep`, and run
with Linux `/proc/self/fd` support; a plain Alpine image is insufficient.
Validate overrides on both architectures before distributing them.

## Configure backups

Mount a dedicated destination into Ludock, separate from every selected game
root:

```yaml
services:
  ludock:
    environment:
      LUDOCK_BACKUP_ROOTS: /backups
    volumes:
      - /srv/ludock-backups:/backups
```

This is a fragment to merge into the deployment, retaining the socket and
application-data mounts. Create the destination directory before starting.
Under **Settings → Backup storage**, choose `/backups`, retention per server,
global byte limit, and free-space reserve. Ludock verifies that the destination
is actually mounted into its container and does not overlap game data. Set
`LUDOCK_SELF_CONTAINER` if a custom hostname prevents self-inspection.

A backup:

1. Locks the server, project, shared data roots, and backup storage.
2. Records whether the server was running, stops it, and verifies stopped state.
3. Rejects known running containers that can write the same roots, and checks for
   external starts or replacements during the copy.
4. Copies selected directory roots to a temporary tar archive, validates entries
   and SHA-256, then finalizes the archive and metadata.
5. Prunes older backups after a successful new backup. The backup currently
   selected for a restore is retained.
6. Restores the previous running state after success or recoverable failure.
   Initially stopped servers remain stopped.

There is no live-backup option. A stop that required forced termination (exit 137 or OOM termination) rejects
the backup; check shutdown behavior for each game.
Ludock cannot lock Watchtower, external Compose invocations, or other writers.

Space is required for the new archive *before* retention removes old archives.
A full global limit can therefore require manual archive deletion or a higher
limit. The destination reserve applies during copying. Restore staging needs
additional free space on each game's own filesystem.

Backups accept regular files and directories only. Symlinks, hard links,
special files, individual file mounts, unsafe paths, oversized archives, and
roots containing nested mounts are rejected. Selected roots must also be
physically distinct: two container paths exposing the same or overlapping host
data cannot be backed up together. Archives are limited to 100,000 entries and
64 path components, including their archive prefixes. Restrict `ludock.files`
to suitable data directories when necessary.

Archives preserve ordinary permission bits, UID/GID ownership, and modification
times. ACLs, extended attributes, and sparse-file layout are not preserved.
Backup archives and Ludock's database should
be protected together: the database holds the binding, root, and checksum
metadata used for restoration. There is no arbitrary archive-import feature.

## Restore and interrupted operations

An administrator selects a backup and types the server name to confirm data
replacement. Ludock validates the archive and target binding, stops the server,
and takes a safety backup before replacement. If that backup cannot complete,
restoration aborts without replacing the data. Restore uses
per-root staging and a persisted replacement journal. Replacement across
several mounts is **not atomic**.

After a failure or restart, recovery uses that journal to finish cleanup or
roll back moved roots. The server returns to its initial running state only
when its data is known to be safe. If recovery or identity validation fails,
leave it stopped and inspect the operation before retrying. Do not delete
`.ludock-restore-*` directories or application operation records as a cleanup
shortcut: they can contain the previous game data needed for recovery.

On Ludock startup, interrupted destructive work is reconciled rather than
blindly replayed. Review **Server → Activity**, the actual Docker state, and
owning-manager logs after an interruption. There is no general operation-cancel
API; stopping Ludock is not a cancel/rollback mechanism.

## Register Compose projects

Compose access requires Ludock's Linux container runtime, the bundled Docker
CLI/Compose plugin, and explicitly mounted trusted source directories. Preserve
the **same absolute project paths on the host and inside Ludock**, especially
for relative bind mounts. Docker resolves bind sources on its host, not inside
Ludock.

```yaml
services:
  ludock:
    environment:
      LUDOCK_COMPOSE_ROOTS: /srv/game-stacks
    volumes:
      - /srv/game-stacks:/srv/game-stacks:ro
```

Register the existing Compose project name, absolute project directory, Compose
files in merge order, and CLI environment files in order under **Settings**.
All inputs must exist, be regular files within approved roots, and avoid symlink
traversal. Each source file has a 2 MiB limit. Implicit `.env` loading is disabled;
register needed interpolation files explicitly. Do not enter secret contents in
project names or file-path fields.

Supported source input is deliberately constrained:

- Local Compose files and literal local service `env_file` paths are snapshotted
  before Compose runs. Environment-file reads also stay inside approved roots.
- `include`, `extends`, secrets/configs, `label_file`, credential specs, lifecycle
  hooks, providers, development configuration, and `volumes_from` are rejected.
- Build-only services are rejected. When a service has both `image` and `build`,
  updates use the configured pullable image and do not run a build.
- Digest-pinned images cannot use the update action.
- Updates require exactly one existing/configured replica of the selected
  service. Service namespace dependencies such as `network_mode: service:...`
  must be updated through the owning manager.
- Source changes require validating and registering the project again.

Ludock executes argument arrays with a restricted environment and does not log
raw resolved configuration or CLI errors containing secrets. Private registry
access may use a dedicated read-only `LUDOCK_DOCKER_CONFIG` mount; do not expose
that directory as a game file root.

**Update server** pulls the configured image, optionally takes a stopped-server
backup, and recreates the selected service with dependency recreation, builds,
and an additional pull disabled. **Recreate anyway** follows the same checks
when the image is unchanged. A skipped backup requires typing the server name.
Previously stopped servers remain stopped; startup-based game updates wait for
the next start. **Configured image is current** refers to the container image,
not the game version downloaded by an image's startup scripts.

Compose definitions remain authoritative. Redeployment can replace direct
runtime changes not saved in that source. Pull failure leaves the existing
container unchanged. If recreation fails, use the registered project in the
owning manager to inspect and recover the actual service state. A data backup
does not roll back the image or Compose definition. Ludock does not clone old
container configurations or silently switch image digests.

Portainer stacks whose source is inaccessible, standalone containers, and
unsupported projects remain manageable for supported lifecycle/data actions;
update them through their owning manager. Validated ordinary recreation is
rediscovered under the same Ludock UUID.

## Schedules, availability, and Discord

Schedules support start, stop, restart, and backup at a selected local time on
selected weekdays. Time zones are explicit. Missed times and spring-forward
gaps are skipped; a repeated fall-back time runs once. v2 does not schedule
updates or arbitrary shell commands. Review a failed/suspended schedule's
reported result and recreate it after fixing its permissions or data binding.

Availability is disabled by default. When enabled, the server is expected to
be available 24/7, with a default two-minute failure grace period. Current
integrations use Docker health when supplied or running state; this does not
prove that players can connect. Maintenance pauses monitoring. Ludock-initiated
stops and active operations suppress alerts. An intentionally stopped server
stays suppressed until observed running again. Loss of Docker connectivity is
reported as an inability to verify availability, without replacing identities.

Configure Discord under Settings, then enable monitoring per server. Discord
webhooks are write-only; blank replacement input preserves the saved URL.
Notifications cover outages/recoveries, backup and schedule failures, restore
results, and update outcomes. Delivery deduplicates recorded events, disables
Discord mentions, retries failures with bounded backoff, and stops after five
attempts. A process interruption after delivery but before acknowledgement can
still result in a repeated message. There is no image-update-available watcher.

## Application backup, logs, and account recovery

Stop Ludock before copying its application database and associated files. Keep
game backup archives separately; copying `/data` does not back up game worlds.
With the example deployment:

```bash
docker compose stop ludock
docker cp "$(docker compose ps --all --quiet ludock):/data/." ./ludock-data-v2.backup
docker compose start ludock
```

Use **Diagnostics**, **Audit log**, and **Ludock logs** for administrator-only
inspection. `docker compose logs --follow --tail 200 ludock` also shows backend
logs. Use `LOG_LEVEL=debug` temporarily; credentials and command contents should
never be included in reports.

To recover an existing administrator password, securely set and export
`LUDOCK_RECOVERY_PASSWORD` in the shell without putting the value in command
history, then run:

```bash
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD \
  ludock node packages/backend/dist/recovery.js admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the existing username. Recovery revokes that account's
sessions. It does not create an account or reset game data.

## API

HTTP routes use `/api/v1`; WebSockets use `/ws/v1`. Legacy unversioned endpoints
are not a compatibility interface. Server paths contain logical UUIDs. The
optional administrator API token uses `Authorization: Bearer …`; do not place
credentials in URLs. Browser clients use their session cookie.

Core resources include servers, per-server files/backups/restores/schedules/
availability/operations/updates, Compose projects, notifications, diagnostics,
and administrator-managed `/users/:userId/server-grants`. Long-running mutations
return an operation; poll its authorized resource for the actual outcome.
