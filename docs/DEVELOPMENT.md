# Development

## Local development

Install the Go version in [`go.mod`](../go.mod), then run:

```sh
scripts/dev.sh
```

Open <http://127.0.0.1:3000> and use the setup code printed in the output.
The script builds Ludock and `cmd/fakedocker`, an in-memory Docker Engine API,
and keeps their data in `data/dev/`. The fake daemon starts a running Minecraft
server, stopped Valheim, Palworld, and Enshrouded servers, and containers
Ludock doesn't add by itself (an unrecognized game, one labeled out, and
Portainer). Rerun the script after changes, and set `LUDOCK_LISTEN` for another
address.

- **Templates:** pages are [templ](https://templ.guide) components in
  `internal/web/views`. After editing a `.templ` file, run
  `go tool templ generate -path internal/web/views` and commit the generated
  `_templ.go` files; CI fails when they are out of date.
- **Assets:** `internal/web/assets` is embedded into the binary: `ludock.css`,
  `ludock.js` (behaviors are `data-*` attributes; pages run no inline scripts),
  page scripts such as `console.js` and `files.js`, the fonts, and vendored
  htmx 4, its SSE and preload extensions, and xterm.js, each with its license.
  The Content Security Policy allows no evaluated script, so htmx attributes
  never use `js:`, trigger filters, `hx-on`, or the `hx-live` extension.
- **Real Docker:** set `LUDOCK_DOCKER_SOCKET` to a dedicated test daemon with
  disposable game data, never one running real servers. Native runs also need
  `LUDOCK_HELPER_IMAGE`, a Ludock image pinned by digest, for file and backup
  helpers.

## Checks

```sh
gofmt -l cmd internal
go vet ./...
go tool staticcheck ./...
go test ./internal/...
cd e2e && npm ci && npx playwright test
```

The Playwright journeys in `e2e/journeys` are the main suite. They build the
real binary and `cmd/fakedocker`, start one of each on fresh temporary data for
every worker, and use the product as people do: one quick journey per thing a
person does, extended before another is added. Go tests are for what a journey
can't reach (see "Checks and tests" in [`AGENTS.md`](../AGENTS.md)). A journey
that changes a server's state, files, schedules, backups, or sharing takes the
`ownServer` fixture: a running Minecraft server like Survival that only it
sees, added for it and removed after (`addServer()`); `otherServer` gives it a
second, so two servers' timers run side by side. Journeys that only look stay on the shared servers. Workers
run whole files, so a file must pass on a fresh install without the files
before it and leave each shared server as the next file expects it
(`restoreServer()`). A file whose journeys each pass on their own can set
`test.describe.configure({ mode: "parallel" })` to spread them over workers.
Accounts a journey creates need names of their own too, such as one built
from `ownServer.container`, since a retry runs on the same install.
`--workers=1` runs every file on one server. CI retries a failed journey once
to show whether it's flaky, but a flaky journey still fails the run.

`journeys/slow` holds one journey per chain a person watches happen by
itself: a warned restart, backup, or restore counting down, a hung game
restarted, a schedule firing and one waiting for players, a disk alert, an
alert reaching a webhook, and an outage on an open page; the notification
settings journeys share that webhook's file. They run in parallel mode and
wait on Ludock's own clocks: player and game readings come every 30 seconds on the
15-second tick that also runs monitoring and schedules, an alerting disk is
measured every minute, and a warned restart waits one. Variations of those
rules are Go tests with an injected clock, not more journeys. Shorten what
Ludock lets you, such as the shortest "Wait before alerting", and start a
server through Ludock after changing it, which replaces an earlier stop's
quiet time with the new wait. The `slow` project runs beside `quick`,
everything else. Each worker needs about 600 MB, and a machine that runs short
of memory stalls every journey at once, so a local run takes workers for at
most half the memory free when it starts, from two to nine. CI runs each
project in two jobs, with a worker for each slow journey, so a slow job takes
about as long as its longest journey, the hung game across a restart.

[`e2e/journeys/fixtures.mjs`](../e2e/journeys/fixtures.mjs) holds the shared
helpers: `asAdmin()` signs a file's journeys in as the setup administrator,
`session()` opens another browser profile, `dockerAction()` changes a container
behind Ludock's back, `gameCommand()` types into a server's game console (the
fake games take `fake-join` and the like), and `fake()` calls the fake
daemon's controls, such as `/fake/disk`. Every page fails its journey on a script error or a browser
dialog. A journey that needs a fresh install, such as the first-run checklist,
starts its own stack with `freshStack()` from `setup.mjs`.

The fake daemon is `internal/docker/fakedocker`, one file per part of Docker:
`containers.go` for create, inspect, and the actions on a container,
`exec.go`, `images.go` with the registry, `events.go`, `state.go` for
changing containers from Go, and `faults.go` for failures and outages.
Journey controls are all in `control.go`, and `helpers.go` runs Ludock's
helpers in-process for both the binary and Go tests. `cmd/fakedocker` seeds the
servers (`seed.go`) and the fake games' consoles (`games.go`).

Locally, run `npx playwright install chromium` once, or point the journeys at
an installed Chromium with `LUDOCK_E2E_CHROMIUM=/path/to/chrome`. Playwright's
own headless shell needs much less memory than a full Chrome. Each stack
listens on a port the system hands out; `LUDOCK_E2E_PORT` picks consecutive
ports from that one instead. `LUDOCK_E2E_LOGS=1` prints the servers' output and
`LUDOCK_E2E_KEEP=1` keeps their temporary data. Backups keep 5 GiB free in
their folder, so that data goes in the system temporary folder only when it
has 8 GiB free, and in `~/.cache/ludock-e2e` otherwise.

### Measuring

Build with `-tags pprof` and set `LUDOCK_PPROF` to a loopback address such as
`127.0.0.1:6060` to serve CPU and allocation profiles under `/debug/pprof/`
and the number of SQL statements run so far at `/debug/queries`; release
builds have neither. The fake daemon lists every Engine API request at
`/fake/requests` and counts its game consoles' connections at `/fake/rcon`.

### Screenshots

The README screenshots come from `fakedocker -demo`.
[`e2e/screenshots.mjs`](../e2e/screenshots.mjs) builds both binaries, sets up a
demo install, and saves the pages to `docs/screenshots`. Rerun it after a
change to those pages:

```sh
cd e2e && node screenshots.mjs
```

### Container journeys

`e2e/docker` runs the built image against real Docker. It deploys
[`compose.yaml`](../compose.yaml) with a test override, starts a small Compose
game stack, and checks the hardened runtime, file helpers started from Ludock's
own image, backup and restore, the shell, an image update, and account
recovery. Run them only against a disposable daemon:

```sh
docker build --pull --no-cache-filter runtime -t ludock:test .
cd e2e && LUDOCK_DOCKER_E2E=disposable-daemon npx playwright test -c docker.config.mjs
```

### Real game servers

[`e2e/real/rig.mjs`](../e2e/real/rig.mjs) brings up the local image beside
one real game server, as an owner would, for trying changes by hand. It never
runs in CI. Use a disposable daemon: it publishes Ludock on `127.0.0.1:3000`
and the game on loopback only, and `remove` and `down` take apart only the
containers, volumes, networks, and files of its Compose projects, named
`ludocktest-…`.

```sh
docker build -t ludock:test .
cd e2e
export LUDOCK_DOCKER_E2E=disposable-daemon
MINECRAFT_EULA=TRUE node real/rig.mjs up   # Minecraft Java, the default
node real/rig.mjs bot Steve                # a headless player; Ctrl-C leaves (Minecraft or Terraria)
node real/rig.mjs remove minecraft
node real/rig.mjs up terraria              # another game beside the same Ludock
node real/rig.mjs down
```

[`e2e/real/games.mjs`](../e2e/real/games.mjs) defines each game: its image,
environment, ports, data folder, memory limit, and the log line or health check
that says it's ready. It has every game in "Checked on real servers" in the
[reference](REFERENCE.md); add one there. One game runs at a time. `up`
finishes setup with the journeys' administrator and prints how to sign in.
`MINECRAFT_EULA=TRUE` accepts the
[Minecraft EULA](https://aka.ms/MinecraftEULA) for the test servers; Valheim
downloads its server from Steam anonymously.

For Terraria, `bot` runs `real/terraria-bot.mjs`, which speaks just enough of
the game's protocol (release 326, Terraria 1.4.5.8) to join and hold a slot;
Terraria bans by address too, and every bot connects from the same one. The
Minecraft bot (mineflayer, installed into the rig folder on first use) joins
Minecraft in offline mode, chats once a minute, and prints every message,
kick, and silence it sees, such as a warning before a restart. Game data lives
in the rig folder (`LUDOCK_RIG_DIR`, a temporary folder by default) so you can
read it from the host. Servers the images download and the second backup copy
live in named volumes, since the temporary folder may be kept in memory.
Images stay after `down`; `LUDOCK_RIG_JAVA_IMAGE` picks another Minecraft
image, such as an older tag to try an update.

#### What the rig has checked

The reference's "Checked on real servers" lists what owners need. What was
seen on each, on a machine with 8 GB of memory and a 25 GB download limit:

- **Minecraft** (vanilla 1.21.11): players joined with the bot. With zero, one,
  and two online, Ludock opened RCON, which the game logs, only when a page
  opened or someone joined; the status check's player sample stood in the rest
  of the time.
- **Terraria** (1.4.5.8): with two bots online, Ludock listed both from
  `playing` and kicked and banned them; the list follows within a few seconds.
  `save` before a stop, then `exit`, and `say` warns players.
- **Factorio:** who's online, kick, and ban answer as expected; warnings appear
  in chat from `<server>`; `/server-save` saves, and the game saves again when
  it stops.
- **Valheim:** with `SERVER_PUBLIC` on, Ludock restarted a hung server by
  itself once automatic restart was on.
- **Vintage Story:** `/autosavenow` saves before a stop, and the game saves
  again when it stops; `/announce` warns, `/list clients` lists, and `/kick`
  kicks.
- **Minecraft: Bedrock Edition:** `list` and `kick` work, and the image saves
  the world when Docker stops it. With the game process
  frozen, Docker called it unhealthy within two minutes, and Ludock restarted
  it and said it answered again. Warnings weren't seen in game, since joining
  needs a Microsoft account.
- **Palworld** (1.0.5): who's online, kick, and ban (as `steam_<ID>`) work;
  the REST API logged the warning as accepted. The saves are about 0.3 MB on a
  new world beside a 3.5 GB server; a backup and its restore each took about
  15 s.
- **Core Keeper:** Docker's stop saves within a few seconds. The world is about
  0.1 MB beside a 570 MB server; a backup and its restore brought back the
  same world.
- **V Rising** (1.1): the game took Ludock's restart warning. Its own
  `shutdown` saves but waits at least a minute. The saves are about 20 MB
  beside a 1.9 GB server; a backup and its restore brought them back, and the
  game loaded the latest.
- **Enshrouded:** status check and count on 15637; Docker's stop saves within
  a few seconds. On both images a backup and its restore brought back the same
  world.
- **7 Days to Die** (3.4): `listplayers` was read on an empty server;
  `saveworld` saves before a stop, `say` posts in global chat, and LinuxGSM
  saves and shuts down in about 10 seconds. The saves are about 17 MB on
  Navezgane beside a 17 GB server; LinuxGSM's logs hold a `steam` link that
  stopped every backup until they were left out. A backup and its restore
  brought back the same saves.
- **Project Zomboid:** the image passes empty arguments that Build 42 refuses,
  and so does Build 41.78 on `GAME_VERSION=legacy41`.

On games other than Minecraft and Terraria, who's online was read on an empty
server, and the replies with players come from the game's own text. Not run:
Counter-Strike 2 (60 GB of disk), Conan Exiles (at least 8 GB of memory), Rust
(12 GB), Satisfactory (8 to 16 GB), both ARKs (6 to 13 GB), and Don't Starve
Together, which needs a server token from Klei.

## CI and releases

The image cross-compiles a static binary (`CGO_ENABLED=0`) and copies it into
Alpine. The binary's subcommands are `serve` (the default),
`recover <username>`, `restore-data <file>`, `healthcheck`, `helper …` (run
inside helper containers), and `version`.

- **CI** runs on every PR and every push to `main` that changes more than
  Markdown or the license: formatting, vet, generated templates, race-enabled
  Go tests, the journeys (each of their two projects in two jobs),
  the pin check, the container journeys on AMD64 and ARM64 (one job per
  journey file), and an image scan. Runs reuse the Go
  build cache and image layers from `main`'s latest run, except the runtime
  stage, so `apk upgrade` picks up security fixes.
- **Weekly:** `govulncheck`, `npm audit` for the journey dependencies, and a
  scan of the latest published image.
- **Nightly:** every push to `main` publishes the image to `ghcr.io` as
  `nightly`.
- **Releases:** Release Please keeps a release PR open on `main`. Merging it
  tags the version, publishes the GitHub release and changelog, and publishes
  `X.Y.Z`; stable releases also move `X.Y`, `X`, and `latest`. Don't edit
  versions, `CHANGELOG.md`, or the release manifest by hand, or move published
  tags.

PRs opened with the default `GITHUB_TOKEN` don't trigger CI. For CI on release
PRs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a fine-grained token for
this repository with Contents, Issues, and Pull requests write access.

## Dependency updates

```sh
go list -u -m all
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
(cd e2e && npm outdated)
(cd third_party/browser && npm outdated)
```

- **Go:** update `go.mod`'s `go` line and the Go image digest in the Dockerfile
  together. Helpers reuse the running Ludock image, so they always match it.
- **Vendored scripts:** htmx, its SSE and preload extensions, and xterm.js
  live in `internal/web/assets/vendor` with their licenses. Each file is an exact copy
  of the published file `third_party/browser/package.json` maps it to, so the
  security scan can audit those versions. To update one, change its version
  there, run `npm install --package-lock-only` in that folder, copy the new
  file from the package, run `scripts/check-vendor.sh`, and rerun the
  journeys.
- **Pins:** workflows pin every action to a commit SHA (with its release as a
  comment) and every image to a digest; `scripts/check-pins.sh` enforces this.
- **Image scans:** [`scripts/scan-image.sh`](../scripts/scan-image.sh) rejects
  fixable medium, high, and critical vulnerabilities.
