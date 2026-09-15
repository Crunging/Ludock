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
  `package.json` declares the minimum. Run `bun install --frozen-lockfile`, then
  `bun run dev`. Use the runner's printed URLs and state path.
- Checkout state and cookies are separate, but Docker is not isolated by them.
  Development defaults to no Docker connection. Use a dedicated test daemon for
  Docker integration work and run one backend per Docker host.
- Use disposable databases, containers, volumes, and Compose projects. Clean up
  only resources you created. Do not put real credentials in fixtures.
- Stop your own instance with Ctrl+C or its captured PID. Do not kill by broad
  process-name matching or delete data to resolve locks.

The runner binds to loopback and stores checkout state in `~/.local/state/ludock/dev`.
Use `bun run dev --print-config` to inspect settings without starting services.
Overrides: `LUDOCK_DEV_HOME`, `LUDOCK_DEV_PORT`, `LUDOCK_DEV_API_PORT` (`PORT` also
works), and `LUDOCK_DB_PATH` (including `:memory:`). After a crash, inspect the PID
in the printed state's `dev.lock` or `<database>.dev.lock` for an overridden path.
Remove a stale lock only after its runner and backend have ended.

## Dependencies and pins

Maintain dependencies and pins during substantive development and release work,
including needed major upgrades. Address security findings. Review related
updates in batches, fix compatibility issues, and commit validated changes without
per-package approval or dependency bot PRs. Ask the maintainer only for product
decisions or unresolved blockers. Avoid dependency churn on unrelated small tasks.

Update `bun.lock` with dependency changes. Pin GitHub Actions to full upstream
commit SHAs and runtime images to verified multi-platform manifest digests, with
readable version tags/comments. Build and helper images must meet the Bun major
and minimum version requirements; they may use different validated images when
upstream packaging or security fixes require it. Prefer suitable official images
over maintaining derivative images. Validate production runtime requirements;
do not turn test-fixture conveniences into runtime dependencies.

Do not preserve historical dependency restrictions without rechecking them.
Use TypeScript 7's native `tsc` for type checks. The `@typescript/native` alias
selects that compiler; the `typescript` alias uses Microsoft's TypeScript 6
compatibility package for `typescript-eslint` until it supports the native API.
Keep the linter's TypeScript API within its supported range. Use Node LTS types
compatible with Bun rather than automatically selecting the newest Node major.

Use `bun outdated --recursive`, `bun update --recursive`, and `bun audit`; review
upstream migration notes before major upgrades. Resolve Action tags to commits,
peeling annotated tags with `^{}`. Inspect both runtime platforms with
`docker buildx imagetools inspect`. `docker build --pull` validates pinned artifacts,
not whether newer tags exist. Update the Compose fixture's expected Alpine digest
with the build pin. The helper pin lives in `packages/backend/src/runtime-images.ts`;
helpers execute Bun directly in distroless Linux without shell tools.
Validate changed images with the platform checks below. PR and daily dependency
security workflows scan application/helper images and audit the lockfile; fix
findings in the affected artifact rather than disabling checks or ignoring advisories.

## Verification

- Run focused tests, lint, and type checks for affected code, covering entry
  points, permissions, failure paths, and scheduled work that shares the behavior.
  Test meaningful behavior; rerun successful checks only after relevant changes.
- Documentation-only edits need content, link, and diff checks, not application
  tests or container builds.
- Use `bun run check` for broad changes or integration concerns; run browser
  checks after building for interaction changes. Build and check changed runtime
  images, validate both supported architectures when relevant, and check Compose
  edits with `docker compose config`.
- Report what ran and any material gaps. CI and release validation cover the
  full platform matrix.

Focused suites use `bun run --filter @ludock/backend test` or
`bun run --filter @ludock/frontend test`. Browser checks use
`bun run --filter @ludock/frontend test:e2e` after the frontend build; append a
spec name or `--project=desktop` to narrow them. Install Chromium with
`bun x --bun playwright install chromium` from `packages/frontend` if needed.
`LUDOCK_E2E_PORT` overrides port 4179; `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` selects
an existing browser. Traces/reports are under `packages/frontend/test-results`
and `playwright-report`. Release-workflow tests require Git, Bash, and `jq`.

After building the frontend, regenerate README screenshots with
`bun run --filter @ludock/frontend screenshots`. This uses isolated demo fixtures
and writes `docs/screenshots`; inspect the images before committing them.

For runtime/Docker changes, build `ludock:test` and run the affected harnesses:
`bun scripts/test-linux.mjs`, `test-compose.mjs`, `test-files.mjs`, and
`test-backups.mjs` (all under `scripts/`). Pull the digest-pinned helper first.
`LUDOCK_TEST_IMAGE` overrides the application image. Socket-enabled harnesses
mount `/var/run/docker.sock`; Compose source paths must match absolute host paths.
Harnesses create and clean up their fixtures. Validate both `linux/amd64` and
`linux/arm64`, including startup with fresh SQLite storage, health/setup/static
assets, bundled Bun/Docker/Compose versions, and relevant storage/recovery behavior.
For recovery changes, interrupt multi-root replacement and verify actual bytes
after rollback/cleanup. Record image/helper digests, fixtures, and outcomes;
identify emulation. Docker Desktop, rootless Docker, custom sockets, proxies, and
external managers need separate acceptance results. Protocol fixtures do not prove
live-game console, graceful shutdown, or restored-world compatibility. Use mock
Discord endpoints unless a live test webhook is explicitly authorized.

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

Keep user procedures in Operations and development instructions here. Prefer
existing sections; do not duplicate feature descriptions or executable assertions.

- `packages/backend/src`: API, Docker access, identity, adapters, and operations.
- `packages/frontend/src`: pages, components, and server-detail feature panels.
- `packages/shared/src`: authoritative schemas, types, and shared helpers.
- `packages/backend/test`, `packages/frontend/test`, `packages/frontend/e2e`, and
  `scripts/test`: backend, component, browser, and development-runner checks.
- [README.md](./README.md) and [docs/OPERATIONS.md](./docs/OPERATIONS.md): setup and user guidance.

Shared schemas export source directly to Bun without a separate build/watcher.
`routes/server-action.ts` owns direct request authorization, locks, and cleanup;
`operations.ts`/`jobs.ts` own persisted work, with recovery in feature modules.
File/backup storage and mount proofs configure isolated helper programs in
`packages/backend/src/helpers/`; descriptor confinement requires Linux `/proc/self/fd`.
Deliberately public errors extend `AppError`; arbitrary exceptions stay private.
Frontend pages retain permissions and drafts; panels own independent reads and
feedback. Preserve specialized freshness rules for live server and file controls.

## Interface style

Preserve keyboard access, mobile usability, form drafts, and useful empty/error
states. Obsolete responses must not overwrite newer input.

Favor readable tables, compact actions, clear status, and obvious next steps.
Distinguish dangerous actions from ordinary controls. Avoid promotional copy,
ornamental gradients, decorative metrics, excessive cards, and redundant badges.

Use `LudockMark.tsx` and authored SVGs in `packages/frontend/public`; keep the
four tiles flat and upright. Label the component only without adjacent application
text. `ludock-mark.svg` is for 24px or larger; the solid favicon preserves the L
at small sizes. `ludock-app.svg` supplies the 180px Apple touch icon.
Use shared orange `--accent`, `--accent-hover`, and `--on-accent` tokens for controls;
keep blue/green/yellow/red status colors distinct.
