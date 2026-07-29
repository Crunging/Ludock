# Ludock

Ludock combines the Latin *ludus* (“game” or “play”) with “dock”: it is a
self-hosted web panel for existing Docker game servers.

It discovers only containers you explicitly opt in, while Docker remains the
source of truth.

> [!IMPORTANT]
> This project is in beta. Back up game data and the panel database before
> upgrading.

## What it does

- Starts, stops, restarts, and monitors opted-in containers
- Streams logs and supports several game-console protocols
- Manages files inside configured container paths
- Provides administrator, operator, and read-only viewer roles
- Records account and server actions in an audit log

It does not create game servers or manage containers without the
`ludock.enable=true` label.

## Quick start

Docker Compose is the recommended installation method.

1. Download [`compose.yaml`](./compose.yaml).
2. Add the management label to each game-server container:

   ```yaml
   services:
     minecraft:
       image: itzg/minecraft-server
       labels:
         ludock.enable: "true"
         ludock.name: "Survival Server"
         ludock.game: "minecraft"
       environment:
         EULA: "TRUE"
       volumes:
         - minecraft-data:/data
       ports:
         - "25565:25565"
       tty: true
       stdin_open: true

   volumes:
     minecraft-data:
   ```

3. Start Ludock:

   ```bash
   docker compose up -d
   ```

4. Open `http://localhost:3000` and create the first administrator within five
   minutes. Restart the panel if the setup window expires.

The image supports `linux/amd64` and `linux/arm64`. Stable versions are
published as `latest`, `MAJOR`, `MAJOR.MINOR`, and `MAJOR.MINOR.PATCH`;
development builds use `nightly`.

## Container labels

| Label | Purpose |
|---|---|
| `ludock.enable` | Required. Set to `"true"` to manage the container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Game identifier, such as `minecraft` or `valheim` |
| `ludock.console` | Console adapter override or `disabled` |
| `ludock.console.host` | RCON or Telnet host override |
| `ludock.console.port` | RCON or Telnet port override |
| `ludock.console.password-env` | Name of the container environment variable containing the console password |
| `ludock.files` | Comma-separated container paths exposed by the file manager |

Minecraft automatically exposes `/data`. Supported Valheim images
automatically expose `/config`. Other servers can declare roots:

```yaml
labels:
  ludock.files: "/config,/backups"
```

The panel rejects `/`, traversal, and symbolic-link escapes.

## Console support

| Game | Transport | Default port |
|---|---|---:|
| Minecraft | Bundled `rcon-cli` | Image-managed |
| Factorio, Palworld, ARK, CS2, Project Zomboid, Conan Exiles, V Rising | Source RCON | Game-specific |
| Rust | WebRCON | 28016 |
| 7 Days to Die | Telnet | 8081 |
| Terraria | Container process stdin | — |

Supported overrides are `minecraft-rcon`, `source-rcon`, `rust-webrcon`,
`telnet-console`, and `stdin-console`. Games without an adapter still have live
logs.

For network consoles, enable the protocol in the game server and store its
password in the game container:

```yaml
labels:
  ludock.console.password-env: "RCON_PASSWORD"
environment:
  RCON_PASSWORD: "${RCON_PASSWORD}"
```

The password value stays on the backend and is not sent to the browser or audit
log. Put the panel and game server on a shared Docker network, and never expose
RCON or Telnet directly to the internet.

## Files and permissions

Administrators and operators can browse, upload, create, rename, download, and
delete files inside configured roots. Viewers can browse and download only.
Stop a game server before replacing active worlds or configuration files.

Volume-backed roots remain available while a server is stopped. The panel uses
a short-lived helper container with no network and dropped Linux capabilities;
data kept only in a container's writable layer requires that container to be
running.

| Role | Access |
|---|---|
| Administrator | Server control, game console, files, container shell, users, and audit log |
| Operator | Server control, game console, and files |
| Viewer | Server status, logs, and file downloads |

## Security

> [!WARNING]
> Docker socket access is effectively root access to the Docker host. A
> read-only socket mount does not make Docker API operations read-only. Run the
> panel only on a trusted host and place it behind an HTTPS reverse proxy.

Browser sessions use revocable HttpOnly cookies, and passwords are hashed with
scrypt. Only labeled containers can be listed or controlled.

Optional settings are documented in [`.env.example`](./.env.example). In
particular:

- `LUDOCK_API_TOKEN` enables administrator API access and must contain at least
  32 characters.
- `TRUSTED_PROXIES` identifies the directly connected reverse proxy whose
  forwarded client address and protocol may be trusted.
- `MAX_UPLOAD_BYTES`, `AUDIT_LOG_MAX_ROWS`, and `FILE_HELPER_IMAGE` adjust
  operational limits.

See [SECURITY.md](./SECURITY.md) for vulnerability reporting and the supported
release policy.

## Backup and recovery

Ludock accounts, sessions, and audit history are stored in
`./data/ludock.db`.
Back it up with the panel stopped:

```bash
docker compose stop ludock
cp ./data/ludock.db ./ludock.db.backup
docker compose start ludock
```

To reset an existing administrator password:

```bash
docker compose stop ludock
docker compose run --rm \
  -e LUDOCK_RECOVERY_PASSWORD='choose-a-new-long-password' \
  ludock node packages/backend/dist/recovery.js admin
docker compose start ludock
```

## Development

Development requires Node.js 24 and pnpm 10.

```bash
corepack enable
pnpm install
pnpm dev
```

Run `pnpm check` before submitting a change. Container or deployment changes
also require:

```bash
docker build -t ludock:test .
```

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the contribution workflow and
[TESTING.md](./TESTING.md) for the release acceptance checklist.

Licensed under the [MIT License](./LICENSE).
