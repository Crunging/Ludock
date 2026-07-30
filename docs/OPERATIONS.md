# Operations

This guide covers Ludock configuration, access levels, diagnostics, and
recovery. Game-server labels and integrations are documented separately in
[Game servers and overrides](./GAME-SERVERS.md).

## Environment settings

Copy [`.env.example`](../.env.example) to `.env` beside `compose.yaml`. Docker
Compose loads this file into the Ludock container.

| Variable | Purpose |
|---|---|
| `LOG_LEVEL` | Log verbosity: `error`, `warn`, `info` (default), or `debug` |
| `LUDOCK_API_TOKEN` | Optional administrator API token; must be at least 32 characters |
| `MAX_UPLOAD_BYTES` | Maximum file upload size |
| `AUDIT_LOG_MAX_ROWS` | Maximum retained audit entries |
| `FILE_HELPER_IMAGE` | Image used to access volume-backed files while a game is stopped |

After changing `.env`, recreate the container:

```bash
docker compose up -d --force-recreate ludock
```

## Reverse proxies

Traefik works without Ludock-specific middleware. It automatically supplies
the external host and protocol metadata Ludock uses for HTTPS cookies,
browser-origin checks, and WebSockets. Other reverse proxies work when they
preserve the public host, forward the external protocol, and support WebSocket
upgrades.

## Logging and diagnostics

The default `LOG_LEVEL=info` records startup, shutdown, and meaningful
connection lifecycle events. Use debug logging temporarily when diagnosing API,
WebSocket, Docker stream, or game-console connections:

```env
LOG_LEVEL=debug
```

Follow the logs while reproducing the problem:

```bash
docker logs --follow --tail 200 ludock
```

Debug output includes request paths, status codes, console adapters, shortened
container IDs, and connection state. Ludock does not log console command text
or authentication credentials, and sensitive structured fields are redacted.

## Access levels

| Role | Access |
|---|---|
| Administrator | Server control, game console, files, container shell, users, and audit log |
| Operator | Server control, game console, and files |
| Viewer | Server status, logs, and file downloads |

Administrators and operators can browse, upload, create, rename, download, and
delete files inside configured roots. Viewers can browse and download only.
Stop a game server before replacing active worlds or configuration files.

Volume-backed roots remain available while a server is stopped. Ludock uses a
short-lived helper container with no network and dropped Linux capabilities.
See [File roots](./GAME-SERVERS.md#file-roots) for discovery and overrides.

## Backup and recovery

Ludock accounts, sessions, and audit history are stored in
`./data/ludock.db`. Back it up with the panel stopped:

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

Return to the [project overview](../README.md).
