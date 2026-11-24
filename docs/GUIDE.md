# Guide

Installing is in the [README](../README.md#quick-start); labels, limits, and
rare setups are in the [reference](REFERENCE.md).

Ludock has five pages: **Servers**, **Activity**, **Users**, **Settings**, and
**Account**, where you change your password and two-factor sign-in. Members see
only Servers, Activity, and Account.

## Everyday tasks

### Finish setting up

After the first administrator is created, the server list shows a **Finish
setting up** list with only what's still undone. Each item does its job in one
click, and goes away once done.

- **Get alerts** opens the notification settings. It goes away once a
  destination is on. If every alert sent to that destination so far has
  failed, it stays, says why, and links straight to its settings.
- **Back up your servers** offers **Back up every night**: after you confirm,
  each server that has data folders and no repeating schedule that backs it up
  (a backup, or a restart with **Back up first**; a paused schedule still
  counts) gets a daily backup between 04:00 and 06:00 in your browser's time
  zone, spaced a few minutes apart. Like a new schedule, each waits for
  players to leave and warns anyone still on where the game allows. It
  appears once backup storage is on.
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
backups come back if you add it again. A server labeled
`ludock.enable: "true"` can't be removed from the panel: set the label to
`"false"` where the container is defined and recreate it.

Ludock never offers itself, its helpers, or Compose one-off containers.

#### Servers that can control the host

A container that could control the host is never added by itself or from the
panel, even on a supported game image: a privileged one, or one given a Docker
or other container engine socket, the host's root, or its system folders
([the exact rule](REFERENCE.md#which-containers-ludock-manages)). Whoever uses
its console or files could take over the machine. If it looks like a game,
**Missing a server?** lists it under **Not added because they can control the
host**. If it really is a game server, remove the access it doesn't need, or
add the label `ludock.enable: "true"` where it is defined and recreate it. Its
**Sharing** tab then warns that sharing it can hand over the host, so share it
only as **Can see it**, or with people you would trust with the host. If a
server later gains that access (for example, if it is recreated as privileged),
Ludock stops managing it; its sharing, schedules, and backups come back if you
remove that access and recreate it.

### Share a server with a friend

1. Open the server and choose **Sharing**.
2. Under **Share with someone**, enter a username for a new friend under **Their
   username**. If you already have members, pick **Someone new…** under
   **Person** first, or pick an existing member.
3. Choose their **Access** and choose **Share**.

For someone new, Ludock makes the account and shows a one-time invite link.
Send it to them privately: they open it, choose a password, and land on the
servers shared with them. The link works once and expires after 24 hours. It
uses the address you opened Ludock at; when that only works on your computer
or your home network, such as `127.0.0.1`, Ludock says so beside the link, so
open Ludock at an address your friend can reach and make a new one.

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
2. Open a destination (Discord, ntfy, Gotify, Telegram, Email, or Webhook)
   and fill in its fields. A new destination is on once saved; untick
   **Send alerts to …** to keep it off.
3. Choose its **Save** button, then **Send test notification**. The result
   shows beside the button: **Delivered**, or why it didn't arrive and what
   to check.

Every server is monitored from the moment Ludock finds it, so alerts start as
soon as a destination is on. Each destination's **Alert types** choose what it
gets; all but **Newer images available** are on by default. Saved tokens,
topics, passwords, and webhook URLs are never shown again; leave a field blank
to keep it. What triggers each alert is in [Monitoring](#monitoring).

If alerts stop arriving, the destination reads **Failing** and says why, and
**Needs attention** on the server list links to it
([details](#notifications)).

### Back up a server, or every night

To back up now, open the server's **Backups** tab and choose **Create backup**.
The server stops while it copies, then goes back to how it was.

To back up on a schedule, use **Back up every night** in
[Finish setting up](#finish-setting-up), or add one on the server's
**Schedules** tab: choose **Backup** as the action, or keep **Restart** with
**Back up first** ticked.

Backups need nothing set up: the example `compose.yaml` mounts a
`ludock-backups` volume at `/backups`, and Ludock turns storage on there by
itself. **Settings → Backups** shows where backups go and how much space they
use. Each server keeps the 3 newest backups plus the last of each of the 7
most recent days, 4 weeks, and 6 months that have one; Ludock removes the rest
after each new backup. Only backups that can be restored count: one whose
archive is gone from the backup folder, with no second copy, stays listed as
missing and takes no place, and while Ludock can't reach a backup's folder it
removes none of the backups there. To keep one for good, choose **Keep…** from
its menu. When the latest backup failed or was interrupted, the **Backups**
tab says so, with a link to what happened, until the next one succeeds.

A server whose data isn't in a writable volume or bind mount can't be backed
up; its **Backups** tab says so. Give its data a volume where you define the
server and recreate it, and Ludock finds the folder by itself.

Each backup is a plain tar archive of the server's data folders, so it can be
restored without Ludock. To keep backups on another disk, or a second copy
elsewhere, see [Backup storage](REFERENCE.md#backup-storage).

### Restore a backup, or one file

Restoring needs an administrator.

- **A whole backup:** on **Backups**, choose **Restore…** on a backup, type the
  server's name, and choose **Restore game data**. Ludock checks the archive,
  stops the server, takes a safety backup of the current data, swaps the
  backup in, and returns the server to how it was.
- **One file or folder:** choose **Browse files** from a backup's menu, find
  the item, and choose **Restore…** from its menu. Leave **Back up first**
  ticked to keep a backup of the current data, then confirm. Ludock replaces
  only that item, and puts the current copy back if anything fails.

**Browse files** also downloads single files from a backup without changing
anything.

A backup restores onto the same game with the same data folders inside the
server, such as `/data`. If you moved that data to another volume or host
folder, or rebuilt the host, older backups say **Taken when the server's data
lived somewhere else**. They restore the same way, and the confirmation says
the data goes where the server keeps it now. A backup of another game, or of
folders the server no longer has, says why it can't be restored; it can still
be browsed and downloaded, or
[restored by hand](REFERENCE.md#restore-a-backup-without-ludock).

### Update a server

Many game images download the latest game when they start, so **Restart**
updates the game; the server's **Update** tab says so under **Game updates**.
To back up first, tick **Back up first** in the Restart dialog.

To move to a newer image:

1. Open the server's **Update** tab. It says when a newer image is available;
   **Check now** asks the registry right away.
2. Leave **Back up first** ticked and choose **Update server** (**Update
   image** for a game Ludock doesn't know). To skip the backup, untick it and
   type the server's name.

Ludock pulls the image and replaces the container with one that keeps the same
settings, ports, networks, and volumes. If the new one fails to start cleanly,
Ludock puts the old one back and leaves it stopped, since the new version may
already have changed the data. Updates need an administrator; details and
limits are in [Updates](REFERENCE.md#updates).

### Restart a game that hangs

A game can freeze while its container keeps running. For servers with a
[status check](REFERENCE.md#supported-games), or an image health check that
stands in for one, Ludock can restart it:

- Use **Restart automatically** in [Finish setting up](#finish-setting-up), or
- tick **Restart it if the game stops answering** under **Monitoring** on the
  server's **Overview**.

When the game stops answering for the **Wait before alerting** time (two
minutes by default), Ludock restarts it the way **Restart** does and sends an
alert. It leaves alone a server that is stopped, still starting, hasn't
answered since it last started, or is paused for maintenance. If the game
still doesn't answer after the restart (within five times **Wait before
alerting**, at most 30 minutes), or after 3 restarts in 6 hours, Ludock alerts
you and stops trying to restart it for that outage. **Activity** lists each of
these restarts as by "Ludock, because the game stopped answering". A server
whose container exits is left to the restart policy set in the tool that runs
it, such as Compose.

### Manage players

Who is online shows on the server list and in the **Players** section of the
server's **Overview** for [games Ludock can read](REFERENCE.md#supported-games).

- **Kick or ban:** with permission to use the console, click a player's name
  under **Players** and choose **Kick** or **Ban…**.
- **Allowlist and bans (Minecraft):** under **Players**, add or remove
  allowed players by name and lift bans. Whether the allowlist is on is the
  server's own `white-list` setting, which Ludock shows but never changes.

Each action is one console command, recorded in **Activity**.

### Schedule restarts and other actions

On the server's **Schedules** tab, a new schedule starts as a daily restart at
08:00 in your time zone, backing up first when it can. Pick the action (Start,
Stop, Restart, Backup, or Send command), the days or a single date, and choose
**Add schedule**.

A restart, stop, or backup is considerate by default where the game allows: it
waits up to 30 minutes for everyone to leave, then goes ahead, warning anyone
still on in the game 5 and 1 minutes before. The form says in one line what
players will experience. To change it, open **More options**:

- **Wait until no one is online** sets how long to wait, and whether to skip
  the run or go ahead if players are still on.
- **Warn players in the game** sets when the warnings go out.

Everyone who can set up schedules on a server sees all of its schedules and
who set each up, so nobody adds a second nightly backup by accident. Only the
owner or an administrator can change one.

**Pause** a schedule in the list to stop it without deleting it. While a run
waits for players to leave, the schedule, the server's overview, and its
**Activity** tab show **Cancel this run**: that run ends without acting, the
schedule's last result says who cancelled it, and the next run happens as
planned. A run that is queued or warning players
[can be cancelled](#stopping-and-restarting) like any other work. A cancelled
run doesn't count as a failure or send an alert.

Once players have been warned, a run that won't happen as announced is called
off in the game, once: they hear "Server restart cancelled." (or the like)
when you cancel the run, pause or delete its schedule, or change its time or
action, and when it is skipped or its owner loses access. An edit that leaves
the warned run as it was says nothing.

## How things work

### Servers and their state

States follow Docker. A server with a health check shows **Starting** until it
passes and **Unhealthy** while it fails. A stopped server shows **Crashed**
after an error exit, **Out of memory** when the kernel killed it, and
**Force-stopped** when Docker killed it for not shutting down in time, so
recent progress may be lost. A server that stopped without anyone stopping it
shows **Down** once its alert is due, as the alert says.

A server's **Overview** starts with whether it's up, whether the game answers,
the address to give friends, its last backup, and what's scheduled next. Then
come **Players**, **Resources** with their history, and **Monitoring**.
**Details** holds what few people need: the health check, that 100% CPU is one
core, and (for administrators) the ports, container, and image.

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
it meanwhile. A server started with `--rm` can be watched but
[never stopped](REFERENCE.md#servers-started-with---rm).

### Stopping and restarting

When the game's console has a **Save world** shortcut, Ludock sends it and
waits five seconds before stopping, including for a backup, restore, or
update. Docker then gives the game 120 seconds to shut down (more for slow
savers such as ARK, Palworld, Valheim, 7 Days to Die, and Don't Starve
Together), or the container's own `stop_grace_period` if it is longer. Terraria's images never pass Docker's stop signal on to the game,
so Ludock also types its `exit` command, which saves and quits.

The Stop and Restart dialogs say how many players are online when Ludock
knows, such as "2 players are online and will be disconnected.", including
for games it can count but not message. **Create backup**, a restore, and the
**Update** tab say that connected players will be disconnected. When the
server is running and the game can message its players, all five offer
**Warn players and wait 1 minute**, ticked by default. Ludock checks who is
online when the work starts (for an update, once the new image is downloaded;
for a restore, once the backup is checked). If someone is on, or Ludock can't
tell, it tells players in the game, waits the minute (shown in **Activity**),
then goes ahead. If nobody is on, it goes ahead at once, and **Activity** says
nobody was online.

If the game's console or Docker doesn't answer when it's time to warn, Ludock
keeps trying for a minute (**Warning players** in **Activity**) and gives
players a full minute from when the warning goes out; if it never goes out, the
work goes ahead and **Activity** says Ludock couldn't warn players. If Ludock
restarts during the minute, it finishes the wait afterwards; if it was down for
more than five minutes past the time players were given, it leaves the server
alone and says so in **Activity**.

Until the minute is up, the server's page, its row on the server list, and its
rows in **Activity** show **Cancel restart** (or stop, backup, restore, or
update) to anyone who may start it. Cancelling changes nothing on the server
and tells the players in the game that it's off. Work queued behind other work
on the same server can be cancelled the same way while it waits. Cancel is
there only while the work is queued or waiting out the minute: once it saves,
stops, or replaces anything, the work finishes. An update downloads the new
image before it warns players, so it can still be cancelled during that
minute. If the cancel and the end of the wait happen at once, the page says
which won.

**Back up first** in the Restart dialog checks the backup folder, stops
the server, backs it up, and starts it once, unless something outside Ludock
stopped it first, which leaves it stopped after the backup. If the backup
fails, the server stays stopped and Ludock sends an alert, since starting may
update the game with no backup to go back to. If Ludock itself restarts during
the backup, the server is started again when Ludock is back, without the
backup, and **Activity** says so.

Every stop, restart, backup, restore, and update is listed in **Activity**,
with who started it. If Docker itself restarts for a few seconds while work
runs, such as for an upgrade, the work waits for it and carries on. What
happens to work when Ludock or Docker restarts is in the
[reference](REFERENCE.md#after-an-interruption).

### Consoles

**Console** on a server opens up to three views. For a game without a console
the button says **Logs**.

- **Logs**: what the server prints.
- **Console**: the game's own console, with shortcut buttons for common
  commands. Up and Down repeat commands from this visit.
- **Shell**: commands run inside the container, for administrators. Each gets
  60 seconds, and the image needs `/bin/sh` and `timeout`.

Network consoles (RCON, WebRCON, Telnet) need the console turned on in the
game. Ludock finds its password in the variables images usually use, such as
`RCON_PASSWORD`, `ADMIN_PASSWORD`, or `TELNET_PASSWORD`, and for Minecraft and
Factorio also the one the image generates. If yours uses another name, set
[`ludock.console.password-env`](REFERENCE.md#labels). Terraria, Bedrock, and
Vintage Story read commands from the server's input, which needs
`stdin_open: true` on the container (and `StdinOnce` off, outside Compose) and
an image that passes input to the game; their replies appear only in
**Logs**, which is also where Ludock reads who's online, so the list command
it sends shows there each time. Ludock reaches consoles from inside the container's
network, so the console port needn't be published.

### Files

**Files** opens the server's data folders: the writable volumes and bind mounts
Ludock considers safe, except where an image keeps the server it downloads,
such as Valheim's `/opt/valheim`. It shows container paths, never host paths.

- Upload by dropping files on the page or with **Choose files**. Each upload
  replaces the destination only once it is complete.
- Choose a text file to preview it, and **Edit** to change it. If the file
  changed since you opened it, the save is refused and your text stays.
- **Unpack** extracts a zip, tar, or tar.gz (including `.mcworld`, `.mcpack`,
  and `.mrpack`) into a new folder beside it. It never replaces anything.
- **Select** picks several entries to download as one tar, move, or delete.
- A folder mounted separately inside another, such as a mods folder, can't
  be deleted, renamed, or moved with what holds it; change what's inside it.
  Neither can a folder holding a restore's hidden `.ludock-restore-*` folder,
  which can be the only copy of the server's previous data.

Stop a game before replacing a world or configuration it has open. Many
images write some settings from their environment variables each time they
start, such as `MOTD` into Minecraft's `server.properties` on
`itzg/minecraft-server`, so an edit to one of those is undone at the next
start; change it where you define the server instead. Ludock never follows
symbolic links, and blocks system and sensitive paths and Docker sockets.
Volume subpaths and `--volumes-from` are in
[Unusual setups](REFERENCE.md#shared-and-nested-mounts). To limit access to
some folders, set [`ludock.files`](REFERENCE.md#labels). Ludock's own working
folders, named `.ludock-…`,
[stay out of Files](REFERENCE.md#ludocks-folders-in-server-data). Size and
time limits are in the [reference](REFERENCE.md#limits).

### Backups

- **Downtime:** a backup stops the server for the whole copy, then returns it
  to its previous state. Live backups aren't supported.
- **Readiness:** the **Backups** tab checks the destination, free space, the
  data folders, and whether another container writes the same data, without
  stopping anything, and says what's wrong only when something is. Every
  backup, restart or update with a backup first, and restore checks again
  before it warns players or stops the server, so one that can't work leaves
  the server running.
- **Space:** Ludock keeps 5 GiB free on the backup disk by default. A backup
  that runs out of room fails and says why. After each successful backup, if
  the next one might not fit, Ludock removes the server's oldest backups, never
  the 3 newest that can be restored or kept ones. A backup whose archive is
  gone doesn't count toward the total limit, so it never stops new backups;
  one in a folder Ludock can't reach counts toward the limit for new
  backups, but never makes Ludock remove others.
- **Cancelled backups:** a forced stop, a start from another tool, a replaced
  container, or another running container writing the same data cancels the
  copy.
- **Contents:** the data folders' ordinary files and folders, with their
  permissions, owners, and modification times. Links and special files are
  refused, and so is a data folder with another mount inside it; the
  **Backups** tab says which.
  [Shared and nested mounts](REFERENCE.md#shared-and-nested-mounts) has the
  exact rule.
- **Safety backups** taken before a restore or update follow the same
  retention rule and are labeled in the list.
- **Records:** Ludock lists only archives it made, from records in
  [its own data](REFERENCE.md#ludocks-own-data); other archives can't be
  imported, and Ludock never changes or removes them.
- **Who:** members with **Full access** can create backups. Browsing,
  downloading, restoring, keeping, and deleting are for administrators, since
  archives can hold configuration secrets.

If a restore fails, check **Activity**: its entry says what the data was left
as. Usually the restore was undone and the data is as it was. If something
outside Ludock started the server partway, the restore stops and leaves it
running; when the previous data was already moved out, **Needs attention**
offers administrators **Undo the restore**, which keeps anything the game wrote
meanwhile. [A restore that stopped partway](REFERENCE.md#a-restore-that-stopped-partway)
has every case.

### Schedules

- A schedule runs with its owner's current access. Members with **Full
  access** see every schedule on the server and manage their own;
  administrators manage all of them. Someone else's schedule says who can
  change it in place of its buttons.
- If Ludock can't run a schedule on time, such as while it's restarting, a
  repeating schedule still runs within 15 minutes and otherwise skips that
  time; a one-time schedule runs within five minutes and otherwise is recorded
  as missed. A daylight-saving hour that doesn't exist is skipped, and a
  repeated one runs once.
- **Back up first** needs the server running when the schedule runs, and then
  works as in the [Restart dialog](#stopping-and-restarting).
- **Wait until no one is online** works on restarts, stops, and backups on
  servers whose players Ludock can read, and waits 1 to 360 minutes. A player
  count Ludock can't read counts as players online. Warnings need **Go ahead
  anyway** and count down to the end of the wait.
- **Warnings** need a game that can message its players (see
  [Supported games](REFERENCE.md#supported-games)) and the owner's permission
  to send console commands. They are best effort: each is sent at most once,
  and skipped when the console doesn't answer.
- **Send command** sends one line, up to 500 characters, through the game
  console. It doesn't start a stopped server. Anyone who can see the schedule
  and send console commands can read the command, so leave passwords out.

### Players

While someone has the server list open, Ludock reads who is online on its
servers about every 30 seconds, and while a server's page is open, on that
server. While nobody has Ludock open, only monitored games with a status check
are asked, about every 2 minutes, and only through the status check. Readings
stay in memory, and a count Ludock can't read shows as unavailable, never as
empty.

Ludock asks the game's console only when the status check can't say the list is
unchanged, since games such as Minecraft log every console connection: once the
console has listed the players, a status check that names the same players, or
says nobody is online, stands in for it. Games with a working status check but
no player list, or no console password, still show how many are online.

A kick or ban targets the player by the name or ID the game's latest list
gave, and only when it is wholly in the game's own form, so it can't carry
quotes, line breaks, or a selector such as `@a`. On Minecraft Ludock also
checks the name against the player's account first, so a nickname or a
plugin's list never targets someone else; on such servers, use the console.

### Monitoring

Monitoring watches each server's container, its game, its disks, and its
memory. The server's **Overview** shows how monitoring sees it under
**Monitoring**; there an administrator opens **Monitoring settings** to turn
it off, change its levels, or **Pause alerts for maintenance**.

| Alert | When |
| --- | --- |
| Server unavailable | The container stops, or fails its health check, without Ludock stopping it, for longer than **Wait before alerting** (120 seconds by default) |
| Game not answering | A game with a status check answered since its container started, then stops answering for that long; for a game whose image's health check stands in, the check passed, then Docker calls it unhealthy for that long |
| Disk nearly full | A data folder's disk reaches 90% full |
| Memory near its limit | A container with a memory limit stays at 90% of it for 5 minutes |

Each alert is sent once, with another when it recovers. A server that stopped
by itself shows in **Needs attention** with **Start**, which is also the
highlighted button on a stopped server's row and page. Stops and work started
through Ludock don't alert, and a server that was already stopped when Ludock
found it stays quiet until it runs. A game still starting, or whose status
check is off or wrong, never counts as not answering. Ludock remembers that a
game answered, so one that hangs while Ludock itself restarts is still caught;
the wait starts again when Ludock is back, unless the alert was already sent.
Disks are measured about every 10 minutes while someone has Ludock open and
every 30 otherwise.

### Notifications

Each destination's **Alert types**:

- **Server problems:** a server is unavailable, its game stops answering, Ludock
  can't reach Docker, a disk is nearly full, or memory is near its limit, and
  each recovery.
- **Failed backups and schedules:** a backup, scheduled action, second copy,
  or save of Ludock's own data fails, or a server is left stopped after a
  failed backup. A backup cut short by Ludock restarting or crashing alerts
  once it is back.
- **Restores and updates:** a restore or update fails, or a restore is cut
  short by Ludock restarting. One that succeeds shows its result in the panel,
  where it was started.
- **Newer images available:** a newer image is published for a server, once per
  version. Off by default.

Alerts read "<server>: <what happened>." and name servers and container paths,
never images or host paths. Each alert is tried up to five times, over about
15 minutes; a test is tried once.

A destination that is on reads **Failing** when its alerts aren't getting
through: an alert has failed three times in a row (about three minutes after
it was raised), or a test failed, with nothing delivered since. It says when
it was last tried and why, and **Needs attention** shows it to administrators
with a link to fix it. It reads **On** again once any delivery to it
succeeds, and the warning goes once it's turned off.

**Settings → Advanced → Recent deliveries** lists the latest 50, each with the
alert as it was sent, its type, and when it was queued, and offers **Retry** on
failed ones. Turning a destination off holds its deliveries until it's on
again.

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
open. **Users** shows a link that hasn't been used yet, with **Revoke link**.
Too many wrong passwords or codes from one address are refused for a while.

### Activity

**Activity** lists work on the servers you can use, newest first, one row for
each piece of work with its latest result. **Filter** opens filters for
server, person, action, result, and dates; it says how many are on, and a link
to a filtered page opens with them showing. Each server's **Activity** tab
lists its own, with filters for the results and kinds of work it has had.
Rows update while work runs, and work that
[can still be cancelled](#stopping-and-restarting) offers **Cancel** on its
row.
Administrators can choose **Show → Everything** to add sign-ins, account
changes, and settings changes.

**Needs attention** on the server list shows recent failures until the same
kind of work succeeds on that server, such as a good backup after a failed one.
An interrupted restore, a force-stop, and a failure that asks for an
administrator stay listed. **Activity** keeps everything.
