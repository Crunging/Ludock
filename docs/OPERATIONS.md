# Operations

How to deploy and run Ludock. For a first install, start with the
[README's quick start](../README.md#quick-start).

## Deployment

Run one Ludock per Docker host with [`compose.yaml`](../compose.yaml). It keeps
its own data in `/data` and game backups in `/backups`, in separate volumes;
keep both private. It needs no other configuration: everyday options are in
**Settings**, and the upload size limit, verbose logging, and reverse proxy are
under **Settings → Advanced**. Options for unusual setups are environment
variables, listed in [Advanced deployment](#advanced-deployment).

- **Docker:** Ludock uses the mounted Unix socket and needs Engine API 1.44 or
  later. Remote Docker connections aren't supported. For
  [rootless Docker](https://docs.docker.com/engine/security/rootless/tips/),
  mount its socket at `/var/run/docker.sock`.
- **Existing data:** Ludock refuses to start on an unrelated database or an
  unsupported schema, and leaves it unchanged.
- **First administrator:** until one exists, the setup page asks for the
  one-time code Ludock prints to its logs at startup (or `LUDOCK_SETUP_CODE`).
  The code has no deadline. A generated code is replaced at each restart until
  setup is done; a configured one stays the same. Failed attempts are limited
  per client address.

### Remote access

Access to the Docker socket gives control of the whole host, even read-only.
For remote access, put an HTTPS reverse proxy in front of Ludock and expose only
the proxy. The proxy must keep the public host name, forward the original
protocol, support WebSocket upgrades and server-sent events without buffering,
and overwrite any client-supplied `X-Forwarded-Host`, `X-Forwarded-Proto`,
`X-Forwarded-Port`, and `X-Forwarded-For` headers.

Then trust the proxy in **Settings → Advanced → Reverse proxy**. When you open
Settings through a proxy at a private or local address and no proxy is trusted
yet, that section opens by itself, names the proxy's address, and offers to
trust it in one click; you can also enter IP addresses or CIDRs separated by commas, such as
`172.18.0.2, 2001:db8::/64`. Ludock then reads each client's address from
`X-Forwarded-For`, so sign-in limits apply per client and the audit log and
session list show real addresses; without it, everyone behind the proxy shares
one limit. A trusted address can claim to be any client, so trust only your
proxies, never a subnet shared with untrusted containers. Ludock reads the
chain right to left and stops at the first untrusted address. Host names and
addresses with ports are refused when you save. Changing the list never signs
anyone out; `LUDOCK_TRUSTED_PROXIES` sets it from outside the panel instead.

## Adding servers

Servers on [supported game images](#supported-games) appear by themselves. For
any other container, such as an ich777, LinuxGSM, or custom image, open
**Missing a server?** under the server list. It lists the containers Ludock can
see but doesn't manage, with their image and published ports; pick the game if
Ludock didn't guess it, and choose **Add**. Nothing changes in Docker or the
container's Compose file. Like every new server, it's visible only to
administrators until you share it.

To take a server out, open it and choose **Remove from Ludock**, whether it was
added by hand or found by itself. The container keeps running as it was. Its
sharing, schedules, monitoring settings, and backups are kept, and come back if
you add it again. Until then it leaves the server list, and its scheduled runs
don't happen.

Ludock remembers these choices in its own database, by Compose project, service,
and replica, or by container name for a standalone container, so they survive
the container being recreated. It decides which containers to manage in this
order:

1. **`ludock.enable` label:** `"true"` adds the container and `"false"` leaves
   it out, whatever the panel says. Any other value leaves it out and is
   reported in **Diagnostics**.
2. **Compose one-off containers** are left out unless labeled.
3. **The panel:** a container added there is managed, and one removed there is
   left out.
4. **Supported game images** are added; other images are left out. A
   `ludock.game` label alone doesn't add an unsupported image.

The panel never offers Ludock itself, its helper containers, update
replacements, or containers that can control the host: privileged containers
and ones given a Docker, Podman, or containerd socket, the host's root, or
`/run`. Their console and files would hand over the whole host, so add one only
with a label, deliberately.

Images are recognized by the end of the repository name, ignoring registry,
tag, and digest, so private mirrors work. Recognition picks the console
integration; it doesn't verify the image.

### Labels

Labels are the advanced way to add or adjust a server, in the Compose service
that owns it:

```yaml
stdin_open: true
labels:
  ludock.enable: "true"
  ludock.game: "terraria"
  ludock.name: "Terraria with friends"
```

| Label | Purpose |
| --- | --- |
| `ludock.enable` | Add (`true`) or leave out (`false`) a container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Game integration, such as `minecraft` or `terraria` |
| `ludock.address` | Exact address players connect to, such as `mc.example.com`, `play.example.com:30000`, or `[2001:db8::1]:2456` |
| `ludock.console` | Console adapter: `minecraft-rcon`, `source-rcon`, `rust-webrcon`, `telnet-console`, or `stdin-console`; `disabled` or `none` turns commands off |
| `ludock.console.host` / `ludock.console.port` | Direct console address, or the console port inside the container |
| `ludock.console.password-env` | **Name** of the game container's environment variable that holds the console password, never the password itself |
| `ludock.query` | Status query: `minecraft-ping` or `a2s`; `disabled` or `none` turns it off. Naming a protocol enables it for any game, with `ludock.query.port` |
| `ludock.query.port` | The status query port inside the container |
| `ludock.secret-env` | Comma-separated **names** of more environment variables whose values are hidden from logs and consoles. Names that look like credentials, such as `RCON_PASSWORD`, `DB_PASS`, `API_KEY`, or `ACCESS_CODE`, and passwords inside URLs are hidden already |
| `ludock.files` | Comma-separated container folders for file access and backups; empty turns both off |

### Supported games

**Diagnostics → Supported games** shows the same list, defined in
[`internal/games/presets.go`](../internal/games/presets.go). The console port is
where Ludock sends commands, not the port players use. The status query port is
where Ludock asks the game whether it answers; when the image documents a
variable for it, such as `SERVER_PORT` or `QUERY_PORT`, its value wins.

| Game | Recognized images | Console | Console port | Status query |
| --- | --- | --- | --- | --- |
| Minecraft | `itzg/minecraft-server` | Minecraft RCON | 25575 | Server List Ping, 25565 (`SERVER_PORT`) |
| Factorio | `factoriotools/factorio` | Source RCON | 27015 | — |
| Palworld | `thijsvanloef/palworld-server-docker`, `jammsen/palworld-dedicated-server` | Source RCON | 25575 | — |
| ARK: Survival Evolved | `hermsi/ark-server`, `hermsi1337/ark-server`, `indifferentbroccoli/ark-server-docker` | Source RCON | 27020 | Steam, 27015 (`SERVER_LIST_PORT`, `QUERY_PORT`) |
| ARK: Survival Ascended | `sknnr/ark-ascended-server`, `mschnitzer/asa-linux-server` | Source RCON | 27020 | — |
| Counter-Strike 2 | `joedwards32/cs2` | Source RCON | 27015 | Steam, 27015 (`CS2_PORT`) |
| Project Zomboid | `renegademaster/zomboid-dedicated-server`, `renegade-master/zomboid-dedicated-server`, `renegade_master/zomboid-dedicated-server` | Source RCON | 27015 | — |
| Conan Exiles | `indifferentbroccoli/conan-exiles-enhanced-server-docker` | Source RCON | 25575 | Steam, 27015 (`QUERY_PORT`) |
| V Rising | `trueosiris/vrising` | Source RCON | 25575 | — |
| Rust | `didstopia/rust-server` | WebRCON | 28016 | — |
| 7 Days to Die | `vinanrra/7dtd-server` | Telnet | 8081 | — |
| Valheim | `community-valheim-tools/valheim-server`, `lloesche/valheim-server` | None | — | Steam, 2457 |
| Terraria | `hexlo/terraria-server-docker`, `beardedio/terraria`, `ryshe/terraria` | Process stdin | — | — |
| Minecraft: Bedrock Edition | `itzg/minecraft-bedrock-server` | Process stdin | — | — |
| Satisfactory | `robtme/satisfactory-server`, `wolveix/satisfactory-server` | None | — | — |
| Enshrouded | `sknnr/enshrouded-dedicated-server`, `mornedhels/enshrouded-server` | None | — | Steam, 15637 (`SERVER_QUERYPORT`, `PORT`) |
| Don't Starve Together | `jamesits/dst-server` | None | — | — |
| Core Keeper | `escaping/core-keeper-dedicated` | None | — | — |
| Vintage Story | `zsuatem/vintagestory`, `devidian/vintagestory` | Process stdin | — | — |

Steam is Valve's A2S_INFO server query. Valheim answers it only when the server
is public (`SERVER_PUBLIC`). The others aren't queried by default: Factorio has
its own protocol, ARK: Survival Ascended no longer answers server queries,
Rust's query port depends on how the image passes its ports, and the Palworld,
Project Zomboid, V Rising, and 7 Days to Die images don't document a query port.
Minecraft: Bedrock Edition and Satisfactory have their own protocols, and the
Don't Starve Together and Core Keeper images don't document a query port either.
If yours answers one, set `ludock.query: a2s` and `ludock.query.port`.

## Servers

The server list shows each server's state, the address players connect to, and
its latest backup. **Overview** adds uptime, CPU and memory use, disk space for
its data folders, the next scheduled action, and availability monitoring.

States follow Docker. A server with a health check shows **Starting** until it
passes and **Unhealthy** while it fails. A stopped server shows **Crashed** after
an error exit, **Out of memory** when the kernel killed it, and
**Force-stopped** when Docker killed it (exit 137) because it didn't shut down
in time, so recent progress may not have been saved.

### Stop and restart

Stops and restarts run as operations in **Activity**. If the game's console has
a **Save world** shortcut, Ludock sends it and waits five seconds first; if the
console isn't available, the stop goes ahead. Docker then allows 120 seconds to
shut down, or 180 for ARK, Palworld, Valheim, and 7 Days to Die and 360 for
Don't Starve Together; a longer `stop_grace_period` on the container wins. If
Ludock restarts during a stop, it waits for that stop to finish without sending
it again, for at least five minutes or the grace period plus a minute.

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
first published port, as does Core Keeper, whose port is optional. Minecraft:
Bedrock Edition connects on TCP 19132; a server on the older UDP transport
needs `ludock.address`. Ports published only on loopback are never offered.

When detection can't know the address, such as behind a port-forwarding router,
a tunnel, or a Minecraft SRV record, set `ludock.address` to exactly what
players type. The port is optional, and invalid values are ignored and reported in **Diagnostics**.

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

- **Minecraft** uses `RCON_PASSWORD` when it is set. Otherwise Ludock reads
  the password the image saved in `/data/.rcon-cli.env`, including one it
  generated, through Docker without running anything in the container. RCON
  must be on, as it is by default (`ENABLE_RCON`).
- **Terraria, Minecraft: Bedrock Edition, and Vintage Story** read commands
  from the process's input, which needs `stdin_open: true`, `StdinOnce` off,
  and an image that forwards input to the server. Replies appear only in the
  logs.
- **The administrator shell** needs `/bin/sh` and `timeout` in the image. Each
  command gets 60 seconds, plus five to force it to stop.

### Players online

The server list and the server's overview show who is online for these games,
and how many for any game with a status query (see
[Supported games](#supported-games)):

| Game | Command | Console |
| --- | --- | --- |
| Minecraft, or any server with `ludock.console: minecraft-rcon` | `list` | RCON |
| Factorio | `/players online` | RCON |
| Palworld | `ShowPlayers` | RCON |
| ARK: Survival Evolved and Survival Ascended | `ListPlayers` | RCON |
| Project Zomboid | `players` | RCON |
| Conan Exiles | `ListPlayers` | RCON |
| 7 Days to Die | `listplayers` | Telnet |

Ludock sends the command about every 30 seconds while the server runs, and again
right after a console command, using the same helper and password as the
console. Only Minecraft reports how many players a server takes. Conan Exiles
shows character names. Anyone who can see the server sees the player count and
names. Readings stay in memory and are never stored. When the console is off,
doesn't answer, or answers with something other than a player list, the server's
players show as unavailable, never as empty. Other games, including those whose
console reads the process's input, which returns no replies, show no player
list. A reading gives way to anything else that needs the server, such as a
console command or a backup.

When the game also has a status query, Ludock asks it in the same helper. A
console list wins, since it names players; the query's count shows when the
console isn't set up or can't list them, so games without a console password
still show how many are online. A query gives counts only, never names: the
names Minecraft sends are a sample that plugins often change, and Ludock
doesn't send Valve's separate player query. The Steam count leaves out bots.

#### Kicking, banning, and the allowlist

Someone who can send console commands can click an online player's name on the
server's overview to kick or ban them; a ban asks first. On Minecraft,
**Allowlist and bans** below the overview lists who is allowed in (the game
calls it the whitelist) and who is banned, adds and removes allowed players by
name, and lifts bans. Whether the allowlist is on is the server's own
`white-list` setting, which Ludock shows but never changes.

| Game | Kick | Ban | Allowlist and ban list | Player is named by |
| --- | --- | --- | --- | --- |
| Minecraft | `kick` | `ban` | `whitelist add`, `whitelist remove`, `pardon` | Name: letters, digits, `_`, up to 16 |
| Factorio | `/kick` | `/ban` | | Name: letters, digits, `.`, `_`, `-`, up to 60 |
| Palworld | `KickPlayer` | `BanPlayer` | | The Steam ID `ShowPlayers` lists |
| ARK: Survival Evolved and Survival Ascended | `KickPlayer` | `BanPlayer` | | The Steam or EOS ID `ListPlayers` lists |
| 7 Days to Die | `kick` | | | The entity ID `listplayers` lists |

Each action is one console command with the same access, checks, and audit as
a typed one; the audit log records who did what to which player. A kick or ban
goes to a player in the latest list of who's online, by the name or ID that list
gave. Ludock sends a name or ID only when it is wholly in the game's own form,
so it can't carry spaces, quotes, line breaks, or a selector such as `@a`, and
refuses anything else. Before a Minecraft kick or ban, Ludock also sends
`list uuids` and goes ahead only when its account names are exactly the names
`list` showed, so a nickname, team prefix, or plugin's list never stands in for
another player; on such a server, use the console. Project Zomboid and Conan Exiles get no actions: Zomboid's names can hold
spaces its commands need quoted, and Conan's kick takes a row number that can
change between readings. 7 Days to Die bans need a length, so they stay in the
console. Ludock reads Minecraft's lists from `whitelist.json`,
`banned-players.json`, and `server.properties` in `/data`, where the itzg image
keeps them.

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

To unpack a zip, tar, or tar.gz archive, such as a world or mod pack you
uploaded, choose **Unpack** from its menu. It's offered for files named like
archives (`.zip`, `.tar`, `.tar.gz`, `.tgz`, `.mcworld`, `.mcpack`,
`.mcaddon`, `.mrpack`); Ludock reads the content to tell the formats apart, so
a `.zip` that's really a tar still unpacks. The contents go into a new folder beside the
archive: the archive's only folder when it has exactly one, such as `world/`,
otherwise a folder named after the archive. If that name is taken, Ludock adds
a number, such as `world (2)`; unpacking never replaces anything. Ludock
treats every archive as untrusted: entries with absolute paths, `..`, control
characters, or backslashes refuse the whole archive; links, hard links, and
special files are left out; and new files and folders get the same
permissions and owner as an upload. Unpacking works in a hidden staging folder
and keeps the free space set as **Minimum free space** under backup settings.
A refused or interrupted unpack removes its staging folder. Ludock records
each staging folder before unpacking, so one left when Ludock stopped is
removed the next time it starts; after three starts that can't remove it,
Ludock logs a warning and stops trying.

Choose **Select** to pick several entries of one folder, then download them as
one tar archive, move them into another folder of the same storage location,
or delete them. Moves follow the rules for renames: they never replace a name,
cross into or out of a nested mount, or move a symbolic link.

A storage location's ID in a page address comes from its container path, so a
page opened before the server's mounts or `ludock.files` label changed can
only act on the folder it showed.

Users who can change files can also edit a previewed text file and save it.
A save is written like an upload and keeps the file's permissions, owner, line
endings, and byte order mark. If the file changed after the editor opened, the
save is refused and the text stays in the editor. Files that mix line endings
are download-only, since a browser can't keep them.

| Limit | Value |
| --- | --- |
| Upload size | No limit, but an upload must leave 1 GiB free on the server's disk (a twentieth of a disk under 20 GiB); set a limit in **Settings → Advanced** |
| Text preview and editing | 512 KiB; larger or binary files are downloaded instead |
| Unpacking an archive | 20 GiB, 100,000 entries, 64 nested folders, and 100 times the archive's size (plus 1 MiB); 30 minutes |
| Selection | 500 entries of one folder |
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

### What Ludock keeps

After each new backup, Ludock removes the server's backups it no longer
needs. It keeps:

- the 3 newest backups;
- the last backup of each of the 7 most recent days with a backup;
- the last of each of the 4 most recent weeks with one;
- the last of each of the 6 most recent months with one;
- every backup an administrator keeps.

A nightly schedule leaves about 14 backups reaching back five months. Only
days, weeks, and months that have backups count, so backups taken rarely or
only by hand are never removed for being old. Days follow Ludock's time zone
(`TZ`). Safety backups taken before a restore or an update follow the same
rule and are labeled in the list.

To keep a backup for good, such as one before a big update, choose
**Keep…** on it in **Backups** and add a note if you like. Retention never
removes a kept backup and it can't be deleted until **Release** is chosen;
kept backups still use disk space, and the list and **Settings** show how
much. Retention never touches a backup being written, restored from,
browsed, or downloaded.

### Storage

With the example deployment, Ludock turns on backup storage by itself in the
first approved root (`/backups`) when it passes the same checks as a manual
save and doesn't overlap game data. **Settings → Backup storage** shows where
backups go, how much space they and kept backups use, and the free space left.
Once settings are saved, including turning storage off, Ludock never sets them
up again. Turning on storage doesn't create backups or schedules.

Under **Advanced** are the backup folder, a minimum of free space to leave on
the backup disk and on game-data disks during restores (5 GiB by default),
and an optional total limit for all backups, which is off by default.

To keep backups on another disk, replace the `/backups` volume with a host
folder that is writable and separate from game data:

```yaml
services:
  ludock:
    volumes:
      - /srv/ludock-backups:/backups
```

Ludock finds its own container to verify the mount. If it says it can't, set
`LUDOCK_SELF_CONTAINER` (see [Advanced deployment](#advanced-deployment)).

### Second copy

If the backup disk or the whole host is lost, its backups go with it. To keep
a second copy of every backup somewhere else, such as a NAS share, a USB disk,
or a folder another tool syncs to the cloud, mount that folder at
`/backups-copy`:

```yaml
services:
  ludock:
    volumes:
      - /mnt/nas/ludock:/backups-copy
```

Run `docker compose up -d --force-recreate ludock`, then choose
`/backups-copy` under **Settings → Backup storage → Advanced → Also copy
backups to**. It must be writable and separate from the backup folder and from
game data. Choosing it also copies the backups you already have.

Advanced: any other folder listed in `LUDOCK_BACKUP_ROOTS` after the first,
such as `LUDOCK_BACKUP_ROOTS=/backups:/mnt/usb`, can be chosen the same way.

- **When:** each backup is copied after its server is back as it was, so the
  copy adds no downtime. The copy is written under a temporary name, read back
  and checked against the original's size and checksum, and only then named
  `<id>.tar`, the same plain tar as the original.
- **Problems:** a copy that can't be made, because the folder is missing,
  read-only, full down to the free-space minimum, or stops responding, never
  fails the backup. Ludock tries again after 1, 5, and 30 minutes and then
  every 6 hours, shows **Second copy failed** and the reason on the backup,
  and sends one *Failed backups and schedules* notification if the third try
  fails. Saving the backup settings again tries at once. A copy to a share that
  hangs is given up after 2 minutes without progress, and nothing else is
  written there until the hung request returns.
- **Removal:** the second folder needs no limits of its own. When retention or
  an administrator removes a backup, its copy is removed too, and kept backups
  keep theirs. A copy in a folder that is away is removed once it is back.
- **Changing the folder:** choosing another second folder copies every backup
  there again; until its copy is made, a backup shows **Second copy waiting**,
  and the copy in the old folder still stands in for a missing archive. Once
  the new copy is made, the old one is left where it is and Ludock no longer
  tracks or removes it. Turning the second copy off keeps existing copies
  tracked, so they are still used and removed with their backups.
- **Using the copy:** if a backup's archive is missing from the backup folder,
  downloading, browsing, and restoring read the copy, after the same checks,
  and **Backups** says so. If the backup disk is lost, choose the second folder
  as the backup folder and restore from the copies. Archives Ludock has no
  record of can't be imported.

### How backups work

- **Downtime:** a backup stops the server for the whole copy, then returns it to
  its previous state. Live backups aren't supported.
- **Readiness:** **Backups** checks configuration, space, and other containers
  writing the same data without stopping anything. Every backup checks again
  before it runs.
- **Space:** a new archive needs room before old ones are removed. The reserve
  applies while copying and while staging a restore on the game data's disk.
  If the next backup might not fit, Ludock removes the server's oldest
  backups first, never its 3 newest or kept ones; if there still isn't room,
  the backup fails, saying how much kept backups use.
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
- **Browsing:** administrators choose **Browse** on a backup to look through
  it folder by folder, with sizes and modification times, and download single
  files from it. Browsing reads the archive and changes nothing. Archives can
  hold configuration secrets, so browsing and downloads are for administrators
  only, like downloading a whole archive.

### Restore

An administrator picks a backup and types the server's name to confirm. Ludock
checks the archive and that each disk has room for its folders above the
reserve, then stops the server and takes a safety backup; if that fails,
nothing is replaced. Each folder is staged and swapped in with a journal, so an
interrupted restore is finished or rolled back when Ludock starts again.
Replacing several folders is **not atomic**. The server returns to its previous
state only once its data is known to be safe.

To get back one file or folder instead, browse the backup, choose
**Restore…** on that item, and type the server's name. Ludock checks the whole
archive and that the item's folder still exists and can be reached without
links or other disks, then stops the server, moves the current copy aside,
puts the backup's copy in its place, and returns the server to its previous
state. Everything else is left as it is. If the swap fails or Ludock restarts
midway, the current copy is put back. No safety backup is taken, and once the
restore succeeds the replaced copy is deleted, so back up first if you might
want it again. A backup made under a different server configuration can be
browsed but not restored.

If recovery fails, leave the server stopped and check **Activity**, the
container in Docker, and its owning manager's logs.

> [!WARNING]
> Don't delete `.ludock-restore-*` folders or operation records after a failed
> restore; they may be needed to recover the previous game data.

## Image updates

Minecraft (both editions), Palworld, Counter-Strike 2, Conan Exiles, Rust,
Valheim, Satisfactory, Enshrouded, Don't Starve Together, and Core Keeper images
download the latest game files when they start, unless the image's own settings
pin a version or skip updates, so every **Restart** updates the game, as
**Update** notes. Choose **Take a backup first** to back up before it.

**Update server** pulls a newer version of the server's image and recreates its
container with the same settings. It needs an administrator and nothing else:
Ludock works through the Docker socket alone, whichever tool created the
container. **Back up first** is ticked when you may back up; skipping it asks
you to type the server's name. Ludock confirms before it starts.

- Ludock pulls the image tag the container was created from. If the image
  hasn't changed, nothing else happens; **Advanced → Update even if the image
  is already current** recreates the container regardless.
- It stops the server, optionally takes a backup, and creates the new
  container beside the stopped original. The new container keeps the original's
  environment, command, ports, networks, static addresses, restart policy,
  mounted folders, and volumes, including unnamed ones, so their data carries
  over. Settings that only repeated the old image's defaults come from the new
  image instead.
- A running server's new container must start cleanly: become healthy if the
  image has a healthcheck, or otherwise keep running for 10 seconds. Only then
  does Ludock remove the original and give the new container its name and
  restart policy. Until then neither container restarts on its own, so a Docker
  restart can't run both. A stopped server stays stopped.
- If the new container stops, restarts, or reports itself unhealthy, Ludock
  removes it and restores the original, but leaves it stopped: the new version
  may already have changed the game data. Check the operation, restore the
  backup if needed, then start the server.
- Ludock never edits Compose files or other definitions. Compose recognizes the
  new container as its own, so a later `docker compose up` doesn't recreate it.
  The new container's `ludock.update` label names the update that created it.
  A change you make to the definition still applies the next time you run your
  own tool.
- Files changed inside the container but outside its mounts and volumes are
  lost, as with any recreation.
- For private registries, set `LUDOCK_DOCKER_CONFIG` (see
  [Advanced deployment](#advanced-deployment)).

Not supported: images pinned by digest or ID, containers Docker removes when
they stop (`--rm`), Swarm services, and containers whose network, process, or
IPC namespace another container shares. Update those through the tool that
created them.

### Newer image checks

About every six hours, Ludock asks each image tag's registry, through Docker's
Engine API, what the tag points to now, without pulling it. It asks once per
tag however many servers use it, and spreads the tags out over time. When a
server's container runs an older version, administrators see **Image update
available** on the server list and a notice on its **Update** tab, and each
notification destination that chose **Newer images available** under **Alert
types** is told once per server and version. Nothing updates
until an administrator chooses **Update server**. **Check now** on the Update
tab asks right away, at most once every 10 seconds.

- Ludock compares the digest the registry gives for the tag with the digests
  Docker recorded when it pulled the container's image. For a multi-platform
  tag both are the tag's index. A tag that no longer offers the server's
  platform isn't reported. On Docker's classic image store, a change to
  another platform alone can show as newer; if **Update server** then finds the
  image current, the notice clears.
- Only the servers **Update server** supports are checked. Ludock can't compare
  an image Docker has no registry digest for, such as one built or loaded
  locally, and says so on the Update tab.
- A registry that can't be reached or refuses the check isn't an alert: the
  Update tab says the last check failed, and Ludock tries again within about an
  hour. Private registries use `LUDOCK_DOCKER_CONFIG`, as pulls do. Docker Hub
  counts each check against its pull rate limit.
- For images that download the game when they start, the notice is about the
  container image, such as its startup scripts and system packages; **Restart**
  still updates the game.
- **Settings → Image updates** turns the checks off; they are on by default.

## Schedules

Schedules start, stop, restart, or back up a server, or send a command to its
game console, at a time on chosen days or once at a date and time. A new
schedule starts as a daily restart at 08:00 in your browser's time zone,
backing up first when backup storage is set up, the server has data folders,
and you may back up. The time zone, waiting for an empty server, and countdown
warnings are under **More options**. To keep a new schedule from running,
**Pause** it in the list.

- **Backups before restarting:** a restart schedule can **Back up first**, like
  **Take a backup first**. It needs backup access, and the server must be
  running when it runs. If the backup fails, the server stays stopped.
- **Countdown warnings:** a restart schedule can announce the restart through the
  game console, at 10, 5, and 1 minutes before by default or up to ten times you
  choose from 1 to 60 minutes. It needs console access and a game with a
  broadcast command: Minecraft (both editions), ARK, Counter-Strike 2, Project
  Zomboid, Conan Exiles, Rust, 7 Days to Die, Terraria, or Vintage Story.
  Warnings are best effort: each is tried at most once and is skipped when the
  server is stopped or busy, the console fails, or it can't start at least 30
  seconds before the restart.
- **Waiting for an empty server:** a restart or stop can **Wait until no one
  is online**, on servers whose players Ludock can read. When players are
  online at the scheduled time, the run waits, up to 1 to 360 minutes (30 by
  default) from that time, and goes ahead as soon as nobody is online; a
  stopped server counts as empty. While it waits, the schedule list, the
  server's overview, and its Activity tab say until when, and Ludock checks
  who is online about every 30 seconds. If players are still online when the
  wait ends, the run is skipped or goes ahead anyway, as chosen. A count
  Ludock can't read, such as while the console doesn't answer, counts as
  players online, and a skip for that reason sends a notification. A restart
  that backs up first waits before the backup. Countdown warnings need **Go
  ahead anyway** and count down to the end of the wait. The wait survives a
  Ludock restart; one that ended more than five minutes before Ludock came
  back is recorded as missed. Pausing, editing, or deleting the schedule
  ends the wait, and the run rechecks the owner's access when it acts.
- **Console commands:** **Send command** takes one line of up to 500
  characters, such as `save-all` or `say Restarting at noon`, and sends it
  through the game console as if typed there, never through the shell. It
  needs console access and a server with a game console. A run is skipped
  when the server isn't running or is busy with other work; it doesn't start
  the server or wait, and a send interrupted by a Ludock restart is recorded
  as interrupted, never repeated. The schedule list shows whether the command was sent
  and, to people with console access, the start of the game's reply, with
  secrets redacted. Anyone who can see the schedule can read its command, so
  leave passwords out.
- **Who:** members with **Full access** to a server manage their own
  schedules and administrators manage all of them. A schedule runs with its owner's current access. Editing or resuming
  needs schedule access and access to the action; pausing doesn't.
- **Timing:** missed times and skipped daylight-saving hours don't run, a
  repeated hour runs once, and resuming doesn't replay missed runs. Editing or
  pausing stops queued work from the old settings.
- **One-time schedules:** the date and time must be in the future and exist in
  the time zone. If Ludock is down at that time and back more than five minutes
  late, the run is recorded as missed and never runs late.
- **Limits:** 100 schedules per server and 1,000 in total, including paused
  ones. Schedules survive changes to the container; see
  [Server identity](#server-identity).

## Availability alerts

Every server is monitored from the moment Ludock finds it: add a
[notification destination](#notifications) to hear about problems. Monitoring
follows Docker's running state and health check, for games with a status query
whether the game itself answers, and the server's disk space and memory. A
server already stopped when Ludock first finds it stays quiet until it runs.
An administrator can turn a server's monitoring off, or **Pause alerts for
maintenance**, on its **Overview**; the levels below are under **Alert
levels**.

- **Game not answering:** Ludock asks a monitored game about every 30 seconds
  while someone has Ludock open and every 2 minutes otherwise, from a
  short-lived helper that shares the game's network and connects to
  `127.0.0.1`, so the query port doesn't need publishing; Ludock never queries
  other hosts. A game that answered since its container started and then stops
  answering while the container runs is an outage after the wait before alerting,
  even if a health check that asks the game also fails,
  reported as **Game not answering**, and it recovers when the game answers
  again. A game that hasn't answered yet in this container run, such as one
  still starting or whose query port is wrong or off, never alerts. Ludock
  keeps answers in memory, so after Ludock restarts a game must answer once
  before its silence counts again; an outage already reported stays until the
  game answers. The
  overview's **Game** entry shows whether the game is answering, hasn't
  answered yet, or isn't checked. A reply that isn't a valid status, including
  Minecraft's pre-1.7 legacy ping and split Steam replies, counts as no
  answer.

- **Disk nearly full:** about every 10 minutes while someone has Ludock open,
  and every 30 minutes otherwise, Ludock measures the free and total space of
  the filesystem under each data folder of a monitored server, even while it's
  stopped, from a short-lived read-only file helper. It alerts when a folder's
  disk reaches the alert level, 90% full by default, and measures that folder
  every minute (every 5 with nobody watching) until it falls 5 points below the
  level.
  **Overview** shows each folder's disk, by container path, to people who can
  read its files. A measurement that fails, such as for a mount the file
  helpers can't reach, shows as not measured and neither raises nor clears an
  alert.
- **Memory near its limit:** for a running container with a memory limit,
  Ludock alerts when every minute's sample stays at the alert level, 90% of the
  limit by default, for 5 minutes, and the alert ends once memory falls 5
  points below the level or the server stops. Containers without a limit
  never alert.
- **Wait before alerting:** how long an outage lasts before it's reported; 120
  seconds by default.
- **Settings:** each alert can be turned off or given its own level on the
  server's **Overview**. Changing an alert's settings ends its current alert
  without a recovery message, and a disk is measured again on the next check.
- **Quiet times:** stops and operations started through Ludock don't alert, and
  a stopped server stays quiet until it runs again, though its disk still
  alerts. Disks and memory aren't checked while an operation runs.
  **Pause alerts for maintenance** pauses alerts without stopping the server or
  its schedules.
- **Docker outages** show availability as unknown, and disks and memory keep
  their last state.

### Notifications

**Settings → Notifications** sends alerts to Discord, ntfy, Gotify, Telegram,
email (SMTP), or a generic webhook. For each destination you use, fill in its
card, turn it on, save, and choose **Send test notification**. Tokens, topics,
passwords, and webhook URLs are never shown again; leave a saved field blank to
keep it. For Discord, create a webhook for the alert channel under **Server
Settings → Integrations**
([Discord's guide](https://support.discord.com/hc/en-us/articles/228383668-Intro-to-Webhooks)).

Each destination's **Alert types** choose what it gets:

- **Server problems:** a server is unavailable, its game stops answering, or
  Ludock can't reach Docker to check it; a nearly full disk or memory near its
  limit; and each of their recoveries.
- **Failed backups and schedules:** a backup fails, a scheduled action fails or
  can't run, or a server is left stopped after its backup failed.
- **Restores and updates:** a restore or update finishes, fails, or finds the
  image already current.
- **Newer images available:** a newer version of a server's image is
  published.

A destination gets every type except newer images until you choose otherwise,
since popular images publish often and the server list and **Update** tab
already show them. A type a destination doesn't get is never queued for it,
and **Send test notification** works whatever is chosen. Each disk or memory alert is sent once, and
names the server and, for a disk, the data folder's container path, never a
host path. An image alert names the server, not its image, since an image
reference can name a private registry. Each destination gets its own delivery. The queue checks
every 15 seconds and survives restarts. A delivery gets up to five attempts,
and **Retry** allows five more. Turning a destination off pauses its
deliveries. A restart right after a delivery can send a message twice.

## Users and access

There are two kinds of account. **Administrators** use every server and manage
users and settings. **Members** see only the servers shared with them, and
what they can do is chosen per server. A new server is shared with nobody.

### Share a server with a friend

Open the server and choose **Sharing**. Pick someone who already has an
account, or **Someone new…** and type a username, choose their access, and
choose **Share**. For someone new, Ludock makes the account and shows a
one-time invite link to copy and send; they open it, choose a password, and
land on the servers shared with them. The same tab lists everyone the server
is shared with: change their access and choose **Save**, or **Stop sharing**.
Changes take effect right away, including for pages, consoles, and log
streams they have open, and work they queued.

| Access | What they can do |
| --- | --- |
| Can see it | See whether it's running, who is online, and the address to join |
| Can run it | Also start, stop, and restart it, use the game console, and read its logs |
| Full access | Also read and change its files, create backups, and set up their own schedules |

Creating a backup stops a running server for the copy and starts it again.
Restoring, downloading, or deleting backups, updates, the container shell,
monitoring, settings, and adding or removing servers stay with
administrators.

**Custom** sets exactly which of those actions someone has, for communities
that need something in between. The server's Sharing tab shows such access as
Custom. Changing files always includes reading them.

**Users** lists everyone with what each can use, and is where you add an
account without sharing a server yet, make someone an administrator or a
member, disable or delete an account, make a reset link, or turn off
two-factor sign-in. Making someone an administrator drops their per-server
sharing, so making them a member again starts with nothing shared; their
schedules stay paused until they have access again. Ludock always keeps at
least one administrator who can sign in.

### Invite and reset links

Sharing a server with someone new always makes an invite link, and on
**Users** a new account gets one by default too: Ludock shows a one-time link
to copy and send, and whoever opens it chooses the password and is signed in.
The account can't sign in until then. For a forgotten password,
**Reset link** makes a link that sets a new one and signs the user out
everywhere else. To type a password instead, choose **Set a password now**
when creating the user, or **Set password** on their row.

A link works once, expires after 24 hours, and is shown only when it's made;
Ludock stores only a hash of it. The user's row shows a link waiting to be
used, with **Revoke link**. Making a new link, disabling or deleting the
account, or changing its password some other way also ends it. Links point at
the address you're using Ludock at, so make them from the address users will
open. Opening a link that doesn't work says only that, and failed attempts
count toward the same per-address limit as sign-ins.

A reset link doesn't get around two-factor sign-in: after the new password,
the user still enters a code from their authenticator app or a recovery code.
If they lost both, choose **Turn off two-factor** first.

### Two-factor sign-in

Anyone can turn on two-factor sign-in in **Account**: scan the QR code with an
authenticator app (or type the setup key), then enter the current password
and a code from the app. Signing in then asks for a 6-digit code after the
password; codes from the previous or next 30 seconds are accepted, and each
works once. Ludock shows ten recovery codes once when two-factor sign-in turns
on; each works once in place of a code. Wrong codes count toward the same
sign-in limits as wrong passwords, and an account is locked out of the code
step for 15 minutes after 10 wrong codes.

Turning two-factor sign-in on or off asks for the current password and signs
out the account's other sessions, like a password change. For a lost phone,
an administrator can choose **Turn off two-factor** on **Users**, which signs
that user out everywhere, or use `ludock recover` (below).

### Server identity

A Compose server is identified by its project, service, and replica, and a
standalone container by its name. Recreating or changing a container in your
own tools keeps its history, sharing, and schedules: a new password, port,
console setting, label, or environment variable is simply picked up. Renaming
a standalone container makes a new server.

Only a change to the server's game or to the data folders Ludock reads, backs
up, and restores asks for anything. Administrators keep full use of the
server. If it's shared with anyone else, their access and their schedules
pause until an administrator opens the server and chooses **Confirm and
restore access**, since what they were given has changed. Backups taken
before the change restore only onto the same data folders.

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
run `docker compose logs --follow --tail 200 ludock`. For more detail, such as
for a bug report, turn on **Verbose logging** in **Settings → Advanced**; it
turns itself off after 24 hours.

### Recover an administrator account

Export `LUDOCK_RECOVERY_PASSWORD` without saving it in your shell history, then:

```bash
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD ludock recover admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the existing username. This also turns off the account's
two-factor sign-in and signs it out everywhere; it doesn't create accounts.

## Advanced deployment

Most installations never need these. Set one under `environment:` in Ludock's
Compose file and run `docker compose up -d` to apply it:

```yaml
services:
  ludock:
    environment:
      LUDOCK_SENSITIVE_PATHS: /srv/private
```

Ludock refuses to start when a value is invalid and names the variable. The
first three also appear in **Settings → Advanced**; a value set here wins, and
the panel shows the setting as fixed by the deployment.

| Variable | Default | For |
| --- | --- | --- |
| `LUDOCK_MAX_UPLOAD_SIZE` | No limit | The largest uploaded file, such as `500 MB` or `4 GiB` |
| `LUDOCK_LOG_LEVEL` | `info` | `error`, `warn`, `info`, or `debug`, kept until removed |
| `LUDOCK_TRUSTED_PROXIES` | None | Reverse proxies, as IP addresses or CIDRs separated by commas |
| `LUDOCK_BACKUP_ROOTS` | `/backups` and `/backups-copy` when mounted | Only for other layouts: approved backup folders, separated by `:`; empty turns backups off |
| `LUDOCK_SELF_CONTAINER` | Detected | Ludock's own container name or ID, if Ludock says it can't find it |
| `LUDOCK_SETUP_CODE` | Printed at start | Your own first-administrator setup code, 32 to 128 characters; remove it after setup |
| `LUDOCK_DOCKER_CONFIG` | None | A read-only folder, outside game file folders, with a Docker `config.json` whose inline `auths` entries reach private registries |
| `LUDOCK_SENSITIVE_PATHS` | None | More container or host paths to block in file access, separated by `:` |
| `LUDOCK_HELPER_IMAGE` | Ludock's own image | Another image for file and backup helpers, pinned by digest |
| `LUDOCK_DOCKER_SOCKET` | `/var/run/docker.sock` | The Docker socket's path inside Ludock, if you mount it elsewhere |
| `LUDOCK_LISTEN` | `:3000` | The address inside the container; change the published port in `compose.yaml` instead |
| `LUDOCK_DATA_DIR` | `/data` | The folder for Ludock's database and its identity key |
| `LUDOCK_RECOVERY_PASSWORD` | None | Only for [recovering an administrator account](#recover-an-administrator-account) |

## Health check

`GET /api/v1/health` needs no sign-in. It answers 200 with
`{"status":"ok","docker":"connected","database":"connected"}` while the database
and Docker are reachable, and 503 with `{"status":"degraded"}` otherwise. The
image's `HEALTHCHECK` runs `ludock healthcheck` against it. Everything else is
the browser interface; Ludock has no other HTTP API.
