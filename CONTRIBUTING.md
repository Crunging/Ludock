# Contributing

Issues and focused pull requests are welcome. For security reports, follow
[SECURITY.md](./SECURITY.md) instead of opening a public issue.

## Development setup

Use the latest stable Bun 1 release, with 1.4.2 as the supported minimum declared
in `package.json`. The `1` in [`.bun-version`](./.bun-version) selects the Bun major
used by CI. Bun provides the runtime, workspace package manager, JavaScript
bundler, and test runner. Follow the [Bun installation guide](https://bun.com/docs/installation)
for a new installation. Update a standalone installation with `bun upgrade`, or
use the package manager that installed Bun, keeping it on the Bun 1 release line:

```bash
bun --version
bun install --frozen-lockfile
bun run dev
```

The repository contains a Bun HTTP/WebSocket backend in `packages/backend`, a
React frontend in `packages/frontend`, and runtime-validated contracts in
`packages/shared`. The [architecture guide](docs/ARCHITECTURE.md) describes
feature ownership, route policies, and contract boundaries.

`bun run dev` prints its frontend/API URLs and database path. Each checkout or
worktree gets stable preferred ports, a distinct session cookie, and persistent
state under `~/.local/state/ludock/dev`. Shared contracts rebuild automatically.
Bun serves the frontend with hot reloading. The backend watcher waits for the
previous process to finish active work before restarting it. Both servers bind
to loopback. Docker is disconnected by default; to use a dedicated development
daemon, set `DOCKER_SOCKET` when starting the runner.
Run only one backend per Docker host and use disposable game data.

Optional overrides:

| Variable | Purpose |
| --- | --- |
| `LUDOCK_DEV_HOME` | Parent directory for checkout-specific development state |
| `LUDOCK_DEV_PORT` | Frontend port; a conflict fails rather than changing it |
| `LUDOCK_DEV_API_PORT` | Backend port (`PORT` is also accepted) |
| `LUDOCK_DB_PATH` | Development database path, or `:memory:` |
| `DOCKER_SOCKET` | Socket for a dedicated development Docker daemon |

Use `bun run dev --print-config` to inspect the selected configuration without
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

TypeScript remains responsible for type checking and shared declaration output.
`bun run build` bundles the backend entry points and the frontend HTML, scripts,
and styles for production.

## Updating tools and dependencies

Dependency manifests use standard compatibility ranges, and `bun.lock` records
the resolved workspace versions. Frozen installs use those recorded versions.
Coding agents handle routine updates during substantive development and release
work, and address security findings. Batch related updates, fix compatibility
issues, run the relevant checks, and commit validated changes. Dependency bot PRs
and per-package approvals are unnecessary; ask the maintainer only when a product
decision or unresolved blocker requires their input. Unrelated small tasks do not
need a dependency refresh.

Inspect available updates, then refresh within the manifest ranges:

```bash
bun outdated --recursive
bun update --recursive
bun run check
```

Review the manifest and lockfile changes together. Use `bun install` after editing
dependency manifests and include the updated lockfile. For major upgrades, review
upstream release notes and migrate the affected code and tests before committing.
Upgrading Bun does not update workspace dependencies automatically.

Runtime images follow `oven/bun:1-alpine` and `alpine:3`. Keep the Bun major aligned
across `.bun-version`, [Dockerfile](./Dockerfile), and the helper image in
[`runtime-images.ts`](./packages/backend/src/runtime-images.ts). GitHub Actions use
supported major tags such as `@v7`; use an available stable release tag when an
action does not publish a major tag. Review upstream changes when updating those
references. Security checks report findings without opening dependency update PRs.

`tar-stream` currently stays on 3.2.0. Its 3.2.1 patch introduces incompatible
header and Node stream type declarations; remove this constraint when the archive
integration type-checks against a compatible release. The lockfile keeps the
validated archive implementation reproducible in the meantime.

Run the source checks after updating dependencies or Bun, and add relevant
browser and platform checks from [TESTING.md](./TESTING.md) for affected behavior.

## Before opening a pull request

Run checks appropriate to the change. Focused tests, lint, and type checks are
usually enough during development; documentation-only changes need content,
link, and diff checks. CI runs the full source, browser, and container suites for
code changes. For broad changes or integration concerns, run the full source
check locally:

```bash
bun run check
```

For image or runtime changes that need local validation, build:

```bash
docker build --pull -t ludock:test .
```

The build uses floating Bun 1 and Alpine 3 base-image tags. `--pull` refreshes
those tags before building; an existing local image does not update itself.

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
