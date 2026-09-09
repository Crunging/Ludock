# Ludock

Ludock is a self-hosted control panel for existing Docker game servers. It
starts and stops servers, provides game consoles and file access, and handles
backups, schedules, availability alerts, and updates through registered Compose
projects.

Recognized game images appear automatically unless explicitly opted out.
Unrecognized images require `ludock.enable: "true"`. Docker Compose, Portainer,
Dockge, or the Docker CLI remains the configuration owner: Ludock does not
provision servers or edit their definitions.

> [!IMPORTANT]
> v2 requires fresh Ludock application storage. It does not import v1 users,
> settings, or sessions. An incompatible database is rejected without resetting
> it. Keep existing game containers and game-data volumes intact.

## Run with Docker

Use [`compose.yaml`](./compose.yaml) with a published v2 image, then start the
panel:

```bash
docker compose up -d
```

Open `http://localhost:3000` and create the first administrator within five
minutes. Restart Ludock if the setup window expires before an account exists.
The example uses a new `ludock-data-v2` named volume; do not reuse a v1 database.

To test this checkout before its image is published:

```bash
docker build -t ludock:test .
docker run --rm -p 3000:3000 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v ludock-data-v2:/data \
  ludock:test
```

Release images target **Linux AMD64 and ARM64**. Each platform requires its own
runtime smoke checks; see [Testing](./TESTING.md). This does not imply that all
recognized game images support both architectures.

## Discover and share servers

A recognized image such as `itzg/minecraft-server` needs no Ludock label.
Private mirrors with a recognized repository suffix also work.

```yaml
labels:
  ludock.name: "Friends' survival world"  # Optional display name
```

For another image, add `ludock.enable: "true"`. To exclude any image, add
`ludock.enable: "false"`. Invalid values exclude the container and appear in
administrator diagnostics. Compose one-off containers are excluded unless
explicitly enabled.

Administrators see all eligible servers. New operators and viewers see none
until an administrator opens **Users → Server access** and assigns specific
servers and actions. **Start and stop** shares only status, start, and stop;
it does not grant logs, commands, files, backups, or schedules.

## Backups and updates

Backups are disabled until an administrator mounts a separate destination,
sets `LUDOCK_BACKUP_ROOTS`, and saves its capacity and retention settings.
Backups stop the server throughout copying and restore its previous running
state afterward. Live backups are not included.

Updates require a registered, accessible Compose project inside
`LUDOCK_COMPOSE_ROOTS`. Ludock pulls the selected service's configured image and
recreates that service without rebuilding it or recreating dependencies.
**Recreate anyway** also replaces a service whose image is unchanged. A stopped
server stays stopped. Inaccessible projects and standalone containers must be
updated through their original manager.

See [Operations](./docs/OPERATIONS.md) for the required mounts, confirmation
flows, limitations, and recovery behavior.

## Documentation

- [Game servers](./docs/GAME-SERVERS.md): recognized repositories, capability
  matrix, console prerequisites, labels, and safe file roots.
- [Operations](./docs/OPERATIONS.md): deployment settings, permissions, backups,
  schedules, Compose updates, monitoring, notifications, and recovery.
- [Testing](./TESTING.md): automated checks and disposable-server acceptance.
- [Contributing](./CONTRIBUTING.md): Node.js 24 / pnpm 10 development workflow.
- [Security](./SECURITY.md): deployment boundary and vulnerability reporting.

## Security

Docker socket access grants host-level power. Mounting the socket read-only
does not make Docker API requests read-only. Deploy one Ludock instance per
managed Docker host, keep it on a trusted network, and use an HTTPS reverse
proxy for remote access.

Browser sessions use revocable HttpOnly cookies, passwords are hashed with
scrypt, and authorization is enforced in the API, WebSockets, and background
jobs. Discovery eligibility is separate from a user's permission to access a
server. Console credentials and notification webhooks are never returned to
the browser.

Licensed under the [MIT License](./LICENSE).
