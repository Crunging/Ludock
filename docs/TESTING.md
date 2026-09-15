# Development and testing reference

Use this reference when changing the corresponding feature. Choose scenarios
that exercise the affected behavior; this is not a checklist to complete for
every change. Start with the setup and check commands below.
These scenarios describe expectations, not recorded proof of passing tests.
Record the image digest, platform, fixture, and result for integration validation.

## Development

Use the latest stable Bun 1 release (`package.json` declares the minimum):

```bash
bun install --frozen-lockfile
bun run dev
```

The runner prints the frontend/API URLs and database path. Each checkout gets
separate ports, cookies, and persistent state under `~/.local/state/ludock/dev`.
Both servers bind to loopback, and Docker is disconnected by default. Set
`DOCKER_SOCKET` only for a dedicated test daemon; run one backend per Docker host
and use disposable game data.

Use `bun run dev --print-config` to inspect settings without starting services.
Overrides: `LUDOCK_DEV_HOME` (state directory), `LUDOCK_DEV_PORT` (frontend),
`LUDOCK_DEV_API_PORT` (backend; `PORT` also works), and `LUDOCK_DB_PATH` (database,
including `:memory:`). Never point development at production or unrelated data.

Stop with Ctrl+C; active operations may need time to finish. After a crash,
inspect the PID in `dev.lock` in the printed state directory or
`<database>.dev.lock` beside an overridden database. Remove a stale lock only
after verifying that its runner and backend have ended. Keep the database intact.

## Checks

```bash
bun run check                              # Types, lint, tests, and build
bun run --filter @ludock/backend test      # Focused backend suite
bun run --filter @ludock/frontend test     # Focused frontend suite
```

For browser changes, build and run the desktop/mobile fixtures:

```bash
bun run --filter @ludock/frontend build
(cd packages/frontend && bun x --bun playwright install chromium)
bun run --filter @ludock/frontend test:e2e
```

Browser tests use mocked APIs and no Docker connection. Append a spec name or
`--project=desktop` for focused checks. `LUDOCK_E2E_PORT` overrides port 4179;
`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` selects an existing browser. Failure traces
and reports are under `packages/frontend/test-results` and `playwright-report`.
Release-workflow tests also require Git, Bash, and `jq`.

For runtime or Docker integration changes, use a dedicated test daemon:

```bash
docker build --pull -t ludock:test .
docker pull "$(bun -p "(await import('./packages/backend/src/runtime-images.ts')).DEFAULT_HELPER_IMAGE")"
bun scripts/test-linux.mjs
bun scripts/test-compose.mjs
bun scripts/test-files.mjs
bun scripts/test-backups.mjs
```

The harnesses create and clean up their own fixtures. `LUDOCK_TEST_IMAGE`
overrides `ludock:test`. Socket-enabled harnesses mount `/var/run/docker.sock`
and require host bind paths visible at identical absolute paths; Docker Desktop
aliases and remote daemons may need a native Linux test host. Validate affected
runtime/helper behavior on both `linux/amd64` and `linux/arm64`. Mocked tests do
not establish live-game compatibility. Use `docker compose config` for Compose
edits; documentation-only edits need link and diff checks.

## Dependency and image updates

Follow the scope and pin policy in [AGENTS.md](../AGENTS.md#dependencies-and-pins).
Inspect available versions with `bun outdated --recursive`; use
`bun update --recursive` for compatible updates. Review upstream migration notes
before major upgrades, check toolchain peer dependencies, and run
affected checks after updating `bun.lock`. Use `bun audit` for known dependency
vulnerabilities.

Resolve GitHub Actions tags to full upstream commits, peeling annotated tags
with `^{}`. Verify runtime image indexes with `docker buildx imagetools inspect`
for both `linux/amd64` and `linux/arm64`. Build and helper images must provide a
compatible Bun version, but need not have identical distributions or digests.
Update the Compose fixture's expected Alpine digest with the build pin. Readable tags or
comments should identify pinned versions. `docker build --pull` validates the
pinned artifacts; it does not check for newer tags. Validate changed images using
the platform checks below.

The default helper uses Bun's official distroless image, pinned in
`packages/backend/src/runtime-images.ts`. Helpers execute Bun directly and do
not require a shell, coreutils, or a package manager. Keep integration fixtures
compatible with that runtime instead of adding tools solely for tests. Verify
both architectures' security scans and storage/backup acceptance before changing
the digest. Application builds use upstream Bun Alpine independently; their final
Alpine image applies its own package updates.

Pull-request checks scan both the application and default helper. The daily
**Dependency security** workflow audits the lockfile and scans the published
`latest` application plus the helper selected by `main`. A finding must be fixed
in the affected package or image; do not disable scans or ignore an advisory to
make the run green. Existing installations adopt a new default helper when they
upgrade Ludock; an explicit `FILE_HELPER_IMAGE` override remains unchanged.

## Docker and architecture validation

Validate both `linux/amd64` and `linux/arm64` on their corresponding CI runners
or a suitable Buildx/emulation setup. A successful build on one architecture is
insufficient. Published images must contain both runtime platforms; check the
actual tag with `docker buildx imagetools inspect` before distribution.

For each platform:

- Build the production image and initialize fresh SQLite storage.
- Check `/api/v1/health`, the setup page, and static assets. Without a socket,
  health reports degraded; with the test daemon it reports healthy.
- Run the bundled `bun --version`, `docker --version`, and `docker compose version`.
- Resolve and run the digest-pinned default helper on that platform;
  verify file access, stopped-server backup, and restore against disposable volumes.
- Exercise Compose source validation inside the Linux Ludock runtime. Host
  macOS/Windows unit execution cannot establish descriptor-path compatibility.
- Verify no build-host-specific native artifact entered the target runtime.
- Run the repository's image vulnerability and workflow checks.

Docker Desktop, rootless Docker, custom socket paths, and external managers
need separate documented acceptance results. The image starting does not prove
that a given host's Compose bind paths and mount permissions work.

## Choose the affected suites

Test names document individual regressions; avoid maintaining a second assertion
list here. Backend suites are under `packages/backend/test`, component suites under
`packages/frontend/test`, and desktop/mobile fixtures under `packages/frontend/e2e`.

| Change | Start with these suites |
| --- | --- |
| Authentication and storage startup | `auth*`, `password`, `setup*`, `migrations`, `api-token-operations`; frontend `auth-recovery`, `account-history-recovery`, `login` |
| Discovery, bindings, and access | `discovery`, `identity`, `authorization`, `server-action`, `server-lifecycle`, `server-recovery`; browser `permissions`, `live-state` |
| Contracts and error responses | `contracts`, `native-http`, `request-security`, compile-only `typechecks/`; frontend `api` |
| File and helper lifetimes | `file-storage`, `file-helper-linux`, `docker-helpers`, `docker-storage.integration`; frontend/browser `files*` |
| Backups and recovery | `backups`, `backup-storage`, `backup-docker`, `restore-helper-script`, `restore-extract-script`, `backup-readiness`, `backup-routes` |
| Compose updates | `compose`, `compose-source`, plus `scripts/test-compose.mjs` in Linux |
| Queues, schedules, and monitoring | `operations`, `jobs`, `schedules`, `schedule-time`, `monitoring`; frontend/browser `schedules` |
| Notifications | `notifications`, `notification-routes`; frontend/browser `notification-deliveries` |
| History, attention, and logs | `history`, `attention`, `application-logs`, `events`; frontend/browser `history`, `needs-attention`, `activity-filters`, `application-log-filters` |
| Console and streams | `game-console*`, `console-access`, `container-logs`, `websocket-server`; frontend `console-recovery`, `websocket-recovery`; browser `console` |
| Page reads, settings, and navigation | `page-read`, `admin-recovery`, `workflows`, `server-detail-navigation`, `resolution-links`; browser `navigation`, `workspace`, `server-detail-guidance` |
| Build, development, and publishing | `scripts/test/`; backend `static-files`, `runtime-images`; production and browser checks above |

Run affected entry points as well as their underlying functions. For shared behavior,
include direct, queued, and scheduled callers. Check permissions, failure paths,
revocation during asynchronous preparation, and obsolete-response handling. UI
changes must preserve keyboard access, mobile usability, drafts, and truthful
loading/empty/error states.

## Disposable-server acceptance

Use a dedicated Docker host, disposable game data, a separate backup mount, and
fresh `/data`. Keep owning Compose files mounted read-only at identical absolute
paths on the daemon and inside Ludock. Harnesses clean up only their own fixtures.
A backup protects game data, not images or Compose configuration. External managers
are outside Ludock's in-process locks.

The automated suites above cover most feature rules. The following checks require
Linux, real Docker behavior, failure injection, or a human inspecting the interface.
Record the platform, image/helper digests, fixture, and outcome; a passing mock is
not evidence of live-game compatibility.

### Setup, grants, and deployment

- Follow the [setup procedure](../README.md) from empty storage. Verify one-time
  setup, restart persistence, HTTPS cookies, and account recovery. Preserve a copy
  of unrelated/unsupported fixture databases and confirm rejection leaves them intact.
- Give an operator start/stop access to two disposable servers. Check the same
  boundaries through UI, direct HTTP, and WebSocket requests. Add console, logs,
  files, backups, and schedules independently; revoke access while work prepares.
- Recreate a server through its owner and confirm the logical UUID/history/grants
  survive. Change game/mount identity and verify review is required. Docker outages
  must retain authorized history without enabling mutations or claiming an empty,
  healthy installation.
- Exercise the intended proxy/socket layout. Docker Desktop, rootless Docker,
  custom sockets, and external managers need their own acceptance results.

### Lifecycle, console, and files

- Exercise each supported adapter with a disposable game or protocol fixture:
  Minecraft `rcon-cli`, Source RCON, Rust WebRCON, 7DTD Telnet, and Terraria stdin.
  Check disconnects, cancellation, backpressure, split Unicode, and credential
  redaction. Reconnect must not resend commands.
- Verify server/project/shared-volume/overlapping-bind conflicts stay locked until
  helper or console cleanup completes, including lost Docker responses and client
  disconnects. Stop/restart confirmations must remain keyboard and mobile usable.
- Run file operations with running and stopped containers. Replace parents with
  symlinks during reads/writes, substitute the game during helper startup, and revoke
  access during preparation. No operation may reach outside its approved roots.
- Incomplete, oversized, cancelled, or revoked uploads must preserve original bytes
  and metadata. Cleanup removes only that upload's temporary sibling. Verify long
  Unicode archive paths and files over 8 GiB without truncated names or sizes.

### Backup, restore, and update recovery

- Verify separate destination mounts, capacity/reserve errors, retention, and
  protected restore targets. Preflight must create no helpers or archives and stop
  no servers; execution repeats its checks under locks.
- Check initial running and stopped states. Copying must occur entirely while
  stopped, with shared writers excluded. External restart/replacement invalidates
  copying; a failed/forced stop must not produce a completed backup.
- A restore validates binding and archive and creates a safety backup. Keep the
  server stopped between nested backup and replacement. Reject traversal, duplicate
  or oversized entries, links/special files, missing roots, and checksum mismatches.
- Interrupt restore before staging, while moving old data, during each replacement
  root, and during cleanup. Restart Ludock and verify journal rollback/cleanup and
  actual bytes in every root. Multi-root restore is not atomic. Invalid journals
  or uncertain recovery keep the server stopped; never delete staging to pass a test.
- Revoke authority during backup/restore/update preparation: new privileged steps
  stop, while rollback, cleanup, and safe initial-state restoration remain possible.
- Test ordinary update and forced recreation with automatically discovered Compose
  sources, transitive reads, literal dollar values, and unrelated services. Preserve
  initially stopped state and source discovery across both application and game
  recreation. No owner configuration may be rewritten.

### Scheduling, notifications, and interface

- Pause/edit a schedule while its operation is queued, revoke its action grant,
  and interrupt execution. Verify revisions and persisted results after restart.
  Test timezone transitions without catch-up destructive runs.
- Enable monitoring on a disposable server; verify outage/recovery notices after
  grace and suppression during Ludock operations or maintenance.
- Mock Discord for routine tests. Live delivery needs an explicitly authorized
  test webhook. Check saved enabled configuration, bounded retries, no mentions,
  and secret-free settings/history/errors.
- Inspect 320px and 390px layouts, keyboard tabs/dialogs, and direct links through
  sign-in and browser history. Preserve drafts across tabs and failed saves; revoke
  access or change bindings while requests are pending.
- Fail backup history, schedule history, and each settings read independently.
  Show a local retry, preserve unrelated drafts/controls, and prevent mutations
  that depend on unavailable or refreshing state. Polling must not overlap slow
  reads or let obsolete responses restore revoked data.
