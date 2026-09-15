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

After signing in:

1. Open **Servers**. Recognized game containers appear automatically; use
   **Game not shown?** if one is missing.
2. To enable backups, open **Settings → Backup storage**, choose **Use /backups**,
   review the limits, and save. No extra mount setup is needed with this example.
3. To share a server, open **Users**, create an account, then choose its servers
   and permitted actions in the access editor.

Ludock images target **Linux AMD64 and ARM64**; individual game images may
support fewer platforms. Backups require downtime. Compose updates require
read-only access to the owner's source files; standalone containers are updated
through their original manager.

Docker socket access grants host-level power, even with a read-only socket
mount. Run one Ludock instance per Docker host, keep it on a trusted network,
and use an HTTPS reverse proxy for remote access.

## Documentation

- [Operations](./docs/OPERATIONS.md): game images and labels, deployment settings,
  permissions, files, backups, Compose updates, schedules, alerts, and recovery.
- [Development instructions](./AGENTS.md): local setup, code boundaries, tests,
  and dependency/image updates.
- [Security](./SECURITY.md): deployment boundary and vulnerability reporting.

Licensed under the [MIT License](./LICENSE).
