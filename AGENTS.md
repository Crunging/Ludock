# Ludock

Ludock is a self-hosted control panel for existing Docker game servers: a
Bun backend, a React frontend built and served by Bun, and shared Zod contracts.

## Product priorities

- Manage existing servers without provisioning them or editing their owning
  manager's configuration. Docker Compose, Portainer, Dockge, or the Docker CLI
  remains the owner.
- Make self-hosting straightforward: useful defaults, readable units, clear
  next steps, and practical desktop and mobile controls.
- Keep access explicit and game data safe. Reliability matters more than adding
  features that cannot recover predictably from failure.

## Working defaults

Follow the maintainer's request and use judgment. Workflow, layout, and style
advice here are defaults, not reasons to block a task or add approval stops.
Prefer straightforward modules, explicit dependencies, and existing feature
boundaries. Refactor or introduce abstractions when they clearly improve the
change; preserve the correctness requirements below across those changes.
Coordinate shared boundaries when working in parallel.

## Terms that matter

- **Server** is a logical game server with a UUID and durable history.
- **Container** is its current physical Docker resource; containers can change.
- **Eligible** means discovery includes a server, not that a user may access it.
- **Operation** is persisted work with progress and recovery.

## Correctness requirements

- **Discovery:** evaluate `ludock.enable` first: invalid values exclude with an
  administrator diagnostic; false excludes; true includes. Then unlabeled Compose
  one-offs exclude; recognized images include automatically; everything else excludes.
- **Access:** administrators see eligible servers; operators and viewers need
  explicit server and action grants within their role ceilings. Lifecycle grants
  do not imply console, logs, files, backups, or schedules. Enforce permissions in
  the backend and UI, and recheck authority during long-running work.
- **Identity:** keep logical UUIDs distinct from Docker IDs. Revalidate bindings
  before mutation and fail closed on ambiguous or materially changed identities.
- **Storage and secrets:** confine file access to approved roots, including
  through symlinks. Never expose credentials or tokens. Reject unrelated
  databases and unsupported schemas without changing them. Do not destroy or
  reset real or unrelated data as a development shortcut; authorized restore
  and recreation use their normal validation and confirmation safeguards.
- **Operations:** persist progress and recover after interruption. Coordinate
  conflicting server/project/shared-root work and preserve authorization, lock,
  and cleanup lifetimes, including those provided by `serverAction`.
- **Backups:** keep servers stopped throughout copying, persist initial running
  state, and restore it safely. Parent update/restore operations own state
  restoration for nested backups. Live backups are not supported.
- **Updates:** forced recreation and updates are administrator-only and require
  explicitly registered Compose projects. Validate transitive reads within
  approved roots, invoke argument arrays without a shell, and preserve initially
  stopped state.
- **Contracts and adapters:** change shared schemas with their producers and
  consumers. Keep protocol-specific console behavior in adapters.

## Consider affected paths

Check the entry points, permissions, and failure states relevant to the change,
including scheduled work when it shares the behavior. For UI changes, preserve
keyboard access, mobile usability, form drafts, and useful empty/error states.
Obsolete responses must not overwrite newer input. This is a prompt to consider
coverage, not a requirement to exercise every feature on every task.

## Local development and fixtures

- Use the latest stable Bun 1 release; `.bun-version` selects the major and
  `package.json` declares the supported minimum of 1.4.2. Refresh a local
  standalone installation with `bun upgrade`, keeping it on the Bun 1 release line.
- `bun install --frozen-lockfile` installs dependencies; `bun run dev` starts the
  managed instance. Read its printed URLs and state path rather than assuming
  ports. `bun run dev --print-config` inspects configuration without starting it.
- Own routine dependency maintenance during substantive development and release
  work, and address security findings. Update in coherent batches, fix compatibility
  issues, validate, and commit without per-package approval or dependency bot PRs.
  Keep `bun.lock` and frozen installs so tested dependency versions are reproducible.
  Handle major upgrades when needed; involve the maintainer only for product
  decisions or unresolved blockers. Avoid dependency churn on unrelated small tasks.
  See [CONTRIBUTING.md](./CONTRIBUTING.md#updating-tools-and-dependencies) for commands.
- Pin GitHub Actions to full upstream commit SHAs and runtime images to verified
  multi-platform manifest digests. Keep readable version tags/comments and keep
  the Bun image identical in the Docker build and
  `packages/backend/src/runtime-images.ts`, on the major in `.bun-version`.
  Refresh these pins in reviewed, tested batches during substantive development
  and release maintenance; do not create Dependabot update PRs or leave pins stale.
- Checkout state and cookies are separate, but Docker is not isolated by them.
  Development defaults to no Docker connection. Use a dedicated test daemon for
  Docker integration work and run one backend per Docker host.
- Use disposable databases, containers, volumes, and Compose projects. Clean up
  only resources you created. Do not put real credentials in fixtures.
- Stop your own instance with Ctrl+C or its captured PID. Do not kill by broad
  process-name matching or delete data to resolve locks.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for setup, overrides, and lock recovery.

## Verification

- Match checks to the change. Run focused tests, lint, and type checks for
  affected code; add tests for meaningful behavior rather than implementation
  details. Do not rerun successful checks unless something relevant changed.
- Documentation-only edits need content, link, and diff checks, not application
  tests or container builds.
- Use `bun run check` for broad changes or integration concerns. It is not required
  before every commit; CI runs the full source suite for code changes.
- Use relevant browser checks for interaction changes. The separate command is
  `bun run --filter @ludock/frontend test:e2e` after building the frontend.
- Build `ludock:test` with `docker build --pull -t ludock:test .` when an
  image/runtime change needs local validation; use
  `docker compose config` for Compose configuration changes. Validate affected
  runtime/helper behavior on both `linux/amd64` and `linux/arm64` when it depends
  on architecture. CI and release validation cover the complete platform matrix.
- Report what ran and any material gaps. See [TESTING.md](./TESTING.md) for check
  commands and acceptance criteria; choose the relevant parts.

## Commits and work artifacts

Useful, validated commits are authorized without per-commit approval. Work
through coherent changes, use Conventional Commits, and explain the problem,
resulting behavior, and relevant validation in pull requests.

Leave release versions to release-please. Do not advance the root package version
or release manifest in ordinary feature or fix PRs; see
[CONTRIBUTING.md](./CONTRIBUTING.md#release-versions).

Keep private notes, temporary plans, and disposable scratch outside the repo,
without adding gitignore entries for them. Reusable scripts, fixtures, and design
documentation belong in the repository when they are part of the delivered change.

## Documentation and repository map

Update guidance that a change makes inaccurate. User docs should help complete
a task; architecture docs should explain durable decisions and constraints.
Prefer updating an existing section over duplicating code or writing a diary.

- `packages/backend/src`: API, Docker access, identity, adapters, and operations;
  feature HTTP handlers currently live in `routes`.
- `packages/frontend/src`: pages, components, and server-detail feature panels.
- `packages/shared/src`: authoritative schemas, types, and shared helpers.
- `packages/backend/test`, `packages/frontend/test`, `packages/frontend/e2e`, and
  `scripts/test`: backend, component, browser, and development-runner checks.
- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md): current implementation boundaries.
- [README.md](./README.md), [docs/OPERATIONS.md](./docs/OPERATIONS.md), and
  [docs/GAME-SERVERS.md](./docs/GAME-SERVERS.md): setup and user guidance.

## Interface style

Favor readable tables, compact actions, clear status, and obvious next steps.
Distinguish dangerous actions from ordinary controls. Avoid promotional copy,
ornamental gradients, decorative metrics, excessive cards, and redundant badges.
