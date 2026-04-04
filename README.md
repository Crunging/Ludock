# Docker Game Manager

A lightweight, Docker-native game server control panel. Unlike traditional panels, this app is **completely stateless** — it uses the Docker Engine API as the single source of truth, making it fully compatible with Portainer, Watchtower, and any other Docker tooling.

## Features

- **Docker-native**: Mounts `docker.sock` — no database, no shadow state
- **Label-driven discovery**: Containers opt-in via `game-panel.enable=true`
- **Live console**: Stream container logs and send commands via WebSocket
- **Real-time updates**: Docker event stream pushes state changes instantly
- **Portainer/Watchtower safe**: If Watchtower recreates a container, the panel picks it up automatically

## Quick Start

### Add Labels to Your Game Servers

```yaml
# docker-compose.yml
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
    tty: true
    stdin_open: true
```

### Run the Panel

```bash
docker compose up -d
```

Or with Docker directly:

```bash
docker run -d \
  -p 3000:3000 \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  docker-game-manager
```

The panel will be available at `http://localhost:3000`.

## Labels

| Label | Required | Description |
|---|---|---|
| `game-panel.enable` | ✅ | Set to `"true"` to make a container visible |
| `game-panel.name` | ❌ | Display name (defaults to container name) |
| `game-panel.game` | ❌ | Game type identifier (e.g., `minecraft`, `valheim`) |

## Authentication

Set the `PANEL_SECRET` environment variable to enable token-based auth:

```bash
docker run -d \
  -p 3000:3000 \
  -e PANEL_SECRET=my-secret-token \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  docker-game-manager
```

## Development

```bash
# Install dependencies
pnpm install

# Run both frontend & backend in dev mode
pnpm dev

# Backend only
pnpm backend:dev

# Frontend only (with API proxy to :3001)
pnpm frontend:dev
```

### Tech Stack

- **Backend**: Node.js, TypeScript, Express 5, Dockerode, ws
- **Frontend**: React 19, Vite, xterm.js
- **Monorepo**: pnpm workspaces

## Architecture

```
┌─────────────┐     ┌──────────────┐     ┌──────────────┐
│   Browser    │◄───►│   Backend    │◄───►│ Docker Engine│
│  React SPA   │ WS  │  Express+WS  │sock │  (docker.sock)│
│  xterm.js    │     │  Dockerode   │     │              │
└─────────────┘     └──────────────┘     └──────────────┘
```

### Console (dual-stream, safe)

- **Output**: `container.logs({ follow: true })` → WebSocket → xterm.js
- **Input**: WebSocket → `container.exec()` → response piped back

This approach never calls `container.attach()`, so there's zero risk of killing PID 1.

## License

See [LICENSE](./LICENSE).
