# Ludock

Ludock is a self-hosted control panel for existing Docker game servers. It
starts and stops servers, provides game consoles and file access, and handles
backups, schedules, availability alerts, and updates through existing Compose
projects.

Recognized game images appear automatically unless explicitly opted out.
Unrecognized images require `ludock.enable: "true"`. Docker Compose, Portainer,
Dockge, or the Docker CLI remains the configuration owner: Ludock does not
provision servers or edit their definitions.

## Run with Docker

Docker and the `docker compose` command are required. Save
[`compose.yaml`](./compose.yaml) in a folder on your Docker host and run the
commands below from that folder. To customize it,
copy [`.env.example`](./.env.example) to `.env` beside the Compose file and
uncomment the settings you want to change. For example, `LUDOCK_PORT=8080`
changes the browser port, and `MAX_UPLOAD_SIZE=500 MB` limits each file upload.
The defaults work without an `.env` file.

Start the panel:

```bash
docker compose up -d
```

Open `http://localhost:3000` (or your chosen port). From another device, replace
`localhost` with your Docker host's address. On the Docker host, retrieve the
one-time setup code:

```bash
docker compose logs ludock
```

Enter the most recent code in the setup page and create the first administrator
within five minutes. The generated code is written directly to the local container console,
not Ludock's browser-accessible application logs. If setup expires, run
`docker compose restart ludock`, retrieve the newly generated code, then choose
**Check again** in the panel. You can instead configure a private
`LUDOCK_SETUP_CODE` in `.env`; configured codes are not printed.

The example creates separate volumes for Ludock application data and backups.
Existing game containers and game-data volumes stay in place.

Unsupported application databases are rejected without modification. If reusing
an older development database, keep its volume for rollback and follow the
[storage upgrade guidance](./docs/OPERATIONS.md#deployment-and-application-storage)
before starting the new image.

After signing in:

1. Open **Servers**. Recognized game containers appear automatically; use
   **Game not shown?** if one is missing.
2. To enable backups, open **Settings → Backup storage**, choose **Use /backups**,
   review the limits, and save. No extra mount setup is needed with this example.
3. To share a server, open **Users**, create an account, then choose its servers
   and permitted actions in the access editor.

Ludock images target **Linux AMD64 and ARM64**; individual game images may
support fewer platforms. For local builds and disposable test environments, see
[local development](#development).

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

The example Compose deployment provides backup storage at `/backups`. Backups
are enabled when an administrator saves its capacity and retention settings.
This volume stores archives on the Docker host; to use another disk, replace
the backup volume mount with a host-directory mount as shown in
[`compose.yaml`](./compose.yaml).
Backups stop the server throughout copying and restore its previous running
state afterward. Live backups are not included.

Updates discover Compose projects automatically; source files must be accessible inside
`LUDOCK_COMPOSE_ROOTS`. Ludock pulls the selected service's configured image and
recreates that service without rebuilding it or recreating dependencies.
**Recreate anyway** also replaces a service whose image is unchanged. A stopped
server stays stopped. Inaccessible projects and standalone containers must be
updated through their original manager.

See [Operations](./docs/OPERATIONS.md) for the required mounts, confirmation
flows, limitations, and recovery behavior.

## Development

Use the latest stable Bun 1 release (`package.json` declares the minimum):

```bash
bun install --frozen-lockfile
bun run dev
bun run check  # Types, lint, tests, and build
```

The dev runner prints its URLs and uses separate state for each checkout.
Docker is disconnected by default. Stop with Ctrl+C.

See the [development and testing reference](./docs/TESTING.md) for overrides,
lock recovery, focused tests, and disposable Docker fixtures.

## Documentation

- [Game servers](./docs/GAME-SERVERS.md): recognized repositories, capability
  matrix, console prerequisites, labels, and safe file roots.
- [Operations](./docs/OPERATIONS.md): deployment settings, permissions, backups,
  schedules, Compose updates, monitoring, notifications, and recovery.
- [Architecture](./docs/ARCHITECTURE.md): shared contracts, feature modules, and
  development instance boundaries.
- [Security](./SECURITY.md): deployment boundary and vulnerability reporting.

## Security

Docker socket access grants host-level power. Mounting the socket read-only
does not make Docker API requests read-only. Deploy one Ludock instance per
managed Docker host, keep it on a trusted network, and use an HTTPS reverse
proxy for remote access.

Browser sessions use revocable HttpOnly cookies. New passwords use Argon2id;
existing scrypt hashes remain valid and are upgraded after a successful sign-in.
Authorization is enforced in the API, WebSockets, and background jobs.
Discovery eligibility is separate from a user's permission to access a
server. Console credentials and notification webhooks are never returned to
the browser.

Licensed under the [MIT License](./LICENSE).
