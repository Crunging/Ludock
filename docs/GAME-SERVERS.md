# Game servers and overrides

Ludock discovers existing Docker containers. It does not create or configure
game servers, and it manages only containers with:

```yaml
labels:
  ludock.enable: "true"
```

For a recognized image, that is the only label required. Ludock infers the
game integration from the image repository and discovers file roots from the
container's writable mounts.

## Supported images

Tags, registry prefixes, and image digests do not affect recognition. An exact
known repository is required; similarly named repositories are not guessed.

| Game | Recognized images | Published platforms |
|---|---|---|
| Minecraft | `itzg/minecraft-server` | amd64, arm64, riscv64 |
| Factorio | `factoriotools/factorio` | amd64, arm64 |
| Palworld | `thijsvanloef/palworld-server-docker`, `jammsen/palworld-dedicated-server` | amd64; Thijs also publishes arm64 |
| ARK: Survival Evolved | `hermsi/ark-server`, `indifferentbroccoli/ark-server-docker` | amd64 |
| ARK: Survival Ascended | `sknnr/ark-ascended-server`, `mschnitzer/asa-linux-server` | amd64 |
| Counter-Strike 2 | `joedwards32/cs2` | amd64 |
| Project Zomboid | `renegademaster/zomboid-dedicated-server` and its GHCR/Quay variants | amd64 |
| Conan Exiles | `indifferentbroccoli/conan-exiles-enhanced-server-docker` | amd64 |
| V Rising | `trueosiris/vrising` | amd64 |
| Rust | `didstopia/rust-server` | amd64 |
| 7 Days to Die | `vinanrra/7dtd-server` | amd64 |
| Valheim | `ghcr.io/community-valheim-tools/valheim-server`, `lloesche/valheim-server` | amd64 |
| Terraria | `hexlo/terraria-server-docker`, `beardedio/terraria`, `ryshe/terraria` | image-dependent; Hexlo and Ryshe publish arm64 |

Games without a supported command transport still have live container logs.

## Labels and overrides

Explicit labels take precedence over image inference.

| Label | Purpose |
|---|---|
| `ludock.enable` | Required. Set to `"true"` to manage the container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Game override, such as `minecraft` or `terraria` |
| `ludock.console` | Console adapter override or `disabled` |
| `ludock.console.host` | RCON or Telnet host override |
| `ludock.console.port` | RCON or Telnet port override |
| `ludock.console.password-env` | Name of the game container environment variable containing its console password |
| `ludock.files` | Comma-separated file-root override; set to an empty string to disable files |

An unknown or custom image needs `ludock.game` to select a game integration:

```yaml
services:
  terraria:
    image: example/custom-terraria:latest
    labels:
      ludock.enable: "true"
      ludock.game: "terraria"
```

Use `ludock.console` only when the game preset selects the wrong transport or
when a custom image needs an explicit adapter. Supported values are
`minecraft-rcon`, `source-rcon`, `rust-webrcon`, `telnet-console`, and
`stdin-console`. Set it to `disabled` or `none` for logs without command input.

## Game consoles

These are console connections and ports, not the ports players use to join a
game.

| Game | Console connection | Default console port |
|---|---|---:|
| Minecraft (`itzg/minecraft-server`) | Bundled `rcon-cli` inside the container | 25575 (internal) |
| Factorio | RCON | 27015 |
| Palworld | RCON | 25575 |
| ARK and ARK: Survival Ascended | RCON | 27020 |
| Counter-Strike 2 | RCON | 27015 |
| Project Zomboid | RCON | 27015 |
| Conan Exiles | RCON | 25575 |
| V Rising | RCON | 25575 |
| Rust | WebRCON | 28016 |
| 7 Days to Die | Telnet | 8081 |
| Terraria | Container process input | — |

Minecraft normally uses port `25565` for players and `25575` for RCON. Ludock
runs the bundled `rcon-cli` command inside `itzg/minecraft-server`, so the RCON
port should not be published or configured in Ludock.

For consoles reached over a Docker network, enable the protocol in the game
server and store its password in the game container:

```yaml
services:
  game:
    labels:
      ludock.enable: "true"
      ludock.console.password-env: "RCON_PASSWORD"
    environment:
      RCON_PASSWORD: "${RCON_PASSWORD}"
```

The label contains the environment variable's name, not its secret value. The
password remains on the backend and is not sent to the browser or audit log.
Put Ludock and the game server on a shared Docker network, and never expose
RCON or Telnet directly to the internet.

## File roots

The file manager uses the container's actual writable bind mounts and named
volumes. It does not rely on an image-specific default path and never exposes
host source paths to the browser.

Read-only, system, Docker socket, host-root, and nested duplicate mounts are
excluded. This allows custom host paths and named volumes to work without
additional labels:

```yaml
services:
  terraria:
    image: ryshe/terraria:latest
    stdin_open: true
    tty: true
    volumes:
      - ./terraria-data:/root/.local/share/Terraria/Worlds
    labels:
      ludock.enable: "true"
```

Terraria uses container process input for console commands, so its standard
input must remain open. The
[`ryshe/terraria` image instructions](https://hub.docker.com/r/ryshe/terraria)
also use this world path and an interactive TTY.

Use `ludock.files` to select a subset of mounts or expose custom container
paths:

```yaml
labels:
  ludock.files: "/config,/backups"
```

An empty value disables the file manager:

```yaml
labels:
  ludock.files: ""
```

Volume-backed roots remain available while a server is stopped. Data stored
only in the container's writable layer requires the container to be running.
Ludock rejects `/`, traversal, and symbolic-link escapes.

Return to the [project overview](../README.md).
