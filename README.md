# Ludock

> **Beta:** Ludock is currently in beta. Expect bugs and changes as it develops.
> Keep independent backups of important game data, and
> [report issues](https://github.com/Crunging/Ludock/issues).

Ludock is a self-hosted control panel for existing Docker game servers. It
starts and stops servers, provides game consoles and file access, and handles
backups, schedules, availability alerts, and updates through existing Compose
projects.

Recognized game images appear automatically unless explicitly opted out.
Unrecognized images require `ludock.enable: "true"`. Docker Compose, Portainer,
Dockge, or the Docker CLI remains the configuration owner: Ludock does not
provision servers or edit their definitions.

## Screenshots

The current interface, shown with demo servers and data.

**Servers** — status, ports, recent backups, and everyday controls.

![Ludock server list with Minecraft, Factorio, and Valheim demo servers](./docs/screenshots/servers.png)

<details>
<summary>Backups and file access</summary>

**Backups** — readiness checks and retained archives for a server.

![Ludock server backup page showing readiness checks and three retained backups](./docs/screenshots/backups.png)

**Files** — browse and manage files inside approved server data roots.

![Ludock file browser showing the demo Minecraft server's folders and configuration files](./docs/screenshots/files.png)

</details>

## Run with Docker

Use Docker with [Compose 2.24.0 or later](https://docs.docker.com/reference/compose-file/services/#required).
Check with `docker compose version`. Save [`compose.yaml`](./compose.yaml) in
a folder on the host that runs your game containers. Run the commands below
from that folder. **The default installation needs no `.env` file.
Recognized game images need no extra labels.**

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

After signing in:

1. Open **Servers**. Recognized game containers appear automatically; use
   **Game not shown?** if one is missing.
2. To enable backups, open **Settings → Backup storage**. `/backups` is filled
   in for you; review the suggested limits and save. No extra mount setup is
   needed with this example.
3. To share a server, open **Users**, create an account, then choose its servers
   and permitted actions in the access editor.

Ludock images target **Linux AMD64 and ARM64**; individual game images may
support fewer platforms. Backups require downtime. Compose updates require
read-only access to the owner's source files; standalone containers are updated
through their original manager.

Docker socket access grants host-level power, even with a read-only socket
mount. Run one Ludock instance per Docker host, keep it on a trusted network,
and use an HTTPS reverse proxy for remote access.

## Optional configuration

Most everyday configuration happens in the browser:

| What you want to do | Where to do it |
| --- | --- |
| Start, stop, or restart a server | **Servers** or the server’s details |
| Set backup limits and retention | **Settings → Backup storage** |
| Run an action regularly | **Server → Schedules**; choose an action, time, and days |
| Monitor a server expected to stay online | **Server → Availability** |
| Receive Discord alerts | **Settings → Discord notifications** |
| Share specific servers and controls | **Users → Server access** |

Browser settings take effect when saved. For deployment settings, copy
[`.env.example`](./.env.example) to `.env` beside `compose.yaml`, uncomment only
the values you need, and recreate Ludock:

```bash
docker compose up -d --force-recreate ludock
```

For example, `LUDOCK_PORT=8080` changes the browser port and
`MAX_UPLOAD_SIZE=500 MB` limits each upload. A different host socket uses
`LUDOCK_DOCKER_SOCKET=/run/user/1000/docker.sock` (replace the path with yours).
Compose update access needs a read-only source mount; see
[Operations](./docs/OPERATIONS.md#compose-updates) for that setup.

## Documentation

- [Operations](./docs/OPERATIONS.md): game images and labels, deployment settings,
  permissions, files, backups, Compose updates, schedules, alerts, and recovery.
- [Development instructions](./AGENTS.md): local setup, code boundaries, tests,
  and dependency/image updates.
- [Security](./SECURITY.md): deployment boundary and vulnerability reporting.

Licensed under the [MIT License](./LICENSE).
