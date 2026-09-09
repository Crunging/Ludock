# Contributing

Issues and focused pull requests are welcome. For security reports, follow
[SECURITY.md](./SECURITY.md) instead of opening a public issue.

## Development setup

Ludock requires Node.js 24 and pnpm 10.

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm dev
```

The repository contains an Express backend in `packages/backend`, a React
frontend in `packages/frontend`, and runtime-validated contracts in
`packages/shared`. The [architecture guide](docs/ARCHITECTURE.md) describes
feature ownership, route policies, and contract boundaries.

`pnpm dev` prints its frontend/API URLs and database path. Each checkout or
worktree gets stable preferred ports, a distinct session cookie, and persistent
state under `~/.local/state/ludock/dev`. Shared contracts rebuild automatically.
Both servers bind to loopback. Docker is disconnected by default; to use a
dedicated development daemon, set `DOCKER_SOCKET` when starting the runner.
Run only one backend per Docker host and use disposable game data.

Optional overrides:

| Variable | Purpose |
| --- | --- |
| `LUDOCK_DEV_HOME` | Parent directory for checkout-specific development state |
| `LUDOCK_DEV_PORT` | Frontend port; a conflict fails rather than changing it |
| `LUDOCK_DEV_API_PORT` | Backend port (`PORT` is also accepted) |
| `LUDOCK_DB_PATH` | Development database path, or `:memory:` |
| `DOCKER_SOCKET` | Socket for a dedicated development Docker daemon |

Use `pnpm dev --print-config` to inspect the selected configuration without
starting services or creating state. Existing database overrides must be regular
files without hard links; existing symlink targets and directory aliases resolve
to one database lock. Do not point development at a production database or
unrelated application storage.

Stop with Ctrl+C. The runner shuts down its children and releases checkout and
database locks after they exit. Backend reloads wait for active work to drain, so
a restart or stop can take longer while a data operation finishes. A crash may
leave `dev.lock` in the printed state directory or
`<database>.dev.lock` beside an overridden database. Inspect the recorded PID and
verify that run and its backend have ended before removing those lock files.
Do not remove the database or game data to resolve a development lock.

The lower-level `backend:dev` and `frontend:dev` commands remain available for
manual setups; they do not provide the managed runner's isolation or locks.

## Before opening a pull request

Run checks appropriate to the change. Focused tests, lint, and type checks are
usually enough during development; documentation-only changes need content,
link, and diff checks. CI runs the full source, browser, and container suites for
code changes. For broad changes or integration concerns, run the full source
check locally:

```bash
pnpm check
```

For image or runtime changes that need local validation, build:

```bash
docker build -t ludock:test .
```

Use `docker compose config` to validate Compose configuration changes. Validate
affected runtime and helper behavior on both Linux AMD64 and ARM64 when it
depends on architecture; CI and release validation cover the full platform matrix.

Add or update tests for behavior changes. Keep these product boundaries intact:

- Discover recognized images automatically, respect explicit opt-out, and require
  `ludock.enable=true` for unknown images.
- Keep file operations inside configured roots, including through symlinks.
- Preserve role ceilings and independent per-server action grants in every API,
  WebSocket, transfer, operation, and schedule.
- Implement new console protocols as adapters.
- Preserve logical identity checks, stop-only backups, and safe recovery.
- Reject unrelated databases and unsupported schemas without changing their
  contents.

Use [TESTING.md](./TESTING.md) for manual acceptance checks.
