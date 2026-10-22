# Operations

How to deploy and run Ludock. For a first install, start with the
[README's quick start](../README.md#quick-start).

## Deployment

Run one Ludock per Docker host with [`compose.yaml`](../compose.yaml). It keeps
its own data in `/data` and game backups in `/backups`, in separate volumes;
keep both private.

- **Docker:** Ludock uses the mounted Unix socket and needs Engine API 1.44 or
  later. Remote Docker connections aren't supported. For
  [rootless Docker](https://docs.docker.com/engine/security/rootless/tips/),
  mount its socket at `/var/run/docker.sock`.
- **Settings:** optional deployment settings are in
  [`.env.example`](../.env.example). Copy it to `.env`, uncomment what you need,
  and run `docker compose up -d --force-recreate ludock`.
- **Existing data:** Ludock refuses to start on an unrelated database or an
  unsupported schema, and leaves it unchanged.

### Remote access

Access to the Docker socket gives control of the whole host, even read-only.
For remote access, put an HTTPS reverse proxy in front of Ludock and expose only
the proxy. The proxy must keep the public host name, forward the original
protocol, support WebSocket upgrades and server-sent events without buffering,
and overwrite any client-supplied `X-Forwarded-Host`, `X-Forwarded-Proto`,
`X-Forwarded-Port`, and `X-Forwarded-For` headers.

Set `LUDOCK_TRUSTED_PROXIES` to the proxies' IP addresses or CIDRs, separated by
commas (for example `172.18.0.2,2001:db8::2`). Ludock then reads each client's
address from `X-Forwarded-For`, so sign-in limits apply per client and the audit
log records real addresses; without it, everyone behind the proxy shares one
limit. Trust only proxy addresses, never a subnet shared with untrusted
containers. Ludock reads the chain right to left and stops at the first
untrusted address. Host names and addresses with ports aren't accepted, and an
invalid value prevents startup.

## Adding servers

Ludock decides which containers to manage in this order:

1. **`ludock.enable`:** `"true"` adds the container and `"false"` leaves it out.
   Any other value leaves it out and is reported in **Diagnostics**.
2. **Compose one-off containers** are left out unless labeled.
3. **Supported game images** are added; other images are left out. A
   `ludock.game` label alone doesn't add an unsupported image.

Images are recognized by the end of the repository name, ignoring registry,
tag, and digest, so private mirrors work. Recognition picks the console
integration; it doesn't verify the image. To add another image, label it in the
Compose service that owns it:

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
| `ludock.address` | Exact address players connect to, such as `mc.example.com`, `play.example.com:30000`, or `[2001:db8::1]:2456` |
| `ludock.console` | Console adapter: `minecraft-rcon`, `source-rcon`, `rust-webrcon`, `telnet-console`, or `stdin-console`; `disabled` or `none` turns commands off |
| `ludock.console.host` / `ludock.console.port` | Direct console address, or the console port inside the container |
| `ludock.console.password-env` | **Name** of the game container's environment variable that holds the console password, never the password itself |
| `ludock.files` | Comma-separated container folders for file access and backups; empty turns both off |

### Supported games

**Diagnostics → Supported games** shows the same list, defined in
[`internal/games/presets.go`](../internal/games/presets.go). The console port is
where Ludock sends commands, not the port players use.

| Game | Recognized images | Console | Console port |
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

## Servers

The server list shows each server's state, the address players connect to, and
its latest backup. **Overview** adds uptime, CPU and memory use, the next
scheduled action, and availability monitoring.

States follow Docker. A server with a health check shows **Starting** until it
passes and **Unhealthy** while it fails. A stopped server shows **Crashed** after
an error exit, **Out of memory** when the kernel killed it, and
**Force-stopped** when Docker killed it (exit 137) because it didn't shut down
in time, so recent progress may not have been saved.

### Stop and restart

Stops and restarts run as operations in **Activity**. If the game's console has
a **Save world** shortcut, Ludock sends it and waits five seconds first; if the
console isn't available, the stop goes ahead. Docker then allows 120 seconds to
shut down, or 180 for ARK, Palworld, Valheim, and 7 Days to Die; a longer
`stop_grace_period` on the container wins. If Ludock restarts during a stop, it
waits for that stop to finish without sending it again, for at least five
minutes or the grace period plus a minute.

With permission to create backups, the Restart dialog offers **Take a backup
first**. Ludock checks the backup destination before stopping anything, keeps
the server stopped while the backup runs, then starts it once. Starting can
update the game, so if the backup fails the server stays stopped and Ludock
sends a notification; review the operation, then start it yourself.

### Addresses

Ludock combines the host in **Settings → Server address** with the host port
published for the game's own port and protocol; until an administrator sets a
host, it uses the one in your browser's address bar. A supported game that
doesn't publish its game port shows no address, and other images use their
first published port. Ports published only on loopback are never offered.

When detection can't know the address, such as behind a port-forwarding router,
a tunnel, or a Minecraft SRV record, set `ludock.address` to exactly what
players type. The port is optional, changing it doesn't need a binding review,
and invalid values are ignored and reported in **Diagnostics**.

## Consoles

A server's console shows its Docker logs, its game console, and, for
administrators, a shell inside the container. Shortcut buttons send common
commands; ones that need more input, such as a message, fill in the command
line instead. Up and Down step through commands sent during the visit; nothing
is stored.

Network consoles (RCON, WebRCON, and Telnet) need the console enabled in the
game and its password in an environment variable named by
`ludock.console.password-env`. Each command runs in a short-lived helper
container that shares the game container's network and connects on
`127.0.0.1`, so the console port doesn't need to be published. The password
reaches the helper over standard input. Set `ludock.console.port` for a
non-default port. `ludock.console.host` makes Ludock connect directly instead;
these protocols aren't encrypted, so point it only at a trusted host or an
authenticated tunnel.

- **Minecraft** runs `rcon-cli` inside the game container, so no RCON port is
  published. The image must provide `rcon-cli`, `/bin/sh`, `sleep`, `mkdir`,
  `rm`, and `rmdir`.
- **Terraria** reads commands from the process's input, which needs
  `stdin_open: true`, `StdinOnce` off, and an image that forwards input to the
  server.
- **The administrator shell** needs `/bin/sh` and `timeout` in the image. Each
  command gets 60 seconds, plus five to force it to stop.

## Files

Writable bind mounts and local named volumes that are safe to expose become file
folders, even while the server is stopped. Ludock shows container paths, never
host paths or the container's own writable layer. To narrow access, list folders
in `ludock.files`, such as `"/data/worlds,/data/config"`; the label can't add
mounts.

Ludock blocks read-only, system, and sensitive paths and Docker sockets; it
never follows symlinks (deleting one removes only the link), won't delete a
top-level folder, and refuses hard links in downloads and overwrites and
symlinks or special files in folder downloads. Nested mounts you've excluded
are hidden, and their parent folders can't be downloaded, deleted, or renamed.

Stop a game before replacing a world or configuration it has open. Uploads are
written to a temporary file and replace the destination only once complete, so
allow space for both.

| Limit | Value |
| --- | --- |
| Upload size | 2 GiB; set `LUDOCK_MAX_UPLOAD_SIZE`, such as `500 MB` or `1.5 GiB` |
| Text preview | 512 KiB; larger or binary files are downloaded instead |
| Folder listing | 10,000 entries |
| Upload or download | 30 minutes; uploads also stop after 60 seconds without data |
| Other file actions | 55 seconds |

File work runs in short-lived helper containers that mount only approved data,
run as root, and have no network. They're removed afterward. Any left by an
interruption are removed when Ludock starts again, before other work runs, and
otherwise exit after 35 minutes. Bind mounts need validated host paths and
recursive read-only bind support; symlinked sources and Docker Desktop path
aliases can prevent access. Named volumes must use the `local` driver without
options that remap the host path.

## Backups

### Storage

With the example deployment, Ludock turns on backup storage by itself when
`/backups` is the only approved root, passes the same checks as a manual save,
and doesn't overlap game data. The defaults are 10 backups per server, a 100 GiB
total limit, and 5 GiB of free disk space kept in reserve; change them in
**Settings → Backup storage**. Once settings are saved, including turning
storage off, Ludock never sets them up again. Turning on storage doesn't create
backups or schedules.

To keep backups on another disk, replace the `/backups` volume with a host
folder that is writable and separate from game data:

```yaml
services:
  ludock:
    volumes:
      - /srv/ludock-backups:/backups
```

If a custom container host name stops Ludock from verifying the mount, set
`LUDOCK_SELF_CONTAINER`.

### How backups work

- **Downtime:** a backup stops the server for the whole copy, then returns it to
  its previous state. Live backups aren't supported.
- **Readiness:** **Backups** checks configuration, space, and other containers
  writing the same data without stopping anything. Every backup checks again
  before it runs.
- **Space:** a new archive needs room before old ones are removed. The reserve
  applies while copying and while staging a restore on the game data's disk.
- **Cancelled copies:** a forced stop (exit 137 or out of memory), a start from
  another tool, a replaced container, or another running container writing the
  same data cancels the copy.
- **Contents:** data folders must be separate directories without nested
  mounts; links, special files, single-file mounts, and overlapping folders are
  rejected. Archives hold up to 100,000 entries and 64 folder levels; use
  `ludock.files` to pick smaller folders. Permissions, owners, and modification
  times are kept; ACLs, extended attributes, and sparse files aren't.
- **Records:** each archive's record lives in Ludock's database, so back up
  archives and the database together. Importing other archives isn't
  supported.

### Restore

An administrator picks a backup and types the server's name to confirm. Ludock
checks the archive and that each disk has room for its folders above the
reserve, then stops the server and takes a safety backup; if that fails,
nothing is replaced. Each folder is staged and swapped in with a journal, so an
interrupted restore is finished or rolled back when Ludock starts again.
Replacing several folders is **not atomic**. The server returns to its previous
state only once its data is known to be safe.

If recovery fails, leave the server stopped and check **Activity**, the
container in Docker, and its owning manager's logs.

> [!WARNING]
> Don't delete `.ludock-restore-*` folders or operation records after a failed
> restore; they may be needed to recover the previous game data.

## Compose updates

Minecraft, Palworld, Counter-Strike 2, Conan Exiles, Rust, and Valheim images
download the latest game files when they start, so every **Restart** updates the
game, as **Update** notes. Choose **Take a backup first** to back up before it.

**Update server** pulls a newer image and recreates the service. It needs the
Linux Ludock image, an administrator, and the Compose project folders mounted
read-only at their **original absolute paths on the Docker host**:

```yaml
services:
  ludock:
    environment:
      LUDOCK_COMPOSE_ROOTS: /srv/game-stacks
    volumes:
      - /srv/game-stacks:/srv/game-stacks:ro
```

Ludock finds the project folder, its Compose files, and its environment files
from Docker's labels, using the project's `.env` if none were recorded. Every
file must be inside an approved folder, not a symlink, and under 2 MiB.
Variables that exist only in your shell must be added to those files.

- **Update server** pulls the image, optionally takes a backup first, and
  recreates only that service. **Recreate anyway** does the same when the image
  hasn't changed.
- Skipping the backup requires typing the server's name. A stopped server
  stays stopped.
- Ludock snapshots the files first and stops if they change. It never edits
  them; recreated containers get a `ludock.compose.source` label recording
  where their files are.
- For private registries, mount a read-only folder for `LUDOCK_DOCKER_CONFIG`
  outside game file folders.

Not supported: `include`, `extends`, secrets, configs, `label_file`, credential
specs, lifecycle hooks, providers, development settings, `volumes_from`, and the
`!reset` and `!override` tags (anchors and `<<` merge keys work); build-only
services, images pinned by digest, multiple replicas, and services sharing
another service's namespace; and containers not started by Compose or missing
their Compose labels. Update those through the tool that created them.

If the pull fails, the container is unchanged. If recreation fails, recover the
service with its owning manager; a data backup can't roll back the image or the
Compose file.

## Schedules

Schedules start, stop, restart, or back up a server at a time on chosen days,
or once at a date and time. They default to your browser's time zone, and
**Create paused** saves one without running it.

- **Backups before restarting:** a restart schedule can back up first, like
  **Take a backup first**. It needs backup access, and the server must be
  running when it runs.
- **Countdown warnings:** a restart schedule can announce the restart through the
  game console, at 10, 5, and 1 minutes before by default or up to ten times you
  choose from 1 to 60 minutes. It needs console access and a game with a
  broadcast command: Minecraft, ARK, Counter-Strike 2, Project Zomboid, Conan
  Exiles, Rust, 7 Days to Die, or Terraria. Warnings are best effort: each is
  tried at most once and is skipped when the server is stopped or busy, the
  console fails, or it can't start at least 30 seconds before the restart.
- **Who:** operators manage their own schedules and administrators manage all
  of them. A schedule runs with its owner's current access. Editing or resuming
  needs schedule access and access to the action; pausing doesn't.
- **Timing:** missed times and skipped daylight-saving hours don't run, a
  repeated hour runs once, and resuming doesn't replay missed runs. Editing or
  pausing stops queued work from the old settings.
- **One-time schedules:** the date and time must be in the future and exist in
  the time zone. If Ludock is down at that time and back more than five minutes
  late, the run is recorded as missed and never runs late.
- **Limits:** 100 schedules per server and 1,000 in total, including paused
  ones. When a server's binding changes, recreate its schedules.

## Availability alerts

Monitoring is off by default. An administrator turns it on from a server's
**Overview**. It follows Docker's running state and health check, not whether
players can connect.

- **Grace period:** how long to wait before reporting an outage; 120 seconds by
  default.
- **Quiet times:** stops and operations started through Ludock don't alert, and
  a stopped server stays quiet until it runs again. **Maintenance mode** pauses
  alerts without stopping the server or its schedules.
- **Docker outages** show availability as unknown.

### Discord

In Discord, open **Server Settings → Integrations**, create a webhook for the
alert channel, and copy its URL
([Discord's guide](https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks)).
In Ludock, paste it in **Settings**, turn delivery on, save, and choose **Send
test notification**. Ludock never shows the saved URL again; leaving the field
blank keeps it.

Alerts cover outages and recoveries, failed backups and schedules, and restore
and update results. The queue checks every 15 seconds and survives restarts.
Failed deliveries retry up to five times, and **Retry** allows five more.
Turning delivery off pauses the queue. A restart right after a delivery can
send a message twice.

## Users and access

| Role | Access |
| --- | --- |
| Administrator | Every server and setting |
| Operator | Shared servers, with any of: logs, file downloads, start, stop, restart, console, changing files, creating backups, and managing their own schedules |
| Viewer | Shared servers, with logs and file downloads if shared |

Operators and viewers see a server only once it's shared with them. Changing
files also needs file downloads, and a backup permission includes the temporary
stop it needs but not general start and stop. Restoring, downloading or
deleting archives, updating, the container shell, and settings are for
administrators. Removing access also blocks the user's queued work and closes
their open consoles and log streams.

### Server identity

A Compose server is identified by its project, service, and replica, and a
standalone container by its name. Recreating a container keeps its history and
sharing; renaming a standalone container makes a new server. If a server's
game, mounts, or project change, non-administrators lose access until an
administrator chooses **Accept binding**; its schedules then need to be
recreated, and older backups restore only if they match the accepted data.

### History

**Operations** lists past and current work, and administrators can follow each
to its **Audit log** events. Audit entries record the outcome at that moment
and can expire before their operation. **Needs attention** includes failures
among each server's latest 100 operations. During a Docker outage, history
stays readable and controls are disabled.

## Maintenance

### Back up Ludock

Stop Ludock and copy all of `/data`, including the identity key folder beside
the database (`/data/ludock.db.identity-key/`). Restore the database and key
together; a missing or mismatched key prevents startup, so recover a lost key
from a backup rather than creating a new one. Copying `/data` doesn't back up
game worlds.

```bash
docker compose stop ludock
docker cp "$(docker compose ps --all --quiet ludock):/data/." ./ludock-data.backup
docker compose start ludock
```

### Logs

Administrators can use **Diagnostics**, **Audit log**, and **Ludock logs**, or
run `docker compose logs --follow --tail 200 ludock`. Set
`LUDOCK_LOG_LEVEL=debug` temporarily for more detail.

### Recover an administrator account

Export `LUDOCK_RECOVERY_PASSWORD` without saving it in your shell history, then:

```bash
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD ludock recover admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the existing username. This signs the account out
everywhere; it doesn't create accounts.

## Health check

`GET /api/v1/health` needs no sign-in. It answers 200 with
`{"status":"ok","docker":"connected","database":"connected"}` while the database
and Docker are reachable, and 503 with `{"status":"degraded"}` otherwise. The
image's `HEALTHCHECK` runs `ludock healthcheck` against it. Everything else is
the browser interface; Ludock has no other HTTP API.
