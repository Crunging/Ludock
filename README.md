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
2. Opt each game-server service into Ludock:

   ```yaml
   services:
     minecraft:
       labels:
         ludock.enable: "true"
         ludock.name: "Survival Server"
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

## Add game servers

Add `ludock.enable=true` to each game-server container you want Ludock to
manage. Known images need no other labels: Ludock selects the game integration
from the image and discovers file roots from its writable mounts.

See [Game servers and overrides](./docs/GAME-SERVERS.md) for:

- recognized images and platform availability;
- game, console, port, password-variable, and file-root overrides;
- console transports and their internal ports;
- custom-image examples and file-root discovery behavior.

## Documentation

- [Game servers and overrides](./docs/GAME-SERVERS.md): recognized images,
  labels, console transports, custom images, and file roots.
- [Operations](./docs/OPERATIONS.md): environment settings, log levels,
  permissions, backups, and administrator recovery.
- [Security policy](./SECURITY.md): deployment boundary, supported releases,
  and vulnerability reporting.
- [Testing](./TESTING.md): release acceptance criteria and product boundaries.
- [Contributing](./CONTRIBUTING.md): development setup and contribution
  workflow.

## Security

> [!WARNING]
> Docker socket access is effectively root access to the Docker host. A
> read-only socket mount does not make Docker API operations read-only. Run the
> panel only on a trusted host and place it behind an HTTPS reverse proxy.

Browser sessions use revocable HttpOnly cookies, and passwords are hashed with
scrypt. Only labeled containers can be listed or controlled.

See the [operations guide](./docs/OPERATIONS.md) for configuration, logging,
backup, and recovery, and [SECURITY.md](./SECURITY.md) for the supported release
policy.

## Development

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup and the
contribution workflow, and [TESTING.md](./TESTING.md) for the release acceptance
checklist.

Licensed under the [MIT License](./LICENSE).
