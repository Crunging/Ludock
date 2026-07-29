# Docker Game Manager

A Docker-native game server control panel. Docker remains the source of truth
for servers, while SQLite stores panel
accounts, sessions, roles, and audit history.

## Features

- **Docker-native**: Discovers and controls existing containers through `docker.sock`
- **Label-driven discovery**: Containers opt-in via `game-panel.enable=true`
- **Game console**: Native RCON, WebRCON, Telnet, and process-console commands
- **Live logs**: Stream container stdout and stderr over WebSocket
- **Administrator shell**: Separate advanced container troubleshooting access
- **File manager**: Browse, download, upload, rename, and remove scoped server files
- **Real-time updates**: Docker event stream pushes state changes instantly
- **Multi-user access**: Administrator, operator, and viewer roles
- **Account security**: Scrypt password hashing, revocable sessions, and audit history
- **Recreation-aware**: Automatically discovers replacement containers that retain the management labels

## Quick Start

### Add Labels to Your Game Servers

```yaml
# docker-compose.yml
services:
  minecraft:
    image: itzg/minecraft-server
    labels:
      game-panel.enable: "true"
      game-panel.name: "Survival Server"
      game-panel.game: "minecraft"
    environment:
      EULA: "TRUE"
    ports:
      - "25565:25565"
    volumes:
      - minecraft-data:/data
    tty: true
    stdin_open: true

volumes:
  minecraft-data:
```

### Run the Panel

```bash
# Optional: configure deployment overrides
cp .env.example .env

docker compose up -d
```

Or with Docker directly:

```bash
docker run -d \
  --name game-panel \
  -p 3000:3000 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v game-panel-data:/data \
  ghcr.io/crunging/docker-game-manager:latest
```

Images are published for `linux/amd64` and `linux/arm64`, so the same tag works
on x86 servers, Apple Silicon, and 64-bit Raspberry Pi.

Stable releases are published as `latest`, `MAJOR`, `MAJOR.MINOR`, and
`MAJOR.MINOR.PATCH`. The moving tags only ever advance: a patch released from an
older branch updates its own series without taking `latest` backwards. Builds
from every commit to `main` are published as `nightly` and an immutable
`nightly-RUN-SHA` tag, and the ten most recent nightly releases are retained.

The panel will be available at `http://localhost:3000`. On first launch, create
the initial administrator account in the browser within five minutes. If the
setup window expires, restart the panel to reopen it.

## Labels

| Label | Required | Description |
|---|---|---|
| `game-panel.enable` | ✅ | Set to `"true"` to make a container visible |
| `game-panel.name` | ❌ | Display name (defaults to container name) |
| `game-panel.game` | ❌ | Game type identifier (e.g., `minecraft`, `valheim`) |
| `game-panel.console` | ❌ | Console adapter override (see the support table below) or `disabled` |
| `game-panel.console.host` | ❌ | RCON/Telnet host override when the container IP is not reachable |
| `game-panel.console.port` | ❌ | RCON/Telnet port override |
| `game-panel.console.password-env` | ❌ | Name of the game container environment variable holding its console password |
| `game-panel.files` | ❌ | Comma-separated container paths exposed in the file manager |

Minecraft servers automatically expose `/data`. Valheim servers using
`community-valheim-tools/valheim-server` or `lloesche/valheim-server`
automatically expose `/config`. Other images can opt in explicitly:

```yaml
labels:
  game-panel.files: "/config,/backups"
```

The panel will never expose `/` as a file root.

## Authentication

Interactive users sign in with a username and password. Passwords are hashed
with scrypt and browser sessions are stored as opaque, revocable, HttpOnly
cookies. Administrators can create accounts, assign roles, reset passwords,
disable access, and inspect the audit log.

With a new database, the first visitor can create the administrator account
during a five-minute window after the panel starts. If setup is not completed
in time, it locks until the panel is restarted. Do not expose an unconfigured
panel to an untrusted network.

| Role | Access |
|---|---|
| Administrator | Server control, game console, file management, container shell, user management, and audit log |
| Operator | Server control, game console, and file management |
| Viewer | Read-only server status, logs, and file downloads |

`PANEL_API_TOKEN` is optional and intended only for API automation or
emergency administrative access. It is not the normal browser sign-in method.
Generate a 256-bit value with `openssl rand -hex 32` and send it as
`Authorization: Bearer <token>`.

Because the token grants unconditional administrator access and is exempt from
the login throttle, tokens shorter than 32 characters are ignored and logged as
an error at startup rather than accepted.

Terminate external access at an HTTPS reverse proxy. Secure cookies, origin
checks, WebSockets, and HSTS work without additional panel configuration.

`TRUSTED_PROXIES` is optional. It lets audit logs and rate limits use the client
IP supplied by the directly connected proxy. Otherwise, they use the proxy IP.
Setting it also restricts `X-Forwarded-Proto` to that proxy, so a directly
connected client can no longer claim its own request arrived over HTTPS.

For Docker, inspect the proxy address on its shared network and add `/32`:

```bash
docker inspect -f '{{(index .NetworkSettings.Networks "proxy").IPAddress}}' traefik
```

Replace `traefik` with `nginx`, `nginx-proxy-manager`, or `caddy` as needed.
Prefer a static proxy IP or dedicated proxy-only network. Multiple entries may
be comma-separated; do not trust networks containing untrusted systems.

> [!WARNING]
> Access to the Docker socket is effectively root-level access to the Docker
> host. A read-only bind mount does not make Docker API operations read-only.
> Do not expose this panel directly to the public internet.

## Development

```bash
# Install dependencies
corepack enable
pnpm install

# Run both frontend & backend in dev mode
pnpm dev

# Backend only
pnpm backend:dev

# Frontend only (with API proxy to :3001)
pnpm frontend:dev

# Type-check, lint, test, and build
pnpm check
```

See [TESTING.md](./TESTING.md) for the release acceptance checklist.

### Tech Stack

- **Backend**: Node.js 24, TypeScript, Express 5, Dockerode, ws, SQLite
- **Frontend**: React 19, Vite, xterm.js
- **Monorepo**: pnpm workspaces

## Architecture

```
┌─────────────┐     ┌──────────────┐     ┌──────────────┐
│   Browser    │◄───►│   Backend    │◄───►│ Docker Engine│
│  React SPA   │ WS  │  Express+WS  │sock │  (docker.sock)│
│  xterm.js    │     │  Dockerode   │     │              │
└─────────────┘     └──────────────┘     └──────────────┘
```

### Game console, logs, and container shell

- **Output**: `container.logs({ follow: true })` → WebSocket → xterm.js
- **Game commands**: WebSocket → game-native RCON, WebRCON, Telnet, or stdin
- **Administrator shell**: WebSocket → `/bin/sh` via `container.exec()`

The panel selects a console adapter from `game-panel.game`:

| Game identifiers | Transport | Default port |
|---|---|---:|
| `minecraft` | Bundled `rcon-cli` executable | Image-managed |
| `factorio` | Source RCON | 27015 |
| `palworld` | Source RCON | 25575 |
| `ark`, `ark-survival-evolved`, `ark-survival-ascended`, `asa` | Source RCON | 27020 |
| `cs2`, `csgo`, `counter-strike-2` | Source RCON | 27015 |
| `project-zomboid`, `projectzomboid` | Source RCON | 27015 |
| `conan-exiles`, `v-rising` | Source RCON | 25575 |
| `rust` | Rust WebRCON | 28016 |
| `7-days-to-die`, `7dtd` | Telnet | 8081 |
| `terraria` | Container process stdin | N/A |

The explicit adapter values are `minecraft-rcon`, `source-rcon`,
`rust-webrcon`, `telnet-console`, and `stdin-console`. This allows compatible
custom images and modded servers to opt in even when their game identifier is
not in the table.

For network consoles, enable the protocol in the game server itself. Keep the
password in the game container's environment, then tell the panel only the
environment variable's name:

```yaml
labels:
  game-panel.console.password-env: "RCON_PASSWORD"
  # Override these only when they differ from the game default:
  game-panel.console.port: "27015"
  # game-panel.console.host: "game-server"
environment:
  RCON_PASSWORD: "${RCON_PASSWORD}"
```

The panel automatically recognizes common password variables including
`RCON_PASSWORD`, `ADMIN_PASSWORD`, `SERVER_ADMIN_PASSWORD`,
`ARK_ADMIN_PASSWORD`, and `SRCDS_RCONPW`. It reads the value on the backend for
each command; the password is never sent to the browser or written to the
audit log.

By default the panel connects to the game container's Docker IP. Put the panel
and game on a shared Docker network. If that is not possible, publish the
console port and set `game-panel.console.host` and
`game-panel.console.port` to an address reachable from the panel container.
Do not expose RCON or Telnet directly to the public internet.

Minecraft servers labeled `game-panel.game=minecraft` use the
`minecraft-rcon` adapter. In the Game Console, enter native commands directly:

```text
difficulty hard
whitelist on
whitelist add PlayerName
say Server restart in 5 minutes
```

The `itzg/minecraft-server` image includes `rcon-cli` and enables RCON by
default. Other Minecraft images must provide a compatible `rcon-cli` executable
inside the container, or the adapter should be disabled with
`game-panel.console=disabled`.

The panel never calls `container.attach()`, so closing the browser does not
detach or stop the main game process. The container shell is deliberately
separate from the game console and restricted to administrators.

Console support remains game-specific. Vanilla Valheim exposes administration
through its in-game F5 console rather than a standard remote endpoint, so it
has live logs but no built-in command adapter. Satisfactory uses a separate
HTTPS management API and currently has logs only. A Valheim administration mod
can opt into an appropriate adapter with `game-panel.console`; do not use the
administrator-only container shell as a substitute for routine game commands.

### Worlds, mods, and configuration files

Open **Files** from a server card to browse its configured storage roots.
Administrators and operators can drag files into the current folder, create
folders, rename entries, download individual files, download folders as tar
archives, and delete entries. Viewers have browse and download access only.

For the common Valheim image, useful locations under `/config` include:

- `worlds_local` for `.db` and `.fwl` world files
- `bepinex/plugins` for BepInEx mod files
- `bepinex/config` for mod configuration

Stop the game server before replacing or deleting an active world. Game
servers may keep world data in memory and write it during shutdown, which can
overwrite an upload or leave a mismatched world pair. Keep both Valheim
`.db` and `.fwl` files together and take a backup first.

Uploads are streamed rather than buffered in panel memory. The default maximum
file size is 2 GiB and can be changed with `MAX_UPLOAD_BYTES`.

Volume-backed roots remain available while a game server is stopped. The panel
uses a short-lived, capability-dropped helper container with no network access
and removes it after each operation; it never starts the game container.
`alpine:3.22` is the default helper image and can be changed with
`FILE_HELPER_IMAGE`. Files stored only in a container's writable layer require
that container to be running, so persistent game data should always use a
Docker volume or bind mount.

## Security model

- Only containers labeled `game-panel.enable=true` may be listed or controlled.
- Lifecycle, game-console, log, and shell access enforce that opt-in boundary.
- Game-console commands do not pass through a shell.
- File access is limited to normalized roots declared by the panel's labels or
  a known game-image convention.
- File paths reject traversal and symbolic-link escapes.
- Container-shell access is restricted to administrators.
- Interactive access requires a persistent account session.
- Password changes and administrator resets revoke existing sessions.
- State-changing browser requests are restricted to the same origin.
- Security headers include a restrictive content security policy.
- The public `/api/health` endpoint reports only panel and Docker connectivity.

The Docker socket grants host-level control even when bind-mounted read-only.
Expose the panel only through HTTPS, keep the host and panel patched, and grant
accounts only to trusted operators.

## Operations

Account data is stored at `/data/panel.db` in the persistent `panel-data`
volume. Do not run the panel without this volume: deleting it removes accounts,
sessions, and audit history.

To take a consistent backup with Docker Compose:

```bash
docker compose stop panel
docker compose cp panel:/data/panel.db ./panel.db.backup
docker compose start panel
```

If all administrators lose access, stop the panel and reset an existing
account from the host:

```bash
docker compose stop panel
docker compose run --rm \
  -e PANEL_RECOVERY_PASSWORD='choose-a-new-long-password' \
  panel node packages/backend/dist/recovery.js admin
docker compose start panel
```

The recovery command revokes every session for that account and records the
event in the audit log. Avoid leaving the recovery password in shell history;
an environment file or an interactive shell is safer on shared systems.

## License

See [LICENSE](./LICENSE).
