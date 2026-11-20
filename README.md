# Ludock

> **Beta:** expect bugs and changes. Keep your own backups of important game
> data, and [report issues](https://github.com/Crunging/Ludock/issues).

Ludock is a simple web panel for the Docker game servers you already run with
Compose, Portainer, or the Docker CLI. It finds them by itself and knows games:
who is online, saving before a stop, warning players before a restart, and
checking that the game itself still answers. Your servers stay yours: Ludock
never creates or edits them, and removing it leaves them as they were.

![Ludock's server list with Minecraft, Factorio, and Valheim servers](./docs/screenshots/servers.png)

## Quick start

You need Docker with Compose, on Linux (AMD64 or ARM64).

1. On the host that runs your game servers, save
   [`compose.yaml`](./compose.yaml) and start Ludock:

   ```bash
   docker compose up -d
   ```

2. Get the setup code from its log. Restarting Ludock prints a new one.

   ```bash
   docker compose logs ludock
   ```

3. Open `http://<your-host>:3000`, enter the code, and create your account.

4. Follow the short **Finish setting up** list on the first page. It turns on
   alerts, nightly backups, and automatic restarts in a click each.

![The Finish setting up list above the server list](./docs/screenshots/setup.png)

Servers on [supported game images](./docs/REFERENCE.md#supported-games) appear
by themselves. For any other container, choose **Missing a server?** under the
server list.

## What it does

- **Start, stop, and restart**, saving the world first and warning players
  with a countdown.
- **Consoles and logs** in the browser, and a list of who is online, with kick,
  ban, and the Minecraft allowlist.
- **Files:** upload, edit, unpack, and download a server's data.
- **Backups** as plain tar archives, on a schedule, with weeks to months kept
  and an optional second copy on another disk. Restore a whole backup or one
  file.
- **Schedules** for restarts, backups, and console commands, which can wait
  until no one is online.
- **Alerts** to Discord, ntfy, Gotify, Telegram, email, or a webhook when a
  server goes down, its game stops answering, or a disk fills up, and an
  automatic restart when a game hangs.
- **Image updates**, with a backup first and the old container put back if the
  new one fails.
- **Sharing** a server with a friend by invite link, letting them see it, run
  it, or manage it fully.

<details>
<summary>More screenshots</summary>

**Backups:** kept and recent backups of a server, ready to browse or restore.

![A server's backups, one kept for good](./docs/screenshots/backups.png)

**Files:** a server's data folders.

![The file browser showing a Minecraft server's folders](./docs/screenshots/files.png)

**Sharing:** an invite link for a friend.

![Sharing a server with an invite link](./docs/screenshots/sharing.png)

</details>

## Security

Access to the Docker socket is control of the whole host, even mounted
read-only. Run Ludock on a trusted network, and use an HTTPS reverse proxy to
reach it from elsewhere. See [SECURITY.md](./SECURITY.md).

## Documentation

- [Guide](./docs/GUIDE.md): everyday tasks, and how backups, schedules,
  consoles, and monitoring behave.
- [Reference](./docs/REFERENCE.md): supported games, labels, limits, remote
  access, environment variables, and restoring Ludock itself.
- [Development](./docs/DEVELOPMENT.md): building, testing, and releasing.

Licensed under the [MIT License](./LICENSE).
