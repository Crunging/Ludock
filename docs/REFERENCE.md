# Reference

Details for looking things up, then options for unusual setups. Everyday tasks
are in the [guide](GUIDE.md).

## Supported games

Containers on these images are added by themselves. **Settings → System** shows
the same list. Images are recognized by the end of the repository name,
ignoring registry, tag, and digest, so private mirrors work; recognizing an
image doesn't verify where it came from.

| Game | Images | Console (port) | Status check (port) |
| --- | --- | --- | --- |
| Minecraft | `itzg/minecraft-server` | Minecraft RCON (25575) | Server List Ping (25565, `SERVER_PORT`) |
| Factorio | `factoriotools/factorio` | Factorio RCON (27015) | Health check |
| Palworld | `thijsvanloef/palworld-server-docker`, `jammsen/palworld-dedicated-server` | Palworld RCON (25575), REST API (8212) for warnings | Health check |
| ARK: Survival Evolved | `hermsi/ark-server`, `hermsi1337/ark-server`, `indifferentbroccoli/ark-server-docker` | ARK RCON (27020) | Steam server query (27015, `SERVER_LIST_PORT`, `QUERY_PORT`) |
| ARK: Survival Ascended | `sknnr/ark-ascended-server`, `mschnitzer/asa-linux-server` | ARK RCON (27020) | — |
| Counter-Strike 2 | `joedwards32/cs2` | Source RCON (27015) | Steam server query (27015, `CS2_PORT`) |
| Project Zomboid | `renegademaster/zomboid-dedicated-server`, `renegade-master/zomboid-dedicated-server`, `renegade_master/zomboid-dedicated-server` | Project Zomboid RCON (27015) | — |
| Conan Exiles | `indifferentbroccoli/conan-exiles-enhanced-server-docker` | Conan Exiles RCON (25575) | Steam server query (27015, `QUERY_PORT`) |
| V Rising | `trueosiris/vrising` | V Rising RCON (25575) | — |
| Rust | `didstopia/rust-server` | Rust WebRCON (28016) | — |
| 7 Days to Die | `vinanrra/7dtd-server` | 7 Days to Die Telnet (8081) | — |
| Valheim | `community-valheim-tools/valheim-server`, `lloesche/valheim-server` | — | Steam server query (2457) |
| Terraria | `hexlo/terraria-server-docker`, `beardedio/terraria`, `ryshe/terraria` | Terraria console (server input) | — |
| Minecraft: Bedrock Edition | `itzg/minecraft-bedrock-server` | Bedrock console (server input) | Health check |
| Satisfactory | `robtme/satisfactory-server`, `wolveix/satisfactory-server` | — | — |
| Enshrouded | `sknnr/enshrouded-dedicated-server`, `mornedhels/enshrouded-server` | — | Steam server query (15637, `SERVER_QUERYPORT`, `PORT`) |
| Don't Starve Together | `jamesits/dst-server` | — | — |
| Core Keeper | `escaping/core-keeper-dedicated` | — | — |
| Vintage Story | `zsuatem/vintagestory`, `devidian/vintagestory` | Vintage Story console (server input) | Health check |

The console port is where Ludock sends commands, inside the container. The
status check asks the game whether it answers; when the image documents a
variable for its port, listed after the port, that variable's value wins.
The Steam server query is Valve's A2S, which Valheim answers only when the
server is public: with `SERVER_PUBLIC` off, the server's page says the check is off.
If your server answers one that isn't listed, set `ludock.query: a2s` and
`ludock.query.port`.

A supported game without a status check of its own uses its image's Docker
health check, when the image has one: healthy is answering, and unhealthy
after it was healthy since the container started is not answering, which
alerts and can restart the server like a status check. Docker calls a
container unhealthy only after its check fails several times in a row (about
90 seconds by default). A health check sees only what it tests: Factorio's
and Bedrock's ask the game, but Palworld's and Vintage Story's only check
that the process exists, so a hung server still passes them. Health check is
listed for the images known to have one; no health check means no claim.
`ludock.query: none` turns this off too.

Games on server input (Terraria, Bedrock, Vintage Story) print their replies
in their own output rather than answering a command. Ludock types the list
command and reads the reply from the log lines that follow it; other lines
in between are skipped, a reply that never comes counts as unknown, and only
the names are kept. While a page shows the server, the command and its reply
appear in its log about every 30 seconds.

What each game gets beyond start, stop, files, and backups:

| Game | Who's online | Kick and ban | Warnings to players | Restart updates the game |
| --- | --- | --- | --- | --- |
| Minecraft | Names | Kick, ban, allowlist | ✓ | ✓ |
| Factorio | Names | Kick, ban | ✓ | |
| Palworld | Names | Kick, ban | ✓ | ✓ |
| ARK: Survival Evolved | Names | Kick, ban | ✓ | |
| ARK: Survival Ascended | Names | Kick, ban | ✓ | |
| Counter-Strike 2 | Count | | ✓ | ✓ |
| Project Zomboid | Names | | ✓ | |
| Conan Exiles | Character names | | ✓ | ✓ |
| Rust | | | ✓ | ✓ |
| V Rising | | | ✓ | ✓ |
| 7 Days to Die | Names | Kick | ✓ | |
| Valheim, Enshrouded | Count | | | ✓ |
| Terraria | Names | Kick, ban | ✓ | |
| Vintage Story | Names | Kick | ✓ | |
| Minecraft: Bedrock Edition | Names | Kick | ✓ | ✓ |
| Satisfactory, Don't Starve Together, Core Keeper | | | | ✓ |

Names need the console's password; a count comes from the status check. "Restart updates the game" holds unless the
image's own settings pin a version or skip updates.

Project Zomboid and Conan Exiles get no kick or ban buttons: Zomboid's names
can hold spaces its commands need quoted, and Conan's kick takes a row number
that can change. Terraria and Bedrock offer them only for names without
spaces. 7 Days to Die and Vintage Story bans need a length, and Bedrock has no
ban command, so those stay in the console. Palworld's warnings go over its
REST API, which keeps the spaces RCON's `Broadcast` drops; they're offered
while `REST_API_ENABLED` (`RESTAPI_ENABLED` on jammsen's image) is on, both
images' default.

### Checked on real servers

These games were run on their real images, under Compose, with Ludock beside
them. Players joined Minecraft Java and Terraria, with scripted clients that
need no account; on the others, who's online was read on an empty server, and
the replies with players come from the game's own text. The rest follow
their images' documentation.

- **Minecraft** (`itzg/minecraft-server`, vanilla 1.21.11): with zero, one,
  and two players online, Ludock opened RCON, which the game logs, only when
  a page opened or someone joined; the status check's player sample vouched
  for the list the rest of the time.
- **Terraria** (`hexlo/terraria-server-docker`, 1.4.5.8): its console needs
  `stdin_open: true`, with or without `tty`. Ludock saves with `save` before a
  stop and then types `exit`, since the images never pass Docker's stop
  signal on to the game. `say` warns players. Files and backups take the
  worlds folder Ludock finds mounted. With two players online, Ludock listed
  both from `playing`, and kicked and banned them; the list follows within a
  few seconds. A ban also bans the player's address, and the ban list lives
  in `/root/.local/share/Terraria/banlist.txt`, outside the worlds folder, so
  backups don't include it. The image has no health check, so Terraria gets
  no automatic restart. `ryshe/terraria` and `beardedio/terraria` were only
  inspected: both also keep the game from Docker's stop signal.
- **Factorio** (`factoriotools/factorio`): the console works by itself with the
  password the image generates. Who's online, kick, and ban answer as
  expected; warnings appear in chat from `<server>`; `/server-save` saves, and
  the game saves again when it stops. The image's health check asks the game
  over RCON, so a hung game is not answering and can be restarted; that was
  checked on Bedrock, not again on Factorio.
- **Valheim** (`lloesche/valheim-server`): no console. The game saves its world
  when it stops. Files and backups take `/config`, not the server the image
  downloads into `/opt/valheim`. The count and the status check need
  `SERVER_PUBLIC` on, its default, which lists the server publicly; with it
  off, the page says the status check is off for that setup, and automatic
  restart isn't offered. With it on, Ludock restarted a hung server by itself
  once automatic restart was turned on.
- **Vintage Story** (`zsuatem/vintagestory`): its console needs
  `stdin_open: true`. `/autosavenow` saves before a stop, `/announce` warns
  players, and the game saves again when it stops. Ludock reads who's online
  from `/list clients`, and kicks with `/kick`. Its health check only checks
  that the server process exists. `devidian/vintagestory` wasn't run.
- **Minecraft: Bedrock Edition** (`itzg/minecraft-bedrock-server`): its console
  needs `stdin_open: true`. The image saves the world when Docker stops it.
  From 1.26.50 the server takes players on TCP 19132 only, so publish
  `19132:19132/tcp` for its address to show. Ludock reads who's online from
  `list` and kicks with `kick`; Bedrock has no `ban`. Its health check asks
  the game: with the game process frozen, Docker called it unhealthy within
  two minutes, and Ludock restarted it and said it answered again. Joining to
  see warnings needs a Microsoft account, so that wasn't checked.
- **Palworld** (`thijsvanloef/palworld-server-docker`, 1.0.5): its console needs
  `RCON_ENABLED=true` and `ADMIN_PASSWORD`. Who's online, kick, and ban answer
  as expected; kick and ban send `steam_<ID>`, the only form Palworld 1.0
  takes. `Save` saves before a stop. Warnings go to the REST API's announce,
  which the server logged as accepted; seeing them in game needs the game.
  Files and backups take the saves in `/palworld/Pal/Saved`, about 0.3 MB on
  a new world, not the 3.5 GB server download around them; a backup and its
  restore each took about 15 s. `ludock.files: "/palworld"` takes the whole
  folder again.
- **Core Keeper** (`escaping/core-keeper-dedicated`): no console or status
  check, and no address, since the game connects through Steam's relay
  unless `SERVER_PORT` is set. The game saves its world when Docker stops it,
  within a few seconds. Files and backups take the world in
  `/home/steam/core-keeper-data`, about 0.1 MB on a new world, not the
  570 MB server download in `/home/steam/core-keeper-dedicated`. A backup
  and its restore brought back the same world.
- **V Rising** (`trueosiris/vrising`, 1.1): its console needs RCON on, with
  `HOST_SETTINGS_Rcon__Enabled=true` and `HOST_SETTINGS_Rcon__Password`, the
  variable Ludock reads the password from. `announce` warns players: the game
  took Ludock's restart warning. RCON has no save command or player list, so
  who's online isn't shown. Docker's stop ends the game without saving, so a
  stop or backup keeps its last autosave, every 2 minutes by default
  (`AutoSaveInterval` in `ServerHostSettings.json`); the game's own
  `shutdown` saves but waits at least a minute. Files and backups take
  `/mnt/vrising/persistentdata`, about 20 MB, not the 1.9 GB server in
  `/mnt/vrising/server`. A backup and its restore brought back the same
  saves, and the game loaded the latest.
- **Enshrouded** (`sknnr/enshrouded-dedicated-server` and
  `mornedhels/enshrouded-server`): no console. The status check and the count
  answer on 15637. The game saves its world when Docker stops it, within a
  few seconds. With sknnr's image, files and backups take the `savegame`
  volume; mornedhels' keeps the 8 GB server download in `/opt/enshrouded`
  with the world inside it, and files and backups take only its default
  `/opt/enshrouded/server/savegame`; with another `SERVER_SAVE_DIR`, name it
  in `ludock.files`. On both, a backup and its restore
  brought back the same world.
- **7 Days to Die** (`vinanrra/7dtd-server`, 3.4): the image leaves
  `TelnetPassword` empty in `sdtdserver.xml`, so the game takes Telnet
  without a password, only from inside its container, where Ludock connects;
  a password set there is read from that file. Who's online comes from
  `listplayers`, read on an empty server; `saveworld` saves before a stop,
  and `say` warns players: the game posted Ludock's message in global chat.
  The image stops the game through LinuxGSM, which saves and shuts it down
  in about 10 seconds. Files and backups take the saves in
  `/home/sdtdserver/.local/share/7DaysToDie`, about 17 MB on Navezgane, and
  LinuxGSM's config. They leave out the 17 GB server in
  `/home/sdtdserver/serverfiles` (where `sdtdserver.xml` also lives),
  LinuxGSM's own backups, and its logs, whose `steam` link stopped every
  backup; `ludock.files` takes them back. A backup and its restore brought
  back the same saves. With the README's `START_MODE=1` a restart doesn't
  update the game.
- **Project Zomboid** (`renegademaster/zomboid-dedicated-server`) couldn't be
  checked: the image, last updated in 2024, passes empty arguments that the
  current game, Build 42, refuses, and so does Build 41.78 on the image's
  `GAME_VERSION=legacy41`. Files and backups leave out the server download
  in `/home/steam/ZomboidDedicatedServer`, as the image's README mounts it.

Not run, because this test machine (8 GB of memory, a 25 GB download limit)
couldn't hold them: Counter-Strike 2 (60 GB of disk), Conan Exiles (8 GB of
memory at least), Rust (12 GB), Satisfactory (8 to 16 GB), and both ARKs (6
to 13 GB). Don't Starve Together needs a server token from Klei, so it
wasn't run either.

## Labels

Labels are the advanced way to add or adjust a server. Put them on the
container where it's defined, such as its Compose service:

```yaml
services:
  terraria:
    image: example/terraria-server
    stdin_open: true
    labels:
      ludock.enable: "true"
      ludock.game: "terraria"
      ludock.name: "Terraria with friends"
```

| Label | Purpose |
| --- | --- |
| `ludock.enable` | `"true"` adds the container, even one that can control the host; `"false"` leaves it out, whatever the panel says |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Game integration, such as `minecraft` or `terraria` |
| `ludock.address` | Exactly what players type to connect, such as `mc.example.com`, `play.example.com:30000`, or `[2001:db8::1]:2456` |
| `ludock.console` | `minecraft-rcon`, `source-rcon`, `rust-webrcon`, `telnet-console`, or `stdin-console`; `disabled` or `none` turns commands off |
| `ludock.console.port` | The console port inside the container, if not the default |
| `ludock.console.host` | Connect to the console at this host directly. These protocols aren't encrypted, so use it only for a trusted host |
| `ludock.console.password-env` | The **name** of the container's environment variable that holds the console password, never the password |
| `ludock.query` | `minecraft-ping` or `a2s`; `disabled` or `none` turns the status check off |
| `ludock.query.port` | The status check port inside the container |
| `ludock.files` | Comma-separated container folders for files and backups, such as `"/data/worlds,/data/config"`; empty turns both off. A folder that holds, or is inside, one of Ludock's own folders or volumes is never offered, and none is when Docker won't let Ludock inspect its own container |
| `ludock.secret-env` | Comma-separated **names** of more environment variables to hide from logs and consoles |

A `ludock.game` label alone doesn't add an unsupported image; add the
container in the panel or with `ludock.enable`. Invalid label values are
ignored. **Settings → System → Container label problems** lists an invalid
`ludock.enable` or `ludock.address` and incomplete Compose labels.
Variables whose names look like credentials, such as `RCON_PASSWORD`,
`DB_PASS`, or `API_KEY`, and passwords inside URLs are hidden already. A value
is hidden only where it's printed exactly as set; a game that changes it, such
as by encoding or shortening it, can still show it.

### Which containers Ludock manages

In order:

1. A `ludock.enable` label decides. Any value other than `true` or `false`
   leaves the container out.
2. Containers that can control the host are left out: privileged ones, and ones
   that mount a Docker, Podman, containerd, or CRI-O socket, or bind `/`,
   `/run`, `/var/run`, the container runtime's storage, or a folder holding
   them. The panel can't add them, and a `ludock.enable` label their image
   sets doesn't count: it must be set where the container is defined. The
   [guide](GUIDE.md#servers-that-can-control-the-host) says why and what to do.
3. Compose one-off containers (`docker compose run`) are left out.
4. A container added in the panel is managed; one removed there is left out.
5. Containers on supported images are managed; others are left out.

Ludock remembers panel choices, sharing, and schedules by Compose project,
service, and replica, or by container name for a standalone container, so they
survive the container being recreated. Renaming a standalone container makes a
new server.

## Addresses

The address on the server list is the host from **Settings → Server address**,
or the one in your browser until it's set, with the host port published for
the game's own port and protocol. Other images use their first published port,
as does Core Keeper. A supported game that doesn't publish its game port shows
no address, and ports published only on loopback are never shown. Minecraft:
Bedrock Edition connects on TCP 19132; a server on the older UDP transport
needs `ludock.address`.

## Limits

| What | Limit |
| --- | --- |
| Upload | No size limit by default, but an upload must leave 1 GiB free on the server's disk (a twentieth of a disk under 20 GiB); set a limit in **Settings → Advanced** |
| Text preview and editing | 512 KiB; larger or binary files download instead |
| Unpacking an archive | 20 GiB, 100,000 entries, 64 nested folders, and 100 times the archive's size plus 1 MiB; 30 minutes |
| Selection | 500 entries of one folder |
| Folder listing | 10,000 entries |
| Upload or download | 30 minutes, or 60 seconds without data for an upload |
| Other file actions | 55 seconds |
| Backup archive | 100,000 entries and 64 folder levels; use `ludock.files` for smaller folders |
| Shell command | 60 seconds |
| Schedules | 100 per server and 1,000 in total, including paused ones |
| Activity | The newest 100,000 entries; finished work older than 90 days leaves once its entries do, keeping each server's newest 100 |
| Scheduled command | One line, 500 characters |
| Schedule warnings | Up to 10, each 1 to 60 minutes before |
| Waiting for an empty server | 1 to 360 minutes |
| Email recipients | 10 per destination |

Text files that mix line endings can't be edited in the browser, since it
can't keep them; download those instead.

## Health check

`GET /api/v1/health` needs no sign-in. It answers 200 with
`{"database":"connected","docker":"connected","status":"ok"}` while the
database and Docker are reachable, and 503 with `{"status":"degraded"}`
otherwise. The image's `HEALTHCHECK` runs `ludock healthcheck` against it.
Ludock has no other HTTP API.

## Updates

**Update server** (**Update image** for a game Ludock doesn't know) pulls the
tag the container was created from and, if the image changed, replaces the
container:

- It stops the server, takes a backup if chosen, and creates the new container
  beside the stopped one with the same environment, command, ports, networks,
  static addresses, restart policy, mounted folders, and volumes, including
  unnamed ones. Settings that only repeated the old image's defaults come from
  the new image.
- A running server's new container must become healthy, or keep running for 10
  seconds if the image has no health check; one whose health check is still
  starting after 3 minutes is accepted. Only then does Ludock remove the
  original and give the new one its name. Until then neither restarts on its
  own, so Docker can't run both. A stopped server stays stopped. Running or
  stopped means as Ludock found the server when it stopped it, after the
  download: a server started or stopped elsewhere meanwhile is kept that way.
- If the new container stops, restarts, or turns unhealthy before then, Ludock
  removes it and restores the original, stopped. Check **Activity**, restore
  the backup if needed, then start the server.
- Ludock never edits Compose files. Compose recognizes the new container as its
  own, so a later `docker compose up` usually leaves it alone, and changes you
  make to the definition still apply the next time you run your own tool.
- Files changed inside the container outside its volumes and mounts are lost,
  as with any recreation.
- An unnamed volume, such as one an image declares or a Compose `- /data`
  line makes, stays with the server. Docker hands an existing volume to a new
  container only by its name. `docker rm -v` removes only volumes a
  container got without one, and `docker compose down -v` those and the
  volumes the Compose file names, so both leave it behind. Compose does the
  same when it recreates a container itself. **Activity** says so after the
  update. Before `down -v`, list the server's volumes with
  `docker inspect -f '{{range .Mounts}}{{.Name}} {{.Destination}}{{"\n"}}{{end}}' <container>`,
  then, once you no longer want its data, remove the one left over with
  `docker volume rm <name>`. Naming the volume
  in the Compose file avoids this for a new server; an existing server's data
  stays in its unnamed volume, so copy it into a named one before switching,
  or the server starts with an empty one.
- **Advanced → Update even if the image is already up to date** recreates the
  container anyway, such as to pick up a game update that installs on startup.

Not supported: images pinned by digest or ID, containers Docker removes when
they stop (`--rm`, which Ludock never stops at all), Swarm services, and servers whose network, process, or IPC
namespace another container joins. Update those with the tool that made them.

**Newer image checks:** about every six hours Ludock asks each image tag's
registry, through Docker and without pulling, whether the tag points to a newer
image. Administrators then see **Update available** on the server list and
**A newer image is available.** on the **Update** tab. A registry that can't be reached isn't an alert; the
tab says the last check failed. Images built or loaded locally can't be
compared. Docker Hub counts each check against its pull rate limit. Turn the
checks off in **Settings → Advanced → Image updates**.

## Backup storage

**Settings → Backups** shows where backups go, the space they use, and the
free space left. Under **Settings → Advanced → Backup folder and limits**:

- **Backup folder:** one of the folders mounted for backups.
- **Minimum free space (GiB):** kept free on the backup disk, and on game-data
  disks during restores and unpacking. 5 GiB by default.
- **Total limit for all backups (GiB):** off by default. Backups whose
  archives are gone don't count toward it, though the space used in
  **Settings → Backups** still includes every backup Ludock lists.
- **Also copy backups to:** the [second copy](#a-second-copy-of-backups).

Once these settings are saved, Ludock never changes them by itself (including
turning the second copy back on if you chose **No second copy**). To turn
backups off completely, set `LUDOCK_BACKUP_ROOTS` to empty.

### Backups on another disk

Replace the `ludock-backups` volume in `compose.yaml` with a folder on that disk,
created first, writable, and separate from game data. If you already have
backups, stop Ludock and copy the old volume's files into the new folder
first, including the `ludock-data-*.tar` saves; Ludock can't use archives left
behind.

```yaml
services:
  ludock:
    volumes:
      - /srv/ludock-backups:/backups
```

Run `docker compose up -d`. Ludock finds its own container to check the mount;
if it says it can't, set `LUDOCK_SELF_CONTAINER`.

### A second copy of backups

If the backup disk or the host is lost, its backups go with it. To keep a copy
somewhere else, such as a NAS share, a USB disk, or a folder synced to the
cloud:

1. Mount that folder at `/backups-copy`:

   ```yaml
   services:
     ludock:
       volumes:
         - /mnt/nas/ludock:/backups-copy
   ```

2. Run `docker compose up -d --force-recreate ludock`.

Ludock finds the folder and starts copying to it by itself; **Settings →
Backups** says so. To stop, or to use another approved folder, choose under
**Also copy backups to** in **Settings → Advanced**; that choice sticks, even
while `/backups-copy` stays mounted. A second folder listed only in
`LUDOCK_BACKUP_ROOTS` is used only once chosen there.

Ludock copies the backups you already have, then each new one after its server
is back as it was, so the copy adds no downtime. Each copy is checked against
the original before it's named `<id>.tar`.

- **Problems:** a failed copy never fails the backup. Ludock retries after 1, 5,
  and 30 minutes and then every 6 hours, shows **Second copy failed** on the
  backup, and sends one alert if the third try fails. Saving the backup
  settings retries at once.
- **Removal:** when a backup is removed, its copy is removed too; the second
  folder needs no limits of its own.
- **Using the copy:** if a backup's archive is missing from the backup folder,
  downloading, browsing, and restoring read the copy. If the backup disk is
  lost, choose the second folder as the backup folder. If the whole host is
  lost, [restore Ludock's data](#restore-ludocks-data) from it first.
- **A folder that's away:** retention never removes or hides a backup whose
  folder can't be reached, since a disk may only be unmounted for now. The
  **Backups** tab marks it, and deleting it asks first, then removes it from
  Ludock with its second copy and leaves the old folder as it is. A folder
  that stops responding, such as a stuck network share, is given up on after a
  few seconds: the tab says so, and a restore fails before stopping the server.
  One that freezes while an archive is being read is given up on after two
  minutes without an answer; a restore then fails and leaves the game data as
  it was.
- **Changing the folder:** a new second folder gets a copy of every backup;
  until then a backup shows **Second copy waiting**. Old copies stay where they
  were, and Ludock no longer tracks them.

## Ludock itself

### Update Ludock

Ludock doesn't update itself. **Settings** says at the top when a newer image
is published. In the folder with its `compose.yaml`:

```bash
docker compose pull ludock
docker compose up -d ludock
```

Its data and your servers stay as they are. Work Ludock is stopped in the
middle of is recovered at the next start.

Data from a beta can't be used: 1.0 starts fresh (see
[Coming from a beta](../README.md#coming-from-a-beta)). Neither can data from
a newer version than the one starting. Either way, Ludock says why in its log
and on its address, then waits instead of exiting, so a container that
restarts on its own doesn't repeat the message, and it leaves the data as it
was.

### Remove Ludock

In the folder with its `compose.yaml`, let running work finish, then:

```bash
docker compose down
```

Your servers keep running as they were, and Ludock's data and backups stay
in their volumes for a later `docker compose up -d`. Backups restore
[without Ludock](#restore-a-backup-without-ludock). `docker compose down -v`
also deletes those volumes, with every backup in the example `ludock-backups`
volume. A server Ludock updated keeps the container Ludock made, with a
`ludock.update` label nothing reads once Ludock is gone.

### Logs

```bash
docker compose logs --follow --tail 200 ludock
```

For a bug report, turn on **Verbose logging** in **Settings → Advanced**; it
turns itself off after 24 hours. **Settings → System** shows whether Ludock
reaches Docker and which container labels it couldn't use.

### Ludock's own data

Whenever backup storage is on, Ludock saves its own data (accounts, sharing,
schedules, settings, notifications, and the records of every game backup) into
the backup folder: once a day, and within about 10 minutes of a new backup or
an administrator's change. Nothing stops while it saves.

- Each save is `ludock-data-<date>-<time>.tar` (UTC), a plain tar of the
  database and its identity key. It never appears in a server's **Backups**.
  A save never replaces a different save, this installation's or another's,
  as in a folder two share; if one with other contents already has that name,
  the save takes the next free second.
- Ludock keeps the 3 newest saves and the newest of each of the last 7 days
  with one, and removes older ones only after a newer save is checked. It
  removes only its own saves, which hold its identity key; saves from another
  installation or a beta stay as they are.
- With a second copy chosen, the newest save is copied there too, and that
  folder keeps saves by the same rule.
- **Settings → Backups** shows when Ludock's data was last saved, or why
  saving is failing. Failures retry like second copies and alert once.

A save holds password hashes, two-factor secrets, sessions, notification
tokens, and the key that protects them. Keep the backup folder and the second
folder as private as Ludock's data volume. A save doesn't include game worlds;
those are the game backups.

### Restore Ludock's data

`restore-data` restores into a data folder that has no database yet. Stop
Ludock, list the saves, and restore the newest:

```bash
docker compose stop ludock
docker compose run --rm --entrypoint ls ludock /backups
docker compose run --rm ludock restore-data /backups/ludock-data-20261120-031500.tar
docker compose start ludock
```

If the data volume still has a database you want to replace, move it and its
key (`ludock.db*`) aside after stopping Ludock and before `restore-data`:

```bash
docker compose run --rm --entrypoint sh ludock -c 'mkdir /data/replaced && mv /data/ludock.db* /data/replaced/'
```

`restore-data` checks that the file is complete and that its key matches the
database before putting anything in place.

If the whole host was lost, set up Docker and Ludock's `compose.yaml` on the
new host, mount the second folder (or a copy of its files) at `/backups`, and
run the commands above, ending with `docker compose up -d`.

Without Ludock, `tar -xpf` a save into an empty data folder, as the user Ludock
runs as.

A save taken while a restore was running, or before an unfinished one was
undone, doesn't know how that restore ended, so Ludock never undoes it from the
restored data: its **Activity** entry says so and which `.ludock-restore-<id>`
folder to look for.

Afterwards, game backups are listed again and read from the backup folder as
before. Archives made after the save aren't listed, and records of archives
removed since show them as missing. Game servers recreated with the same
Compose project and service are recognized with their sharing and schedules.
If a server's data now lives on a different volume or host folder, Ludock asks
an administrator to confirm the change before people it's shared with can use
it again. Then, to bring a server's game data back:

1. Open the server's **Backups**. Backups taken on the old host say **Taken
   when the server's data lived somewhere else**.
2. Choose **Restore…** on the one you want, type the server's name, and
   confirm. Ludock stops the server, backs up what is there now, puts the
   backup's files in the server's data folders where they are now, and returns
   the server to how it was. Activity notes that the backup was taken before
   the data moved.

This works when the server runs the same game with the same data folders
inside the container, such as `/data`. A backup that doesn't match says why;
[restore it by hand](#restore-a-backup-without-ludock). If you mounted the
second folder as the backup folder, choose a new second folder in **Settings**.

### Restore a backup without Ludock

A game backup is a plain tar. Each data folder is stored under
`snapshot/root-<id>/`, where `<id>` is the first 12 hex digits of the SHA-256
of the folder's container path. Most servers have one folder, such as `/data`:

```bash
id=root-$(printf %s /data | sha256sum | cut -c1-12)
tar -tf backup.tar | head          # shows snapshot/$id/...
sudo mkdir restored
sudo tar -xpf backup.tar --numeric-owner -C restored --strip-components=1 "snapshot/$id"
```

A backup from a beta may number its folders instead, `snapshot/root-0/` and
so on, in the order the server listed them; `tar -tf` shows which `$id` to
use. Run these as root so owners stay as they were. Stop the game, then replace the
contents of its data folder with those of `restored/$id/`, still as root and
keeping owners and permissions, such as with
`sudo cp -a "restored/$id/." <folder>/` after emptying the folder. For a data
folder that is a whole named volume, the folder is the one
`docker volume inspect -f '{{.Mountpoint}}' <volume>` prints; for one inside a
volume, add the rest of its container path to that.

With rootless Docker
the host's root is not the containers' root, so do the same from inside a
container, which keeps owners as the game sees them. It needs no path on the
host for a volume, and room on Docker's own disk to unpack the backup:

```bash
docker run --rm -v <volume or folder>:/target -v "$PWD/backup.tar:/backup.tar:ro" alpine sh -c \
  "mkdir /restored && tar -xpf /backup.tar --numeric-owner -C /restored --strip-components=1 snapshot/$id \
   && find /target -mindepth 1 -delete && cp -a /restored/$id/. /target/"
```

In the backup folder itself, each archive is `<id>.tar`. `ls -lt` lists the
newest first (a second copy is dated as its backup, not when it was copied),
and `tar -tf` shows which game's files one holds, which tells servers of
different games apart. Files starting `ludock-data-` are
[Ludock's own data](#ludocks-own-data), not game backups.

### Recover an administrator account

For a forgotten password with no other administrator, in the folder with
Ludock's `compose.yaml`:

```bash
read -rsp 'New password: ' LUDOCK_RECOVERY_PASSWORD && export LUDOCK_RECOVERY_PASSWORD
docker compose stop ludock
docker compose run --rm -e LUDOCK_RECOVERY_PASSWORD ludock recover admin
unset LUDOCK_RECOVERY_PASSWORD
docker compose start ludock
```

Replace `admin` with the username. This also turns off the account's
two-factor sign-in and signs it out everywhere. It doesn't create accounts.

## Deployment

Run one Ludock per Docker host with [`compose.yaml`](../compose.yaml). It keeps
its database in `/data` and backups in `/backups`, in separate volumes.

- **Docker:** Ludock uses the mounted Unix socket and needs Engine API 1.44 or
  later. Remote Docker connections aren't supported. For
  [rootless Docker](https://docs.docker.com/engine/security/rootless/tips/),
  mount its socket at `/var/run/docker.sock`.
- **Setup code:** until the first administrator exists, the setup page asks for
  the one-time code Ludock prints to its log at startup. A new code is printed
  at each restart until setup is done. A code set with `LUDOCK_SETUP_CODE`
  isn't printed and stays the same. Failed attempts are limited per address.
- **Existing data:** Ludock refuses to start on a database it can't use, such
  as another program's, a beta's, or a newer version's, says which, and leaves
  it unchanged.

### Remote access

The Docker socket gives control of the whole host, so expose Ludock beyond a
trusted network only through an HTTPS reverse proxy, and keep port 3000 itself
reachable only from the proxy or your own network: the example `compose.yaml`
publishes it on every interface. The proxy must keep the
public host name, forward the original protocol, pass WebSocket upgrades and
server-sent events without buffering, overwrite any client-supplied
`X-Forwarded-Host`, `X-Forwarded-Proto`, and `X-Forwarded-Port` headers, and
add the visitor's address at the end of `X-Forwarded-For`, on the same line
or a line of its own.

Then trust the proxy in **Settings → Advanced → Reverse proxy**, so sign-in
limits and the session list see each visitor's own address. When you open
Settings through an untrusted proxy at a private or local address, that section
opens by itself and offers **Trust** with the proxy's address. You can also
enter addresses or ranges, such as `172.18.0.2, 2001:db8::/64`. A trusted
address can claim to be any visitor, so never trust a network shared with
containers you don't control.

### Environment variables

Most installations need none. Set one under `environment:` in Ludock's Compose
file and run `docker compose up -d`. Ludock refuses to start when a value is
invalid and names the variable. The first three are also in **Settings →
Advanced**; a value set here wins, and the panel shows it as fixed.

| Variable | Default | For |
| --- | --- | --- |
| `LUDOCK_MAX_UPLOAD_SIZE` | No limit | The largest uploaded file, such as `500 MB` or `4 GiB` |
| `LUDOCK_LOG_LEVEL` | `info` | `error`, `warn`, `info`, or `debug`, kept until removed |
| `LUDOCK_TRUSTED_PROXIES` | None | Reverse proxies, as addresses or ranges separated by commas |
| `LUDOCK_BACKUP_ROOTS` | `/backups`, plus `/backups-copy` when mounted | Approved backup folders, separated by `:`; empty turns backups off |
| `LUDOCK_SELF_CONTAINER` | Detected | Ludock's own container name or ID, if Ludock says it can't find it. Ludock needs it to find its image and to keep its own folders out of **Files** |
| `LUDOCK_SETUP_CODE` | Printed at start | Your own setup code, 32 to 128 characters; remove it after setup |
| `LUDOCK_DOCKER_CONFIG` | None | A folder with a Docker `config.json` for [private registries](#private-registries) |
| `LUDOCK_SENSITIVE_PATHS` | None | More container or host paths to block in file access, separated by `:` |
| `LUDOCK_HELPER_IMAGE` | Ludock's own image | Another image for file and backup helpers, pinned by digest |
| `LUDOCK_DOCKER_SOCKET` | `/var/run/docker.sock` | The Docker socket's path inside Ludock |
| `LUDOCK_LISTEN` | `:3000` | The address inside the container; change the published port in `compose.yaml` instead |
| `LUDOCK_DATA_DIR` | `/data` | The folder for Ludock's database and identity key |
| `LUDOCK_RECOVERY_PASSWORD` | None | Only for [recovering an administrator](#recover-an-administrator-account) |

### Private registries

For images from a registry that needs a login, mount a read-only folder with a
Docker `config.json` whose `auths` entries hold the credentials inline, outside
any game's file folders, and point `LUDOCK_DOCKER_CONFIG` at it:

```yaml
services:
  ludock:
    environment:
      LUDOCK_DOCKER_CONFIG: /registry
    volumes:
      - /srv/ludock-registry:/registry:ro
```

Updates and newer image checks then use it.

## Behind the scenes

### When a server's container changes

A Compose server is identified by its project, service, and replica, and a
standalone container by its name. Recreating it with a new password, port,
label, or environment variable keeps its history, sharing, and schedules. A
change to its game or to the data folders Ludock reads and backs up pauses the
access of everyone it's shared with, and their schedules, until an
administrator chooses **Confirm and restore access**, since what they were
given has changed. Backups taken before the change still restore when the game
and the data folders' paths inside the container are the same, after an
administrator confirms that the data now lives somewhere else; otherwise the
backup says what differs.

### Helpers

Ludock runs short-lived helper containers from its own image:

- **File and backup helpers** mount only approved data, run as root with no
  network, and are removed afterward; idle ones exit after 35 minutes. Bind
  mounts need validated host paths and recursive read-only bind support; symlinked sources and Docker Desktop path aliases can prevent
  access. Named volumes must use the `local` driver without options that remap
  the host path.
- **Console, player, and status-check helpers** share the game container's
  network and connect on `127.0.0.1`, so console and query ports needn't be
  published. A console password reaches its helper over standard input.

### Ludock's folders in server data

Ludock keeps a few working folders and files inside a server's data folders.
Every name that starts with `.ludock-`, in any letter case, is Ludock's:

| Name | What it is | When it goes |
| --- | --- | --- |
| `.ludock-restore-<id>` | A restore's staging folder, at the top of a data folder: the backup's data before it moves in, then the data it replaced | When the restore ends; one that stopped partway stays until it is finished or undone |
| `.ludock-unpack-<id>` | An unpack in progress, beside the archive | When the unpack ends, or at Ludock's next start |
| `.ludock-upload-<id>.tmp` | An upload or edit in progress, beside the file | When the upload ends |

Files doesn't show these names, and no one can create, rename, move, upload,
unpack, download, or delete one there. A folder holding an unpack's or
upload's folder can still be deleted as a whole, but not one holding a
restore's: it can be below the top of what Files shows, when two servers show
different folders of shared storage or `ludock.files` was widened, and
Files names it instead. Backups leave them out, and a restore leaves those at the top of a data folder
where they are, so one left over, or made by hand, never blocks a backup or a
restore. To
remove one Ludock no longer needs, delete it on the host. The previous data of
a restore that stopped partway is in its `.ludock-restore-<id>/old`.

`ludock-set-aside-<id>`, without the leading dot, holds what a server wrote
while a stopped restore waited to be undone. It is ordinary data: Files shows
it, backups include it, and you can delete it once you've kept what you need.

Ludock never deletes, renames, or moves a folder that is, or holds, a folder
mounted into the server separately, such as `./mods:/data/mods` inside
`./data:/data`; it says so instead. Delete what's inside that folder.

### Activity results

Each piece of work in **Activity** shows one result:

| Result | Means |
| --- | --- |
| Queued | Waiting for other work on the server, or on the backup folder, to finish |
| Running | Under way; work asked to warn players first shows **Checking who is online**, then **Warning players** until the warning is sent and **Giving players 1 minute** once it is |
| Succeeded | Done |
| Already up to date | An update found nothing newer |
| Skipped | Decided not to act, such as a scheduled command for a stopped server |
| Failed | Stopped with the reason shown; Ludock left the server as safe as it could |
| Interrupted | Ludock restarted while it acted; check the server |
| Cancelled | Called off while it was queued or warning players, before it changed anything, by the person named |

An automatic restart is listed as by "Ludock, because the game stopped
answering". Only queued work and work warning players can be cancelled, from
its row in **Activity** or on the server's page, by an
administrator or anyone who may start that action on the server; a cancelled
run doesn't pause its schedule. **Show → Everything** also records cancelling
a scheduled run that waited for players to leave.

### After an interruption

Work is recorded before it runs. When Ludock starts after a crash or restart,
it removes helpers left behind before anything else runs, then reconciles
anything that was in progress:

- **Stops:** waits for a stop that was in progress instead of sending it again.
- **Backups and restarts with a backup first:** starts a server that was
  stopped for a backup, without taking the backup again.
- **Restores and unpacking:** finishes or rolls back an interrupted restore
  from its journal, and removes leftovers from interrupted archive unpacks.
- **Updates:** an update interrupted before its new container ran didn't
  happen, and the server is returned to how it was; one interrupted while its
  new container was being checked is undone and the server left stopped (since
  the new version may have changed the game data); one that had begun removing
  the original is finished, and its new container is started if the server was
  running and Docker stopped it meanwhile. If it won't start, the server is
  left stopped, listed in **Needs attention**, and its entry says so.
- **Everything else:** work Ludock can't safely finish is marked interrupted,
  never repeated. Cancelled work stays cancelled, and a warning Ludock resumes
  can still be cancelled. One-time scheduled runs missed by more than 5 minutes
  are recorded as missed, and repeating ones missed by more than 15 minutes
  skip that time.

**Activity** says what happened to each piece of work.

A restore that stops partway, because something outside Ludock started the
server or a step failed, always ends in one of these states, which its entry
in **Activity** names:

| Where it stopped | What it leaves | How it is finished |
| --- | --- | --- |
| Before any data moved (staging, unpacking) | Nothing: the unpacked copy is removed even if the server is running | Nothing to finish |
| After data moved, with the server stopped | The restore undone | Nothing to finish |
| After data moved, with the server started elsewhere | The previous data in the root's `.ludock-restore-<id>/old`, not in place; the server left running | See below |
| After the backup's data was in place, cleanup failing | The data it replaced in `.ludock-restore-<id>` | Removed at the next backup, restore, or update |

A restore left with previous data out of place is listed in **Needs
attention** for administrators. **Undo the restore** stops the server, the one
case where a data operation stops a server another manager started; it warns
players first when anyone is online and the game can be messaged. Ludock moves
whatever is in the data folder now into `ludock-set-aside-<id>` beside it,
returns the previous data, and starts the server again if it was running. If
no one does that, the next backup, restore, update, or restart with a backup
first finishes the undo the same way before its own work, and so does Ludock's
next start if the server is stopped then. If the `.ludock-restore-<id>` folder
is gone, there is nothing to put back: the data is left as it is and the
entry says so. A state Ludock can't finish safely, such as data folders that
changed since, makes those refuse instead; a backup, restore, or update that
stopped a running server for it starts it again, unless the undo had already
begun moving data. A restart with a backup first, while Ludock keeps running,
starts it only once its backup is taken.

If Docker itself restarts while work runs, such as for an upgrade, Ludock's
requests to it wait up to 90 seconds for it to answer again, and the work's
entry in **Activity** says it waited. What happens to the step Docker cut
depends on the step:

- **Waits and carries on:** stops, starts, container checks, and an update's
  steps, each checked again before it is sent. A stop or start whose answer
  was lost counts as done if Docker then shows the server stopped or started.
- **Done again from the start, at most three times:** a backup's copy, whose
  partial archive is removed first, and a restore's unpacking, whose stage
  folders are emptied first. Each starts again only once the helper that was
  writing is gone. Unpacking also starts again when only the connection to
  Docker dropped, such as when a proxy in front of its socket reloads.
- **Rolled back:** a restore cut while it moved data into place. Its journal
  undoes the swap instead of repeating it.
- **Fails:** a scheduled console command, which is sent on time or not at
  all, and work Docker answered with an error of its own.

A Docker that keeps containers running while it restarts (`live-restore`)
and makes its own socket, not systemd's, replaces the socket file Ludock's
container mounted, which then never answers. Ludock notices within seconds
and exits with an error, so the `restart: unless-stopped` in its
`compose.yaml` starts it again on the new socket; open consoles reconnect by
themselves for about two minutes.

Docker coming back with the server started on its own still counts as a
change outside Ludock. After 90 seconds, or when Ludock can't tell what Docker
did, the work stops and says Ludock couldn't reach Docker.

Asked to stop, Ludock gives running work about six seconds to finish, inside
the ten Docker waits before killing it. Work still running then, such as a
backup or a warned restart's minute, is left as a crash would leave it and is
resumed or checked on the next start.
