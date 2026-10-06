# Operations

How to deploy and run Ludock day to day. For a first install, start with the
[README's quick start](../README.md#quick-start).

## Deployment

Run one Ludock per Docker host, using [`compose.yaml`](../compose.yaml). It keeps
Ludock's data in `/data` and game backups in `/backups`, in separate volumes;
keep both private.

- **Docker:** Ludock talks to Docker through the mounted Unix socket and needs
  Engine API 1.44 or later. Remote Docker connections aren't supported. For
  [rootless Docker](https://docs.docker.com/engine/security/rootless/tips/), mount
  its socket at `/var/run/docker.sock`.
- **Settings:** optional deployment settings are listed in
  [`.env.example`](../.env.example). Copy it to `.env`, uncomment what you need,
  and recreate Ludock:

  ```bash
  docker compose up -d --force-recreate ludock
  ```

- **Existing data:** Ludock refuses to start on an unrelated database or an
  unsupported schema, and leaves it unchanged. Keep that data before switching
  to a fresh volume.

### Remote access

Access to the Docker socket gives control of the whole host, even when it's
mounted read-only. For remote access, put an HTTPS reverse proxy in front of
Ludock and expose only the proxy. The proxy must:

- Keep the public host name and forward the original protocol.
- Support WebSocket upgrades. API requests and WebSockets use same-origin
  session cookies.
- Overwrite any client-supplied `X-Forwarded-Host`, `X-Forwarded-Proto`,
  `X-Forwarded-Port`, and `X-Forwarded-For` headers.

Set `LUDOCK_TRUSTED_PROXIES` to your proxies' IP addresses or CIDRs, separated by
commas (for example `172.18.0.2,2001:db8::2`). Ludock then reads each client's
address from `X-Forwarded-For`, so login and setup limits apply per client and
audit events record the client's address. Without it, everyone behind the proxy
shares one limit.

- Trust only proxy addresses, never a subnet shared with untrusted containers.
- Additional trusted proxies may append the peer address they saw. Ludock reads
  the chain from right to left and stops at the first untrusted address.
- IPv4, IPv6, and CIDRs are accepted; host names and addresses with ports are
  not. An invalid setting prevents startup. Invalid forwarding headers fall back
  to the direct peer address.

## Adding servers

Ludock decides which containers to manage in this order:

1. **`ludock.enable`:** `"true"` adds the container and `"false"` leaves it out.
   Values are trimmed and case-insensitive. Any other value leaves it out and is
   reported in **Diagnostics**.
2. **Compose one-off containers** are left out unless labeled.
3. **Supported game images** are added; any other image is left out. A
   `ludock.game` label alone doesn't add an unsupported image.

Recognition ignores tags, digests, and registry prefixes and matches the end of
the repository name, so private mirrors work. It picks the console integration;
it doesn't verify the image or the game version. To add a custom image, set
labels in the Compose service that owns it:

```yaml
stdin_open: true
labels:
  ludock.enable: "true"
  ludock.game: "terraria"
  ludock.name: "Terraria with friends"
```

### Labels

| Label | Purpose |
| --- | --- |
| `ludock.enable` | Add (`true`) or leave out (`false`) a container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Game integration, such as `minecraft` or `terraria` |
| `ludock.address` | Exact address players connect to, such as `mc.example.com`, `play.example.com:30000`, or `[2001:db8::1]:2456`; replaces the detected address |
| `ludock.console` | Console adapter: `minecraft-rcon`, `source-rcon`, `rust-webrcon`, `telnet-console`, or `stdin-console`; `disabled` or `none` turns commands off |
| `ludock.console.host` / `ludock.console.port` | Console address, when the default isn't right |
| `ludock.console.password-env` | **Name** of the game container's environment variable that holds the console password, never the password itself |
| `ludock.files` | Comma-separated container folders for file access and backups; empty turns both off |

### Supported games

**Diagnostics → Supported games** shows the same list, defined in
[`server-presets.ts`](../packages/backend/src/server-presets.ts). The console
port is where Ludock sends commands, not the port players use.

| Game | Recognized images (repository suffix) | Console | Console port |
| --- | --- | --- | --- |
| Minecraft | `itzg/minecraft-server` | `rcon-cli` in the container | Set inside the container |
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

## Server list and addresses

The server list shows each server's state, the address players connect to, and
its latest backup. **Server → Overview** adds uptime, CPU and memory use, the
next scheduled action, and availability monitoring.

### States

States follow Docker. A running server with a health check shows **Starting**
until the check passes and **Unhealthy** while it fails. A stopped server shows:

- **Crashed** when it exited with an error code.
- **Out of memory** when the kernel killed it.
- **Force-stopped** when Docker killed it (exit 137), usually because it didn't
  shut down within its stop grace period. Recent progress may not have been saved.

### Addresses

Ludock combines the name or IP in **Settings → Server address** with the host
port published for the game's own port and protocol. A supported game that
doesn't publish its own port shows no address; other images use their first
published port. Until an administrator sets a name, Ludock
uses the host name from your browser's address bar.

Ports published only on loopback (such as `127.0.0.1:25565:25565`) are never
offered, because players on other machines can't reach them.

When detection can't know the address, add a `ludock.address` label with exactly
what players type. Use it for a router that forwards a different port, a tunnel
service, a separate domain per game, or a Minecraft SRV record. The port is
optional, and changing the label doesn't require a binding review. Invalid values
are ignored and reported in **Diagnostics**.

## Consoles

Open a server's console for its Docker logs, its game console, and, for
administrators, a shell inside the container.

- **Shortcuts:** buttons for common commands, such as listing players or saving
  the world. Shortcuts that need more input, such as a message, fill in the
  command line instead of sending.
- **History:** Up and Down step through commands sent during the current visit.
  Nothing is stored.

Network consoles (RCON, WebRCON, and Telnet) need a reachable address, the
console enabled in the game, and credentials. These protocols aren't encrypted,
so keep them on a trusted Docker network or behind an authenticated TLS tunnel.
Ludock sends console credentials to `ludock.console.host`, so point it only at an
endpoint you trust.

- **Minecraft** runs `rcon-cli` inside the game container and needs no published
  RCON port. The image must provide `rcon-cli`, `/bin/sh`, `sleep`, `mkdir`,
  `rm`, and `rmdir`.
- **Terraria** reads commands from the process's input. It needs
  `stdin_open: true`, `StdinOnce` turned off, and an image that forwards input
  to the server.
- **Administrator shell** commands need `/bin/sh` and `timeout` in the image.
  Each command has 60 seconds, plus up to five to force it to stop. They work
  even when `/tmp` is read-only, but disconnecting may then wait for that
  deadline. The server stays locked until Docker confirms the command exited.

## Files

Writable bind mounts and local named volumes that are safe to expose become file
folders automatically, even while the server is stopped. Ludock shows container
paths, never host paths or the container's own writable layer. To narrow access,
list folders in a label, such as `ludock.files: "/data/worlds,/data/config"`;
labels can't add mounts.

Ludock blocks:

- Read-only, system, and sensitive paths, and Docker sockets.
- Following symlinks, and deleting a top-level folder. Deleting a symlink removes
  only the link.
- Hard links in downloads and overwrites, and symlinks or special files in
  folder downloads.
- Nested mounts you've excluded. They're hidden, and their parent folders can't
  be downloaded, deleted, or renamed.

Stop a game before replacing a world or configuration it has open. Uploads go to
a temporary file and replace the destination only after the whole transfer
succeeds, so allow space for both. Canceling a batch doesn't undo files that
already finished.

| Limit | Value |
| --- | --- |
| Upload size | 2 GiB; set `MAX_UPLOAD_SIZE`, such as `500 MB` or `1.5 GiB` |
| Text preview | 512 KiB; larger or binary files are downloaded instead |
| Folder listing | 10,000 entries |
| Upload or download | 30 minutes; uploads also stop after 60 seconds without data |
| Other file actions | 55 seconds |

File work runs in short-lived helper containers that mount only approved data,
run as UID 0, and have no network. They're removed afterward, or expire after 35
minutes if Ludock is interrupted. Bind mounts need validated host paths and
recursive read-only bind support; symlinked sources or Docker Desktop path
aliases can prevent access. Named volumes must use the `local` driver without
options that remap the host path. Helper image overrides and custom host names
are covered in [`.env.example`](../.env.example).

## Backups

### Set up storage

With the example deployment, open **Settings → Backup storage**. Ludock suggests
the `/backups` folder; review the defaults and save to turn backups on. Nothing
is enabled until you save. If several folders are configured, pick one.

| Setting | Default |
| --- | --- |
| Backups kept per server | 10 |
| Combined size limit | 100 GiB |
| Minimum free disk space | 5 GiB |

To keep backups on another disk, create a host folder and replace the `/backups`
volume mount with it, keeping the socket and data mounts:

```yaml
services:
  ludock:
    volumes:
      - /srv/ludock-backups:/backups
```

The folder must be mounted, writable, and separate from game data. If a custom
container host name prevents Ludock from verifying the mount, set
`LUDOCK_SELF_CONTAINER`.

### How backups work

- **Downtime:** a backup stops the server for the whole copy, then returns it to
  its previous state. A server that was already stopped stays stopped. Live
  backups aren't supported.
- **Readiness:** **Server → Backups** checks configuration, space, and other
  programs writing the same data, without stopping or copying anything. It
  doesn't reserve space, so every backup checks again before it runs.
- **Space:** a new archive needs room *before* old ones are removed, so a full
  limit may mean deleting backups or raising the limit. The free-space minimum
  applies while copying and while staging a restore on the game data's disk.
- **Invalid copies:** a forced stop (exit 137 or out of memory), a start from
  another tool, a replaced container, or another running program writing the
  same data cancels the copy. Ludock can't lock other managers, so check that the
  game shuts down cleanly and its worlds are intact.
- **Contents:** data folders must be separate directories without nested mounts.
  Links, special files, single-file mounts, and overlapping folders are rejected.
  Archives are limited to 100,000 entries and 64 path levels; use `ludock.files`
  to pick smaller folders. Permissions, owners, and modification times are kept;
  ACLs, extended attributes, and sparse files aren't.
- **Restoring needs Ludock's database:** each archive's records live there, so
  back up archives and the database together. Importing other archives isn't
  supported.

### Restore

An administrator picks a backup and types the server's name to confirm. Ludock
checks the archive, stops the server, and takes a safety backup; if that backup
fails, nothing is replaced. Each folder is staged and then swapped in, with a
journal so interrupted work can be finished or rolled back. Replacing several
mounts is **not atomic**.

The server returns to its previous state only once the data is known to be safe.
If recovery fails or the server's identity can't be confirmed, leave it stopped and check **Server → Activity**, the
container in Docker, and its owning manager's logs.

> [!WARNING]
> Don't delete `.ludock-restore-*` folders or operation records after a failed
> restore; they may be needed to recover the previous game data. Stopping Ludock
> doesn't cancel an operation.

## Compose updates

Updates need the Linux Ludock image, an administrator, and the Compose source
folders mounted read-only at their **original absolute paths on the Docker host**:

```yaml
services:
  ludock:
    environment:
      LUDOCK_COMPOSE_ROOTS: /srv/game-stacks
    volumes:
      - /srv/game-stacks:/srv/game-stacks:ro
```

**Server → Update** finds the project folder, its Compose files in order, and any
environment files from Docker's labels. Without recorded environment files, it
uses the project's `.env` if there is one. Every file it reads must stay inside
approved folders, can't be a symlink, and must be under 2 MiB. Variables that
only exist in your shell must be added to those files.

- **Update server** pulls the configured image, optionally takes a backup first,
  and recreates only that service, without building or pulling again.
- **Recreate anyway** does the same when the image hasn't changed, for example
  to pick up a game update that installs on startup.
- Skipping the backup requires typing the server's name. A server that was
  stopped stays stopped.

Not supported:

- `include`, `extends`, secrets, configs, `label_file`, credential specs,
  lifecycle hooks, providers, development settings, and `volumes_from`.
- Build-only services, images pinned by digest, multiple replicas, and services
  that share another service's namespace, such as `network_mode: service:…`.
- Containers not started by Compose, or whose Compose labels or original paths
  aren't available. Update those through the tool that created them.

Ludock snapshots the files before it starts and stops if they change; retry
against the current files. It never rewrites your configuration. For private
registries, mount a dedicated read-only `LUDOCK_DOCKER_CONFIG` folder outside game
file folders.

If the pull fails, the existing container is unchanged. If recreation fails,
recover the service with its owning manager. A data backup can't roll back the
image or Compose file, and redeploying replaces changes that were never saved in
the Compose file. An up-to-date image doesn't mean the game itself is current,
because many images download the game when they start.

## Schedules

Schedules start, stop, restart, or back up a server at a set time on chosen days.
New schedules use your browser's time zone; search for another by city or choose
**Use my time zone**. **Every day**, **Weekdays**, and **Weekends** select days
in one click, and **Create paused** saves a schedule without running it.

- **Who:** operators manage their own schedules and administrators manage all of
  them. A schedule runs with its owner's current access. Editing or resuming
  needs both schedule access and access to the action; pausing doesn't.
- **Timing:** missed times and skipped daylight-saving hours don't run, and a
  repeated hour runs once. Resuming doesn't replay missed runs. Pausing or editing
  stops queued work under the old settings but doesn't undo work already
  started.
- **Results:** **Last result → View activity** shows what actually happened.
- **Limits:** 100 schedules per server and 1,000 in total, including paused ones.
  A schedule with an invalid setup is suspended, and the logs say which.
- **Server changes:** when a server's data binding changes, recreate its schedules.

## Availability alerts

Monitoring is off by default. An administrator turns it on from a server's
**Overview** when the server should be up around the clock. It checks Docker's
running state and health check, not whether players can connect.

- **Grace period:** how long to wait before reporting an outage. The default is
  120 seconds.
- **Quiet times:** stops and operations started through Ludock don't alert, and
  a stopped server stays quiet until it's running again. **Maintenance mode**
  pauses alerts until you turn it off; it doesn't stop the server or its
  schedules.
- **Docker outages** report availability as unknown.

### Discord

In Discord, open **Server Settings → Integrations**, create a webhook for the
channel that should get alerts, and copy its URL
([Discord's guide](https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks)).
In Ludock, paste it in **Settings**, turn delivery on, save, and choose
**Send test notification**.

Alerts cover outages and recoveries, failed backups and schedules, and restore
and update results. Ludock never shows the saved URL; leaving the field blank
keeps it.

The queue checks every 15 seconds and survives restarts. Failed deliveries retry
up to five times; after fixing the setup, **Retry** allows five more. Turning
Discord off pauses the queue, tests, and retries. **Recent deliveries** shows each
attempt's status without secrets. A restart right after delivery can send a
message twice.

## Users and access

| Role | Access |
| --- | --- |
| Administrator | Every server and setting |
| Operator | Shared servers, with any of: logs, file downloads, start, stop, restart, console, changing files, creating backups, and managing their own schedules |
| Viewer | Shared servers, with logs and file downloads if shared |

Operators and viewers need a server shared with them (`server.view`) before they
can see it. Changing files also needs file read access, and console and logs are
shared separately. A backup permission includes the temporary stop it needs, but
not general start and stop. Restoring, downloading or deleting archives,
updating, the container shell, and system settings are administrator-only.
Removing access also blocks the user's queued work and closes their open consoles
and log streams.

### Server identity

Each server has its own Ludock ID. A Compose server is identified by its project,
service, and replica; a standalone container by its name. Recreating a container
keeps its history and sharing, but renaming a standalone container creates a new
server.

If a server's game, mounts, or project change, non-administrators lose access
until an administrator chooses **Accept binding**. Its schedules then need to be
recreated, and older backups can only be restored if they match the accepted
data.

### Activity and history

- **Operations** lists past and current work. Administrators can follow related
  events in **Audit log**. Filters never unlock controls.
- Audit entries record the outcome at the time, so an old "queued" entry doesn't
  change to "succeeded". Old audit entries can expire before their operation.
- **Needs attention** includes failures from each server's latest 100
  operations, even if a later run succeeded.
- During a Docker outage, history stays readable and controls are disabled until
  Ludock can check live state again.

## Maintenance

### Back up Ludock

Stop Ludock and copy all of `/data`, including the identity key folder next to
the database (normally `/data/ludock.db.identity-key/`). Restore the database and
key together: a missing, unsafe, or mismatched key prevents startup, so recover a
lost key from an earlier backup instead of creating a new one. The key protects
server and Compose source fingerprints and queued API-token credentials. Copying
`/data` doesn't back up game worlds.

```bash
docker compose stop ludock
docker cp "$(docker compose ps --all --quiet ludock):/data/." ./ludock-data.backup
docker compose start ludock
```

### Logs

Administrators can use **Diagnostics**, **Audit log**, and **Ludock logs**, or
run `docker compose logs --follow --tail 200 ludock`. Set `LOG_LEVEL=debug`
temporarily for more detail, and leave credentials and command contents out of
anything you share.

### Recover an administrator account

Export `LUDOCK_RECOVERY_PASSWORD` without saving it in your shell history, then:

```bash
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD \
  ludock bun packages/backend/dist/recovery.js admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the existing username. This signs the account out
everywhere; it doesn't create accounts or change game data.

## API

HTTP routes live under `/api/v1` and WebSockets under `/ws/v1`. Server IDs are
Ludock's IDs, not Docker container IDs. Browsers use session cookies; the optional
administrator token uses `Authorization: Bearer …`. Never put credentials in URLs.

- **Long-running actions** return an operation. Poll `/operations/:id` for its
  result, or `/servers/:id/operations` for a server's recent work.
- **History:** `GET /operations` searches servers you can access, and
  administrator-only `GET /audit` searches the audit log. Both accept `limit`
  (up to 250), `cursor`, `serverId`, `actor`, `action`, `status`, `from`, and
  `to`; audit also accepts `operationId`. Dates are inclusive Unix milliseconds.
  `actor` matches an account name or recorded actor ID, and `action` matches the
  operation kind or audit action. Pass `nextCursor` with the same filters until
  it's null. Every page rechecks your access.
- `GET /attention` returns what needs attention, with `discoveryUnavailable`
  when Docker can't be checked. `GET /integrations` lists the supported games.
