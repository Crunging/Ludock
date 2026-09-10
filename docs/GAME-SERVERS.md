# Game servers

Ludock discovers existing Docker containers. It does not install game servers,
edit Compose definitions, or interpret game-specific mod formats.

## Discovery

Eligibility is evaluated in this order:

1. An invalid `ludock.enable` value excludes the container and creates an
   administrator diagnostic.
2. `ludock.enable: "false"` excludes it.
3. `ludock.enable: "true"` includes it, including an unknown image or Compose
   one-off.
4. An otherwise-unlabeled Compose one-off is excluded.
5. A recognized image is included automatically.
6. Other containers are excluded.

Boolean labels are trimmed and case-insensitive; only `true` and `false` are
valid. Unknown containers do not appear as discovery candidates. Use **Game
not shown?** for explicit inclusion instructions.

Recognition removes tags, digests, and registry hostnames/ports, then matches a
known repository suffix. Both `itzg/minecraft-server:java21` and
`registry.example.com/mirrors/itzg/minecraft-server:java21` select Minecraft.
Similarly named repositories are not guessed. Recognition selects integration
capabilities; it does not verify image provenance.

Administrators see eligible servers immediately. Other users require explicit
server/action grants. A game label alone does not make an unknown image eligible.

## Recognized repositories and console capabilities

The integration registry is
[`server-presets.ts`](../packages/backend/src/server-presets.ts). It supplies the
**Diagnostics → Game capabilities** table and `/api/v1/integrations`.

| Game | Recognized repository suffixes | Console adapter | Default port |
| --- | --- | --- | --- |
| Minecraft | `itzg/minecraft-server` | Minecraft `rcon-cli` | Internal configuration |
| Factorio | `factoriotools/factorio` | Source RCON | 27015 |
| Palworld | `thijsvanloef/palworld-server-docker`, `jammsen/palworld-dedicated-server` | Source RCON | 25575 |
| ARK: Survival Evolved | `hermsi/ark-server`, `hermsi1337/ark-server`, `indifferentbroccoli/ark-server-docker` | Source RCON | 27020 |
| ARK: Survival Ascended | `sknnr/ark-ascended-server`, `mschnitzer/asa-linux-server` | Source RCON | 27020 |
| Counter-Strike 2 | `joedwards32/cs2` | Source RCON | 27015 |
| Project Zomboid | `renegademaster/zomboid-dedicated-server`, `renegade-master/zomboid-dedicated-server`, `renegade_master/zomboid-dedicated-server` | Source RCON | 27015 |
| Conan Exiles | `indifferentbroccoli/conan-exiles-enhanced-server-docker` | Source RCON | 25575 |
| V Rising | `trueosiris/vrising` | Source RCON | 25575 |
| Rust | `didstopia/rust-server` | Rust WebRCON | 28016 |
| 7 Days to Die | `vinanrra/7dtd-server` | Telnet | 8081 |
| Valheim | `community-valheim-tools/valheim-server`, `lloesche/valheim-server` | None registered | — |
| Terraria | `hexlo/terraria-server-docker`, `beardedio/terraria`, `ryshe/terraria` | Process stdin | — |

These ports are console ports, not player connection ports. Minecraft's
`rcon-cli` runs inside the game container; its RCON port does not need to be
published for Ludock. Other network adapters require a reachable address,
enabled protocol, and configured credentials. Put Ludock and those servers on
a suitable Docker network. Do not expose RCON or Telnet directly to the public
internet.

## Capability matrix and evidence

Recognition does not establish that a particular game release is ready for
all operations. Status values are `supported`, `conditional`, `unsupported`, and
`unverified`. The current matrix applies as follows to every repository listed
above:

| Capability | Status | Requirement / evidence |
| --- | --- | --- |
| Recognition | Supported | Exact suffix normalization and eligibility fixtures in `test/server-presets.test.ts` and `test/discovery.test.ts` |
| Console | Conditional, except Valheim: unsupported | Adapter configuration and transport fixtures in `test/game-console.test.ts` and `test/game-console-runtime.test.ts`; a protocol fixture is not a live-game compatibility test |
| Game-image platforms | Unverified | Check the chosen upstream image's manifest; Ludock's AMD64/ARM64 targets do not establish game-image architecture support |
| Backup consistency | Unverified per game | The generic backup requires stopped containers and rejects known shared writers; validate graceful shutdown and world integrity against each actual image |
| Readiness | Conditional | Docker health or running state; no game-specific player-connectivity probes are registered |
| Update / same-image recreation | Conditional | Requires a supported, registered Compose project; the image's startup game-update behavior is not verified by image recognition |

The generic backup/restore mechanisms have separate tests. They do not prove
that every game flushed its world cleanly before stopping. Live backups are
unsupported. Select an integration in Diagnostics for its current prerequisites
and source test references.

## Labels and custom images

| Label | Purpose |
| --- | --- |
| `ludock.enable` | Optional for recognized images; explicitly include or exclude a container |
| `ludock.name` | Display name; defaults to the container name |
| `ludock.game` | Integration override, such as `minecraft` or `terraria` |
| `ludock.console` | Adapter override; `disabled` or `none` disables commands |
| `ludock.console.host` | Console network host override |
| `ludock.console.port` | Console network port override |
| `ludock.console.password-env` | Name of the game-container environment variable holding the console password |
| `ludock.files` | Comma-separated approved container data paths; empty disables file access |

Supported console adapter values are `minecraft-rcon`, `source-rcon`,
`rust-webrcon`, `telnet-console`, and `stdin-console`. Credentials stay in the
backend. The password-env label contains a **variable name**, never its value.

For an unrecognized custom image:

```yaml
services:
  terraria:
    image: example/custom-terraria:latest
    stdin_open: true
    labels:
      ludock.enable: "true"
      ludock.game: "terraria"
      ludock.name: "Terraria with friends"
```

`stdin-console` requires an interactive server process with stdin kept open and
`StdinOnce` disabled. Verify that the selected image actually forwards its
process input. Unsupported or unconfigured consoles do not prevent separately
granted log access. Game-console access and log access are independent grants;
console-only users do not receive the general container log stream.

An administrator can use the separate interactive container shell. There are
no saved shell-command hooks in backup, update, or schedule configuration.

## File roots

Writable bind mounts and named volumes become file roots automatically, even
while a server is stopped. Ludock exposes container paths rather than host
source paths. Read-only mounts, broad/system directories, Docker sockets,
configured sensitive paths, and nested duplicate roots are excluded. It does
not expose the container's writable layer.

Use the actual data locations provided by your image. For example, restrict a
container's writable `/data` mount to two known subdirectories:

```yaml
labels:
  ludock.files: "/data/worlds,/data/config"
```

The directories must belong to approved writable mounts. A label cannot grant
access outside that boundary. Disable all file and derived backup roots with:

```yaml
labels:
  ludock.files: ""
```

File reads/downloads require `files.read`; mutation additionally requires
`files.write`. Paths and symlink chains are revalidated inside isolated helpers.
Symbolic-link traversal, root deletion, `../`, and unsafe nested mount paths are
rejected. Approved nested mounts remain accessible, but downloading, deleting,
or renaming a parent containing an excluded mount is blocked. File downloads and
overwrites reject hard links; directory downloads also reject symlinks and
special files. Backups require physically distinct directory roots without
nested mounts and reject links and special files; choose suitable subdirectories
if the image's layout includes them.

See [Operations](./OPERATIONS.md) for helper permissions, destination mounts,
stop-only backup behavior, and restore recovery.
