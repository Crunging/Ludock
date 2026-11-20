# Guide

How to do things in Ludock, then how its parts behave. For installing, see the
[README](../README.md#quick-start); for labels, limits, and rare setups, see the
[reference](REFERENCE.md).

Ludock has four pages: **Servers**, **Activity**, **Users**, and **Settings**.
Members see only Servers and Activity.

## Everyday tasks

### Finish setting up

After the first administrator is created, the server list shows a **Finish
setting up** list with only what's still undone. Each item does its job in one
click, and goes away once done.

- **Get alerts** opens the notification settings.
- **Back up your servers** offers **Back up every night**: after you confirm,
  each server without a backup schedule gets a daily one between 04:00 and
  06:00 in your browser's time zone, a few minutes apart.
- **Restart servers that stop answering** offers **Restart automatically** for
  servers whose game Ludock can check (see
  [Restart a game that hangs](#restart-a-game-that-hangs)).
- **Check the address players use** appears when the address players are shown
  only works on your own network. It opens **Server address**.
- **Add the servers Ludock didn't find** appears when a container it doesn't
  manage looks like a game.
- **Protect your sign-in** sets up two-factor sign-in. It's optional.

An item comes back if it becomes true again, such as a new server without
backups. **Dismiss** hides it for every administrator for good.

### Add a server Ludock didn't find

Servers on [supported game images](REFERENCE.md#supported-games) appear by
themselves. For any other container, such as an ich777, LinuxGSM, or custom
image:

1. On **Servers**, open **Missing a server?** below the list.
2. Pick the game if Ludock didn't guess it, and choose **Add**.

Nothing changes in Docker or in the container's Compose file. A new server is
visible only to administrators until you share it.

To take a server out, open it and choose **Remove from Ludock** at the bottom of
its **Overview**. The container keeps running. Its sharing, schedules, and
backups come back if you add it again.

Ludock never offers itself, its helpers, Compose one-off containers, or
containers that could control the host (privileged ones, or ones given a Docker
socket or the host's root). Add those only with a
[label](REFERENCE.md#labels), deliberately.

### Share a server with a friend

1. Open the server and choose **Sharing**.
2. Type their username under **Share with someone**. If they already have an
   account, pick them from the list instead.
3. Choose their **Access** and choose **Share**.

For someone new, Ludock makes the account and shows a one-time invite link.
Send it to them privately: they open it, choose a password, and land on the
servers shared with them. The link works once and expires after 24 hours.

| Access | What they can do |
| --- | --- |
| Can see it | See whether it's running, who is online, and the address to join |
| Can run it | Also start, stop, and restart it, use its game console, and read its logs |
| Full access | Also read and change its files, create backups, and make their own schedules |

**Custom** picks exactly which of those actions someone has. Restoring,
downloading, and deleting backups, updates, the Shell, monitoring settings, and
adding or removing servers stay with administrators. Changes to sharing take
effect at once, including in pages and consoles the person has open. To change
or end it, use the same tab: pick a new level and choose **Save**, or choose
**Stop sharing**.

### Get alerts

1. Open **Settings** and scroll to **Notifications**.
2. Open a destination (Discord, ntfy, Gotify, Telegram, Email, or Webhook),
   fill in its fields, and tick **Send alerts to …**.
3. Choose its **Save** button, then **Send test notification**.

Every server is monitored from the moment Ludock finds it, so alerts start as
soon as a destination is on. Each destination's **Alert types** choose what it
gets; all but **Newer images available** are on by default. Saved tokens,
topics, passwords, and webhook URLs are never shown again; leave a field blank
to keep it. What triggers each alert is in [Monitoring](#monitoring).

### Back up a server, or every night

To back up now, open the server's **Backups** tab and choose **Create backup**.
The server stops while it copies, then goes back to how it was.

To back up on a schedule, use **Back up every night** in
[Finish setting up](#finish-setting-up), or add one on the server's
**Schedules** tab: choose **Backup** as the action, or keep **Restart** with
**Back up first** ticked.

Backups need nothing set up: the example `compose.yaml` mounts a `backups`
volume, and Ludock turns storage on there by itself. **Settings → Backup
storage** shows where backups go and how much space they use. Each server
keeps the 3 newest backups plus the last of each of the 7 most recent days,
4 weeks, and 6 months that have one; Ludock removes the rest after each new
backup. To keep one for good, choose **Keep…** on it.

Each backup is a plain tar archive of the server's data folders, so it can be
restored without Ludock. To keep backups on another disk, or a second copy
elsewhere, see [Backup storage](REFERENCE.md#backup-storage).

### Restore a backup, or one file

Restoring needs an administrator.

- **A whole backup:** on **Backups**, choose **Restore…** on a backup, type the
  server's name, and choose **Restore game data**. Ludock checks the archive,
  stops the server, takes a safety backup of the current data, swaps the
  backup in, and returns the server to how it was.
- **One file or folder:** choose **Browse** on a backup, find the item, choose
  **Restore…** from its menu, and type the server's name. Ludock replaces only
  that item. No safety backup is taken, so back up first if you might want the
  current copy.

**Browse** also downloads single files from a backup without changing
anything. A backup taken before the server's data folders changed can be
browsed but not restored.

### Update a server

Many game images download the latest game when they start, so **Restart**
updates the game; the server's **Update** tab says so under **Game updates**.
To back up first, tick **Take a backup first** in the Restart dialog.

To move to a newer image:

1. Open the server's **Update** tab. It says when a newer image is available;
   **Check now** asks the registry right away.
2. Leave **Back up first** ticked and choose **Update server**.

Ludock pulls the image and replaces the container with one that keeps the same
settings, ports, networks, and volumes. If the new one fails to start cleanly,
Ludock puts the old one back and leaves it stopped, since the new version may
already have changed the data. Updates need an administrator; details and
limits are in [Updates](REFERENCE.md#updates).

### Restart a game that hangs

A game can freeze while its container keeps running. For servers with a
[status check](REFERENCE.md#supported-games), Ludock can restart it:

- Use **Restart automatically** in [Finish setting up](#finish-setting-up), or
- tick **Restart it if the game stops answering** on the server's **Overview**.

When the game stops answering for the **Wait before alerting** time (two
minutes by default), Ludock restarts it the way **Restart** does and sends an
alert. It leaves alone a server that is stopped, still starting, or hasn't
answered since it last started. If the game doesn't answer again within five
times the wait, or after 3 restarts in 6 hours, Ludock says so and stops
trying.

### Manage players

Who is online shows on the server list and the server's **Overview** for
[games Ludock can read](REFERENCE.md#supported-games).

- **Kick or ban:** with permission to use the console, click a player's name on
  the **Overview** and choose **Kick** or **Ban…**.
- **Allowlist and bans (Minecraft):** below the **Overview**, add or remove
  allowed players by name and lift bans. Whether the allowlist is on is the
  server's own `white-list` setting, which Ludock shows but never changes.

Each action is one console command, recorded in **Activity**.

### Schedule restarts and other actions

On the server's **Schedules** tab, a new schedule starts as a daily restart at
08:00 in your time zone, backing up first when it can. Pick the action (Start,
Stop, Restart, Backup, or Send command), the days or a single date, and choose
**Add schedule**. Under **More options**:

- **Wait until no one is online** holds a restart or stop until the server is
  empty, for up to 30 minutes by default, then skips or goes ahead as chosen.
- **Announce countdown to players** warns players through the game console 10,
  5, and 1 minutes before a restart, or at times you choose.

**Pause** a schedule in the list to stop it without deleting it.

## How things work

### Servers and their state

States follow Docker. A server with a health check shows **Starting** until it
passes and **Unhealthy** while it fails. A stopped server shows **Crashed**
after an error exit, **Out of memory** when the kernel killed it, and
**Force-stopped** when Docker killed it for not shutting down in time, so
recent progress may be lost.

The address players use is the host from **Settings → Server address** (or the
one in your browser until you set it) with the port the container publishes
for its game. When that's wrong, such as behind a router's port forwarding, a
tunnel, or a Minecraft SRV record, label the container with `ludock.address`.

You can keep changing servers with your own tools. A new password, port,
label, or environment variable is picked up, and history, sharing, and
schedules stay. Only a change to the server's game or data folders asks
anything: the server shows **This server changed in Docker**, and the people
it's shared with can't use it, and their schedules wait, until an
administrator chooses **Confirm and restore access**. Administrators keep using
it meanwhile.

### Stopping and restarting

When the game's console has a **Save world** shortcut, Ludock sends it and
waits five seconds before stopping. Docker then gives the game 120 seconds to
shut down (more for slow savers such as ARK, Palworld, Valheim, 7 Days to Die,
and Don't Starve Together), or the container's own `stop_grace_period` if it
is longer.

**Take a backup first** in the Restart dialog checks the backup folder, stops
the server, backs it up, and starts it once. If the backup fails, the server
stays stopped and Ludock sends an alert, since starting may update the game.

Every stop, restart, backup, restore, and update is listed in **Activity**,
with who started it.

### Consoles

**Console** on a server opens up to three views:

- **Logs**: what the server prints.
- **Console**: the game's own console, with shortcut buttons for common
  commands. Up and Down repeat commands from this visit.
- **Shell**: commands run inside the container, for administrators. Each gets
  60 seconds.

Network consoles (RCON, WebRCON, Telnet) need the console turned on in the
game. Ludock finds its password in the variables images usually use, such as
`RCON_PASSWORD`, `ADMIN_PASSWORD`, or `TELNET_PASSWORD`, and for Minecraft also
the one the image generates. If yours uses another name, set
[`ludock.console.password-env`](REFERENCE.md#labels). Terraria, Bedrock, and Vintage Story read commands
from the server's input, which needs `stdin_open: true` on the container, and
their replies appear only in **Logs**. Ludock reaches consoles from inside the
container's network, so the console port needn't be published.

### Files

**Files** opens the server's data folders: the writable volumes and bind mounts
Ludock considers safe. It shows container paths, never host paths.

- Upload by dropping files on the page or with **Choose files**. Each upload
  replaces the destination only once it is complete.
- Choose a text file to preview it, and **Edit** to change it. If the file
  changed since you opened it, the save is refused and your text stays.
- **Unpack** extracts a zip, tar, or tar.gz (including `.mcworld`, `.mcpack`,
  and `.mrpack`) into a new folder beside it. It never replaces anything.
- **Select** picks several entries to download as one tar, move, or delete.

Stop a game before replacing a world or configuration it has open. Ludock
never follows symbolic links, and blocks system and sensitive paths and Docker
sockets. To limit access to some folders, set
[`ludock.files`](REFERENCE.md#labels). Size and time limits are in the
[reference](REFERENCE.md#limits).

### Backups

- **Downtime:** a backup stops the server for the whole copy, then returns it
  to its previous state. Live backups aren't supported.
- **Readiness:** the **Backups** tab checks the destination, free space, and
  whether another container writes the same data, without stopping anything.
  Every backup checks again before it starts.
- **Space:** Ludock keeps 5 GiB free on the backup disk by default. If the next
  backup might not fit, it removes the server's oldest backups first, never the
  3 newest or kept ones; if there still isn't room, the backup fails and says
  why.
- **Cancelled backups:** a forced stop, a start from another tool, a replaced
  container, or another running container writing the same data cancels the
  copy.
- **Contents:** permissions, owners, and modification times are kept; ACLs,
  extended attributes, and sparse files aren't. Links and special files are
  refused.
- **Safety backups** taken before a restore or update follow the same
  retention rule and are labeled in the list.
- **Records:** Ludock lists only archives it made, from records in
  [its own data](REFERENCE.md#ludocks-own-data); other archives can't be
  imported.
- **Who:** members with **Full access** can create backups. Browsing,
  downloading, restoring, keeping, and deleting are for administrators, since
  archives can hold configuration secrets.

If a restore fails, leave the server stopped and check **Activity**. Don't
delete `.ludock-restore-*` folders: they may be needed to recover the previous
data. An interrupted restore is finished or rolled back when Ludock starts
again.

### Schedules

- A schedule runs with its owner's current access. Members with **Full
  access** manage their own; administrators manage all of them.
- Missed times don't run late: if Ludock is down at that time and back more
  than five minutes late, the run is recorded as missed. A daylight-saving
  hour that doesn't exist is skipped, and a repeated one runs once.
- **Back up first** needs the server running when the schedule runs. If the
  backup fails, the server stays stopped.
- **Wait until no one is online** works on servers whose players Ludock can
  read, and waits 1 to 360 minutes. A player count Ludock can't read counts as
  players online. Countdown warnings need **Go ahead anyway** and count down to
  the end of the wait.
- **Countdown warnings** are best effort: each is sent at most once, and
  skipped when the console doesn't answer.
- **Send command** sends one line, up to 500 characters, through the game
  console. It doesn't start a stopped server. Anyone who can see the schedule
  can read the command, so leave passwords out.

### Players

Ludock asks the game's console who is online about every 30 seconds while the
server runs. Games with a status check but no player list, or no console
password, still show how many are online. A count Ludock can't read shows as
unavailable, never as empty. Readings stay in memory.

A kick or ban names a player exactly as the game's latest list did, and Ludock
refuses names with spaces, quotes, or selectors such as `@a`. On Minecraft it
also checks the name against the player's account first, so a nickname or a
plugin's list never targets someone else; on such servers, use the console.

### Monitoring

Monitoring watches each server's container, its game, its disks, and its
memory. An administrator can turn it off, change its levels, or **Pause alerts
for maintenance** on the server's **Overview**.

| Alert | When |
| --- | --- |
| Server unavailable | The container stops, or fails its health check, without Ludock stopping it, for longer than **Wait before alerting** (120 seconds by default) |
| Game not answering | A game with a status check answered since its container started, then stops answering for that long |
| Disk nearly full | A data folder's disk reaches 90% full |
| Memory near its limit | A container with a memory limit stays at 90% of it for 5 minutes |

Each alert is sent once, with another when it recovers. Stops and work started
through Ludock don't alert, and a server that was already stopped when Ludock
found it stays quiet until it runs. A game still starting, or whose status
check is off or wrong, never counts as not answering. Disks are measured about
every 10 minutes while someone has Ludock open and every 30 otherwise.

### Notifications

Each destination's **Alert types**:

- **Server problems:** a server is unavailable, its game stops answering, Ludock
  can't reach Docker, a disk is nearly full, or memory is near its limit, and
  each recovery.
- **Failed backups and schedules:** a backup, scheduled action, second copy,
  or save of Ludock's own data fails, or a server is left stopped after a
  failed backup.
- **Restores and updates:** a restore or update finishes or fails.
- **Newer images available:** a newer image is published for a server, once per
  version. Off by default.

Alerts name servers and container paths, never images or host paths. Each
delivery is tried up to five times; **Recent deliveries** lists the latest 50
and offers **Retry** on failed ones. Turning a destination off holds its
deliveries until it's on again.

### Accounts and sign-in

**Administrators** use every server and manage users and settings. **Members**
use only the servers shared with them. **Users** lists everyone and what each
can use; there you add an account without sharing a server yet, make someone
an administrator or member, disable or delete an account, or make a **Reset
link** for a forgotten password. Making someone an administrator drops their
per-server sharing. Ludock always keeps at least one administrator who can
sign in.

**Two-factor sign-in:** in **Account**, choose **Set up two-factor sign-in**,
scan the QR code with an authenticator app, and enter your password and a code.
Keep the ten recovery codes Ludock shows once; each works once instead of a
code. A reset link still asks for a code, so for a lost phone an administrator
chooses **Turn off two-factor** on **Users**.

Invite and reset links work once, expire after 24 hours, and point at the
address you're using Ludock at, so make them from the address the user will
open. **Users** shows a link that hasn't been used yet, with **Revoke link**. Too many wrong passwords or codes from one address are refused for a
while.

### Activity

**Activity** lists work on the servers you can use, newest first, with filters
for server, person, action, result, and dates. Each server's **Activity** tab
lists its own. Administrators can choose **Show → Everything** to add sign-ins,
account changes, and settings changes. **Needs attention** on the server list
shows recent failures.
