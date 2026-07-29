# Testing Ludock

## Automated checks

From a clean checkout with Node.js 24 and Corepack enabled:

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm check
docker build -t ludock:test .
```

`pnpm check` runs TypeScript checking, frontend linting, backend unit and HTTP
integration tests, and both production builds.

## Manual acceptance checklist

Run the panel with a persistent `/data` volume and add
`ludock.enable=true` to a disposable container.

### Authentication

- A new database displays the initial administrator setup page.
- Username and a 15+ character password create the first administrator.
- Initial setup locks five minutes after startup. Restarting the panel reopens
  the setup window when no account exists.
- Subsequent visits display username/password sign-in, not the setup form.
- Invalid credentials use a generic error and repeated failures are throttled.
- Refreshing and restarting the panel preserve an authenticated session.
- Sign out returns to the sign-in page on desktop and mobile.
- Changing a password revokes every other session.
- A revoked or disabled session immediately loses API and WebSocket access.
- Behind an HTTPS-terminating reverse proxy, setup and login cookies include
  `Secure` and console WebSockets connect without panel-specific proxy flags.
- `TRUSTED_PROXIES` changes the recorded client IP only for requests arriving
  through the configured proxy address or CIDR.

### Users and permissions

- An administrator can create administrator, operator, and viewer accounts.
- Usernames are case-insensitively unique.
- The last enabled administrator cannot be disabled or demoted.
- Operators can start, stop, restart, and use the game console, but cannot open
  a container shell, manage users, or inspect the audit log. They can manage
  files inside configured roots.
- Viewers can see server state and logs but cannot mutate containers, issue
  game commands, open a shell, or change files; they can download files.
- The audit log records sign-in, account, lifecycle, and shell activity.

### Discovery and lifecycle

- Only opted-in containers appear.
- The configured display name and game type are shown.
- Stop changes a running server to `Exited` and presents a Start action.
- Start returns it to `Running`.
- Restart completes and the dashboard stays responsive.
- Direct lifecycle API requests for an unmanaged container return `403`.

### Game console, logs, and shell

- Opening **Game Console** displays recent and live container output.
- On an `itzg/minecraft-server` fixture, `say acceptance-test` is sent through
  RCON and appears in the server log.
- `difficulty hard`, `whitelist on`, and `whitelist add PlayerName` are accepted
  as native Minecraft commands without an `rcon-cli` prefix.
- A Source RCON fixture (Factorio, Palworld, ARK/ASA, CS2, Project Zomboid,
  Conan Exiles, or V Rising) authenticates from its configured password
  environment variable and returns command output.
- A Rust fixture with WebRCON enabled accepts `status` and `server.save`.
- A 7 Days to Die fixture with Telnet enabled accepts `listplayers` and
  `saveworld`.
- A Terraria fixture with an open process stdin accepts `playing` and `save`.
- Incorrect console credentials produce a generic authentication failure and
  never appear in browser messages or audit-log details.
- Operators can use the Game Console but a direct `/ws/shell/:id` upgrade is
  rejected.
- Administrators can switch to **Container Shell**; `printf test-ok` displays
  `test-ok` in the terminal.
- Unsupported games explain that no adapter is configured and retain live logs.
- Leaving the page closes the WebSocket without affecting the container.

### File manager

- A Minecraft server exposes `/data`; a supported Valheim image exposes
  `/config`. An explicit `ludock.files` label overrides inference.
- Files and folders can be browsed without exposing paths outside a configured
  root.
- Dragging one or more files into the upload area stores them in the current
  folder without buffering the full file in panel memory.
- Create, rename, download, and delete operations refresh the listing and
  appear in the audit log.
- Folder downloads are tar archives and file downloads preserve the filename.
- `../`, absolute paths, root deletion, and access through symbolic links are
  rejected.
- Operators can upload and mutate files. Viewers can browse and download but
  receive `403` for mutation endpoints.
- Stop the game fixture before replacing an active world and confirm its world
  files remain paired and load successfully after restart.
- A stopped server with a volume-backed file root remains browsable and
  writable without starting the game container. The temporary helper has no
  network, drops Linux capabilities, and is removed after the operation.

### Responsive layout

- At mobile width, the header, sign-out button, cards, and console input fit
  without horizontal scrolling. File rows stack their metadata and actions.
- At desktop width, the sidebar stays fixed and cards fill the content grid.

## Product boundaries

- The built-in console transports are Minecraft `rcon-cli`, Source RCON, Rust
  WebRCON, 7 Days to Die-style Telnet, and process stdin. Games with a distinct
  management API require a protocol-specific adapter.
- The panel manages existing, explicitly labeled containers; it does not
  provision servers or interpret game-specific mod formats. Its file manager
  can place world, configuration, backup, and mod files in configured paths.
- The Docker socket grants host-level control. Test on a non-production Docker
  host and deploy behind TLS.
