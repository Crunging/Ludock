# Ludock

Ludock is a self-hosted control panel for existing Docker game servers: a
Bun backend, a React frontend built and served by Bun, and shared Zod contracts.

## Product priorities

- Manage existing servers without provisioning them or editing their owning
  manager's configuration. Docker Compose, Portainer, Dockge, or the Docker CLI
  remains the owner.
- Make self-hosting straightforward: useful defaults, readable units, clear
  next steps, and practical desktop and mobile controls.
- Keep access explicit, game data safe, and operations recoverable.

## Working defaults

Follow the maintainer's request. Workflow and style guidance are defaults, not
approval gates. Prefer straightforward modules, explicit dependencies, and
existing feature boundaries; refactor when it simplifies the change. Coordinate
shared boundaries when working in parallel.

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
  automatically discovered Compose sources. Validate transitive reads within
  approved roots, invoke argument arrays without a shell, and preserve initially
  stopped state.
- **Contracts and adapters:** change shared schemas with their producers and
  consumers. Keep protocol-specific console behavior in adapters.

## Local development and fixtures

- Use the latest stable Bun 1 release; `.bun-version` selects the major and
  `package.json` declares the minimum. Install with `bun install --frozen-lockfile`
  and start with `bun run dev`. Use the printed URLs and state path.
- Checkout state and cookies are separate, but Docker is not isolated by them.
  Development defaults to no Docker connection. Use a dedicated test daemon for
  Docker integration work and run one backend per Docker host.
- Use disposable databases, containers, volumes, and Compose projects. Clean up
  only resources you created. Do not put real credentials in fixtures.
- Stop your own instance with Ctrl+C or its captured PID. Do not kill by broad
  process-name matching or delete data to resolve locks.

See [docs/TESTING.md](./docs/TESTING.md#development) for setup, overrides, and lock recovery.

## Dependencies and pins

Maintain dependencies and pins during substantive development and release work,
including needed major upgrades. Address security findings. Review related
updates in batches, fix compatibility issues, and commit validated changes without
per-package approval or dependency bot PRs. Ask the maintainer only for product
decisions or unresolved blockers. Avoid dependency churn on unrelated small tasks.

Update `bun.lock` with dependency changes. Pin GitHub Actions to full upstream
commit SHAs and runtime images to verified multi-platform manifest digests, with
readable version tags/comments. Keep the Bun image reference identical in
`Dockerfile` and `packages/backend/src/runtime-images.ts`, on the major in
`.bun-version`. Inspect updates with `bun outdated --recursive`; use
`bun update --recursive` for compatible updates and review upstream migration
notes for major upgrades. Run the affected checks after updating the lockfile.
Resolve annotated action tags to their peeled commit (`^{}`). Verify image
indexes with `docker buildx imagetools inspect` for both `linux/amd64` and
`linux/arm64`, and update the Compose fixture's expected Alpine digest together
with the build pin. `docker build --pull` checks pinned artifacts, not newer tags.

`tar-stream` stays on 3.2.0 because 3.2.1 has incompatible header and stream types;
remove the constraint when the archive integration type-checks against a
compatible release.

## Verification

- Check affected entry points, permissions, and failure paths, including
  scheduled work that shares the behavior.
- Run focused tests, lint, and type checks for affected code. Test meaningful
  behavior rather than implementation details. Do not rerun successful checks
  unless something relevant changed.
- Documentation-only edits need content, link, and diff checks, not application
  tests or container builds.
- Use `bun run check` for broad changes or integration concerns.
- Run relevant browser checks after building the frontend for interaction changes.
- Build the image when an image/runtime change needs local validation. Validate
  Compose configuration changes with `docker compose config`. Check affected
  runtime/helper behavior on both `linux/amd64` and `linux/arm64` when architecture
  matters; CI and release validation cover the full matrix.
- Report what ran and any material gaps. [docs/TESTING.md](./docs/TESTING.md#checks) has check
  commands; [docs/TESTING.md](./docs/TESTING.md) covers feature-specific failure,
  recovery, permission, and platform scenarios. Consult the relevant sections
  when changing those features.

## Commits and work artifacts

Validated commits are authorized without per-commit approval. Use Conventional
Commits for coherent changes. Pull requests should explain the problem, resulting
behavior, and validation.

Leave release versions to release-please. Do not advance the root package version
or release manifest in ordinary feature or fix PRs. The final squash commit type
controls release classification: `fix`, `perf`, `refactor`, and `revert` produce
patches; `feat` produces a minor release. Other non-breaking types do not trigger
releases. Merge dependency/pin updates before the generated release PR. A root
package version change on `main` triggers stable publication; other code pushes
publish nightly. Preserve already published versions and tags.

Keep private notes, temporary plans, and scratch outside the repo; do not add
gitignore entries for them. Commit reusable scripts, fixtures, and design docs
when they are part of the delivered change.

## Documentation and repository map

Update affected guidance; prefer existing sections. User docs should help
complete tasks; architecture docs should explain durable decisions and constraints.

- `packages/backend/src`: API, Docker access, identity, adapters, and operations.
- `packages/frontend/src`: pages, components, and server-detail feature panels.
- `packages/shared/src`: authoritative schemas, types, and shared helpers.
- `packages/backend/test`, `packages/frontend/test`, `packages/frontend/e2e`, and
  `scripts/test`: backend, component, browser, and development-runner checks.
- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md): current implementation boundaries.
- [docs/TESTING.md](./docs/TESTING.md): development setup, check commands, and regression scenarios.
- [docs/BRAND.md](./docs/BRAND.md): artwork sources and interface color conventions.
- [README.md](./README.md), [docs/OPERATIONS.md](./docs/OPERATIONS.md), and
  [docs/GAME-SERVERS.md](./docs/GAME-SERVERS.md): setup and user guidance.

## Interface style

Preserve keyboard access, mobile usability, form drafts, and useful empty/error
states. Obsolete responses must not overwrite newer input.

Favor readable tables, compact actions, clear status, and obvious next steps.
Distinguish dangerous actions from ordinary controls. Avoid promotional copy,
ornamental gradients, decorative metrics, excessive cards, and redundant badges.

Use the existing four-tile Ludock mark and shared accent tokens. The inline mark
is `LudockMark.tsx`; `public/ludock-app.svg` is the source for the Apple touch
icon, and `public/ludock-mark.svg` is the standalone vector. Keep the solid
favicon variant legible at 16px and status colors distinct from action colors.
