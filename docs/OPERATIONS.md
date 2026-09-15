# Operations

## Deployment

Start with the [README](../README.md#run-with-docker) and
[`compose.yaml`](../compose.yaml). Run one Ludock backend per Docker host.
The example keeps application data at `/data` and game backup archives at
`/backups` in separate volumes. Keep both private. Unrelated databases and
unsupported schemas are rejected without modification; retain their data
before choosing a fresh application volume.

Use [`.env.example`](../.env.example) as the configuration reference. Copy it
to `.env` beside the Compose file and uncomment the settings you need.
`LUDOCK_PORT` changes the browser port; root lists use `:`-separated absolute
paths inside Ludock. Recreate the service after changing environment or mounts:

```bash
docker compose up -d --force-recreate ludock
```

Docker socket access grants host-level power even with a read-only mount.
Use HTTPS for remote access; the proxy must preserve the public host, forward
the external protocol, and support WebSocket upgrades. API requests and
WebSockets use same-origin session cookies.

## Discovery and console setup

`ludock.enable` takes precedence: invalid values exclude with an administrator
diagnostic, `false` excludes, and `true` includes. Values are trimmed and
case-insensitive. Without that label, Compose one-offs are excluded, recognized
images are included, and other images are excluded. A game label alone does
not make an unknown image eligible.

Recognition strips tags, digests, and registry prefixes and matches repository
suffixes, so private mirrors can work. This selects capabilities; it does not
verify image provenance or a game release's compatibility. **Diagnostics →
Game capabilities** shows current prerequisites and validation evidence from
[`server-presets.ts`](../packages/backend/src/server-presets.ts).

| Game | Recognized repository suffixes | Console | Default port |
| --- | --- | --- | --- |
| Minecraft | `itzg/minecraft-server` | In-container `rcon-cli` | Internal configuration |
| Factorio | `factoriotools/factorio` | Source RCON | 27015 |
| Palworld | `thijsvanloef/palworld-server-docker`, `jammsen/palworld-dedicated-server` | Source RCON | 25575 |
| ARK: Survival Evolved | `hermsi/ark-server`, `hermsi1337/ark-server`, `indifferentbroccoli/ark-server-docker` | Source RCON | 27020 |
| ARK: Survival Ascended | `sknnr/ark-ascended-server`, `mschnitzer/asa-linux-server` | Source RCON | 27020 |
| Counter-Strike 2 | `joedwards32/cs2` | Source RCON | 27015 |
| Project Zomboid | `renegademaster/zomboid-dedicated-server`, `renegade-master/zomboid-dedicated-server`, `renegade_master/zomboid-dedicated-server` | Source RCON | 27015 |
| Conan Exiles | `indifferentbroccoli/conan-exiles-enhanced-server-docker` | Source RCON | 25575 |
| V Rising | `trueosiris/vrising` | Source RCON | 25575 |
| Rust | `didstopia/rust-server` | WebRCON | 28016 |
| 7 Days to Die | `vinanrra/7dtd-server` | Telnet | 8081 |
| Valheim | `community-valheim-tools/valheim-server`, `lloesche/valheim-server` | None | — |
| Terraria | `hexlo/terraria-server-docker`, `beardedio/terraria`, `ryshe/terraria` | Process stdin | — |

These are console ports, not player ports. Network consoles require a reachable
address, enabled protocol, and credentials. Their native transports are plaintext;
use a trusted Docker network or a separately managed authenticated TLS tunnel.
Ludock sends console credentials to `ludock.console.host`, so trust that endpoint.

Minecraft runs `rcon-cli` inside the game container and needs no published RCON
port. Its image must provide `rcon-cli`, `/bin/sh`, `sleep`, `mkdir`, `rm`, and
`rmdir` for command execution and timeout cleanup. Terraria's stdin adapter
requires `stdin_open: true`, `StdinOnce` disabled, and an image that forwards
input to the server process.

| Label | Purpose |
| --- | --- |
| `ludock.enable` | Explicitly include/exclude a container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Integration override, such as `minecraft` or `terraria` |
| `ludock.console` | Adapter override; `disabled` or `none` disables commands |
| `ludock.console.host` / `ludock.console.port` | Console address override |
| `ludock.console.password-env` | **Name** of the game-container environment variable containing the password, never its value |
| `ludock.files` | Comma-separated container data paths; empty disables file and derived backup roots |

Console adapters are `minecraft-rcon`, `source-rcon`, `rust-webrcon`,
`telnet-console`, and `stdin-console`. For example, enroll a custom image by
adding these settings to its owning Compose service:

```yaml
stdin_open: true
labels:
  ludock.enable: "true"
  ludock.game: "terraria"
  ludock.name: "Terraria with friends"
```

## Access and identity

Administrators see all eligible servers. Operators and viewers need explicit
per-server grants, including `server.view`. Viewers can receive logs and file
read/download access. Operators can additionally receive individual lifecycle,
console, file-write, backup-create, and schedule-management grants. File writes
also require file-read access. Console and logs are independent grants.
Restore, archive download/delete, update/recreate, shell, and system configuration
remain administrator-only.

A backup grant includes its required temporary stop/restart, without granting
general lifecycle access. Scheduled work uses its owner's current rights;
revocation blocks queued actions and closes affected streams.

Server IDs are Ludock UUIDs. Compose identity uses project/service/replica;
standalone identity uses the container name. Verified recreation preserves history
and grants; a standalone rename creates a new identity. Material game, mount,
or project changes suspend non-administrator access until **Accept binding**.
Schedules whose data binding changed must be reviewed and recreated separately;
old backups must still match the accepted data binding.

Docker outages leave saved history readable and controls disabled until live state
can be verified. **Needs attention** includes historical failures from each server's
latest 100 operations; a later successful run does not remove the earlier failure.

## File access

Safe writable bind mounts and local named volumes become roots automatically,
including while stopped. Ludock exposes container paths, not host paths or the
container's writable layer. Restrict a root with, for example,
`ludock.files: "/data/worlds,/data/config"`; labels cannot expand approved mounts.

Read-only/system/sensitive paths, Docker sockets, symlink traversal, and root
deletion are blocked. Approved nested mounts remain accessible; excluded nested
mounts are hidden and block downloading, deleting, or renaming their ancestors.
Downloads and overwrites reject hard links; directory downloads also reject
symlinks and special files. Deleting a symlink removes only the link.

Helpers mount approved data, run as UID 0, and have no network. Stop a game
before manually replacing active worlds or configuration. Bind access requires
validated host paths and recursive read-only bind support; symlinked sources or
Docker Desktop aliases can make it unavailable. Named volumes must use the local
driver without host-remapping options.

Uploads stage a temporary sibling and replace the destination only after the
complete transfer is validated. Allow space for both files; canceling a batch
does not undo completed uploads. The default upload limit is 2 GiB; configure
`MAX_UPLOAD_SIZE` using decimal units (`MB`) or binary units (`MiB`).
Listings allow 10,000 entries. Transfers have a 30-minute limit; uploads also
stop after 60 seconds without input. Ordinary commands have a 55-second limit.
Helpers are removed after use and expire after 35 minutes if Ludock is interrupted.

Use the default helper digest unless you have validated an override. Missing
images are pulled automatically; upgrading Ludock adopts its new default digest.
Custom images must meet the requirements in [`.env.example`](../.env.example).

## Configure backups

With the example deployment, open **Settings → Backup storage**, choose
**Use /backups**, set retention/capacity limits, and save. For another disk,
replace the existing `/backups` volume mount with a host directory:

```yaml
services:
  ludock:
    environment:
      LUDOCK_BACKUP_ROOTS: /backups
    volumes:
      - /srv/ludock-backups:/backups
```

Merge this fragment into the deployment, retaining its socket and application-data
mounts. Create the host directory first. The destination must be mounted, writable,
and separate from game roots. Set `LUDOCK_SELF_CONTAINER` if a custom hostname
prevents mount verification.

Storage limits and reserves use GiB. Space for a new archive is required **before**
retention removes old archives; a full limit can require deletion or a higher limit.
The disk reserve applies during copying and to restore staging on game-data disks.
The storage summary reports saved limits, not unsaved form edits.

Use **Server → Backups** to check readiness. Preflight identifies configuration,
space, and shared-writer problems without stopping or copying. It reserves no space;
execution repeats validation for manual, scheduled, and nested backups.

Backups keep the server stopped throughout copying, then restore its initial running
state after success or recoverable failure. Initially stopped servers remain stopped.
Live backups are unsupported. Forced termination (exit 137/OOM), external starts,
replacement, and known running shared writers invalidate copying. Ludock cannot
lock external managers; verify the game's graceful shutdown and world integrity.

Roots must be physically distinct directories without nested mounts. Backups reject
links, special files, individual file mounts, and overlapping data roots. Archives
are limited to 100,000 entries and 64 path components, including archive prefixes.
Use `ludock.files` to select suitable subdirectories. Archives preserve ordinary
permissions, UID/GID, and modification times, but not ACLs, extended attributes,
or sparse layout. Protect archives and Ludock's database together: restore requires
the database's binding/root/checksum metadata; arbitrary archive import is unsupported.

## Restore and interrupted work

An administrator selects a backup and types the server name to confirm replacement.
Ludock validates the archive/binding, stops the server, and takes a safety backup.
Failure of that backup aborts replacement. Restore stages each root and persists a
replacement journal; replacement across mounts is **not atomic**.

After interruption, recovery uses the journal to finish cleanup or roll back moved
roots. The server returns to its initial running state only when data is known safe.
If recovery or identity validation fails, leave it stopped and inspect **Server →
Activity**, actual Docker state, and owning-manager logs. Do not delete
`.ludock-restore-*` directories or operation records: they may be needed to recover
previous game data. Stopping Ludock is not an operation-cancel mechanism.

**Operations** searches persisted work; administrators can follow related events
in **Audit log**. Filters never unlock active controls. Audit entries record the
event's outcome at that time, so an old queued event does not become succeeded.
Audit retention can remove events before their operation record disappears.

## Compose updates

Updates require the Linux Ludock image, administrator access, and trusted source
directories mounted read-only at their **original absolute Docker-host paths**:

```yaml
services:
  ludock:
    environment:
      LUDOCK_COMPOSE_ROOTS: /srv/game-stacks
    volumes:
      - /srv/game-stacks:/srv/game-stacks:ro
```

**Server → Update** discovers the project directory, ordered Compose files, and
CLI environment files from Docker labels. With no recorded CLI environment files,
it loads the project's `.env` if present. All transitive reads must remain within
approved roots and reject symlinks; each source file is limited to 2 MiB.
Shell-only interpolation variables must be supplied through the source files.

- Local Compose and literal service `env_file` inputs are snapshotted before use.
- `include`, `extends`, secrets/configs, `label_file`, credential specs, lifecycle
  hooks, providers, development configuration, and `volumes_from` are rejected.
- Build-only services, digest-pinned updates, multiple replicas, and service
  namespace dependencies such as `network_mode: service:...` are unsupported.
- Source changes after confirmation or during execution abort the operation.
  Retry against current sources. Owner configuration is never rewritten.

For private registries, mount a dedicated read-only `LUDOCK_DOCKER_CONFIG`
directory outside game file roots. If source metadata or original paths are
unavailable, update through the owning manager.

**Update server** pulls the configured image, optionally takes a stopped-server
backup, and recreates only that service, without builds or another pull.
**Recreate anyway** does the same when the image is unchanged. Skipping a backup
requires typing the server name. Initially stopped servers remain stopped.
Image currency does not establish the game version downloaded by startup scripts.

Pull failure leaves the existing container unchanged. After recreation failure,
inspect and recover the service through its owning manager. A data backup cannot
roll back the image or Compose definition; redeployment can replace runtime changes
not saved in that definition. Standalone containers use their original manager.

## Schedules and alerts

Schedules run start/stop/restart/backup at a local time on selected weekdays.
Operators manage their own schedules; administrators manage all eligible-server
schedules. Editing/resuming requires both schedule management and the action's
grant, including the owner's current access. Pausing remains possible after losing
an action grant. Changed data bindings require recreating the schedule.

Missed times and spring-forward gaps are skipped; a repeated fall-back time runs
once. Resuming does not replay missed runs. Pausing/editing blocks queued work under
old settings but does not undo dispatched actions. Check **Last result → View
activity** for the actual operation outcome. Limits are 100 schedules per server
and 1,000 per installation, including paused schedules. Invalid configurations are
suspended; logs identify affected schedules.

Availability monitoring is disabled by default. An administrator can enable it per
server for a 24/7 expectation, with a default two-minute failure grace period.
It checks Docker health or running state, not player connectivity. Maintenance and
Ludock operations suppress alerts; intentionally stopped servers stay suppressed
until observed running again. Docker outages report unknown availability.

Administrators configure Discord in Settings. The webhook is write-only; blank
replacement input preserves it. Save an enabled configuration, then **Send test
notification**. The queue checks every 15 seconds and persists across restarts.
Events cover outages/recoveries, backup/schedule failures, and restore/update results.

**Recent deliveries** shows sanitized status and failure reasons. Delivery retries
with bounded backoff up to five attempts. After correcting configuration, **Retry**
uses the saved webhook and permits five more attempts while retaining the total
count. Disabling Discord pauses the queue, tests, and retries. Interruption after
delivery but before acknowledgement can cause a repeated message.

## Application backup and account recovery

### Back up application data

Stop Ludock and copy all of `/data`, including the `.identity-key` directory beside
the database (normally `/data/ludock.db.identity-key/`). Restore the database and
key together. The key protects identity/source fingerprints and queued API-token
credentials; missing, unsafe, or mismatched keys prevent startup. Recover a lost
key from an earlier backup instead of generating a replacement. Game archives
remain separate: copying `/data` does not back up game worlds.

```bash
docker compose stop ludock
docker cp "$(docker compose ps --all --quiet ludock):/data/." ./ludock-data.backup
docker compose start ludock
```

### Inspect logs

Use administrator **Diagnostics**, **Audit log**, and **Ludock logs**, or
`docker compose logs --follow --tail 200 ludock`. Enable `LOG_LEVEL=debug`
temporarily if needed; exclude credentials and command contents from reports.

### Recover an administrator account

Securely set/export `LUDOCK_RECOVERY_PASSWORD` without putting it in shell history:

```bash
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD \
  ludock bun packages/backend/dist/recovery.js admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the existing username. Recovery revokes its sessions;
it does not create an account or reset game data.

## API

HTTP routes use `/api/v1`; WebSockets use `/ws/v1`. Server IDs are logical UUIDs.
Browser clients use session cookies; the optional administrator token uses
`Authorization: Bearer …`. Never place credentials in URLs.

Long-running mutations return an operation. Poll `/operations/:id` for its outcome
or `/servers/:id/operations` for recent server history. `GET /operations` searches
accessible servers; administrator-only `GET /audit` searches audit history.
Both accept `limit` (up to 250), `cursor`, `serverId`, `actor`, `action`, `status`,
`from`, and `to`. Dates are inclusive Unix milliseconds. `actor` matches an account
name or recorded actor identifier; `action` searches the operation kind/audit action.
Audit also accepts `operationId`. Pass `nextCursor` with unchanged filters until
it is null. Every page and operation detail rechecks current access.

`GET /attention` returns authorized summaries and `discoveryUnavailable` when
current Docker state cannot be checked. `GET /integrations` supplies the capability
registry shown in Diagnostics.
