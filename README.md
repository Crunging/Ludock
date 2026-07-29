# Docker Game Manager

A control panel for existing Docker game servers. Docker remains the source of
truth for servers; the panel stores accounts, sessions, roles, and audit history
in SQLite.

## Features

- Discovers explicitly labeled containers through the Docker socket
- Starts, stops, restarts, and monitors existing game servers
- Streams logs and supports RCON, WebRCON, Telnet, and process-console commands
- Manages files within configured container paths
- Provides administrator, operator, and viewer roles with an audit log
- Tracks replacement containers that retain the management labels

## Quick Start

### Add Labels to Your Game Servers

```yaml
# compose.yaml
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

Download [`compose.yaml`](./compose.yaml). To configure optional settings, also
download [`.env.example`](./.env.example) as `.env`. Start the panel with:

```bash
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

Images support `linux/amd64` and `linux/arm64`. Stable releases use `latest` and
versioned tags; builds from `main` use `nightly`.

Open `http://localhost:3000` and create the initial administrator account within
five minutes. Restart the panel if the setup window expires.

## Labels

| Label | Required | Description |
|---|---|---|
| `game-panel.enable` | Yes | Set to `"true"` to make a container visible |
| `game-panel.name` | No | Display name (defaults to container name) |
| `game-panel.game` | No | Game type identifier (e.g., `minecraft`, `valheim`) |
| `game-panel.console` | No | Console adapter override (see the support table below) or `disabled` |
| `game-panel.console.host` | No | RCON/Telnet host override when the container IP is not reachable |
| `game-panel.console.port` | No | RCON/Telnet port override |
| `game-panel.console.password-env` | No | Name of the game container environment variable holding its console password |
| `game-panel.files` | No | Comma-separated container paths exposed in the file manager |

Minecraft servers automatically expose `/data`. Valheim servers using
`community-valheim-tools/valheim-server` or `lloesche/valheim-server`
automatically expose `/config`. Other images can opt in explicitly:

```yaml
labels:
  game-panel.files: "/config,/backups"
```

The panel will never expose `/` as a file root.

## Authentication

Users sign in with a username and password. Passwords are hashed with scrypt,
and browser sessions use revocable HttpOnly cookies. Administrators manage
accounts, roles, password resets, and the audit log.

| Role | Access |
|---|---|
| Administrator | Server control, game console, file management, container shell, user management, and audit log |
| Operator | Server control, game console, and file management |
| Viewer | Read-only server status, logs, and file downloads |

The optional `PANEL_API_TOKEN` provides administrator access for API automation
or emergency use. Generate one with `openssl rand -hex 32` and send it as
`Authorization: Bearer <token>`. Tokens shorter than 32 characters are rejected.

Use an HTTPS reverse proxy for external access.

Set `TRUSTED_PROXIES` when audit logs and rate limits should use client IPs
forwarded by the directly connected proxy. It also limits trusted
`X-Forwarded-Proto` values to that proxy.

For Docker, inspect the proxy address on its shared network and add `/32`:

```bash
docker inspect -f '{{(index .NetworkSettings.Networks "proxy").IPAddress}}' traefik
```

Replace `traefik` with your proxy container name, append `/32` to the address,
and add it to `TRUSTED_PROXIES`. Multiple entries may be comma-separated.

> [!WARNING]
> Access to the Docker socket is effectively root-level access to the Docker
> host. A read-only bind mount does not make Docker API operations read-only.
> Do not expose this panel directly to the public internet.

## Console support

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

Adapter overrides are `minecraft-rcon`, `source-rcon`, `rust-webrcon`,
`telnet-console`, and `stdin-console`.

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

Minecraft servers use `minecraft-rcon`. The `itzg/minecraft-server` image
includes `rcon-cli` and enables RCON by default. Other images must provide a
compatible `rcon-cli` executable or set `game-panel.console=disabled`.

Closing the browser does not detach or stop the game process. The container
shell is separate from the game console and restricted to administrators.

Vanilla Valheim has live logs but no remote command adapter. Satisfactory also
currently has logs only because it uses a separate HTTPS management API.

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
- Role checks apply to server control, consoles, files, users, and audit logs.
- Game-console commands do not pass through a shell.
- File access is restricted to configured roots and rejects traversal and
  symbolic-link escapes.
- Container-shell access is restricted to administrators.
- Password changes and administrator resets revoke existing sessions.
- State-changing browser requests are restricted to the same origin.
- The public `/api/health` endpoint reports only panel and Docker connectivity.

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

## Development

Requires Node.js 24 and pnpm 10.

```bash
corepack enable
pnpm install
pnpm dev

# Backend or frontend only
pnpm backend:dev
pnpm frontend:dev

# Type-check, lint, test, and build
pnpm check

# Build the production image
docker build -t docker-game-manager:test .
```

- Backend: TypeScript, Express 5, Dockerode, ws, SQLite
- Frontend: React 19, Vite, xterm.js

See [TESTING.md](./TESTING.md) for the release acceptance checklist.

## License

See [LICENSE](./LICENSE).
