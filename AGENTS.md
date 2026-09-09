# Ludock

Ludock is a self-hosted control panel for existing Docker game servers. An
Express backend handles Docker access and durable operations; a React/Vite
frontend provides the browser UI. Shared Zod schemas define their contracts.

## Product priorities

1. **Manage what already exists.** Discover game servers without taking over
   their configuration. Docker Compose, Portainer, Dockge, or the Docker CLI
   remains the owner. Ludock does not provision servers or edit their owning
   manager's configuration.
2. **Make self-hosting straightforward.** Prefer useful defaults, clear setup
   steps, readable units, and errors that explain how to recover. Keep routine
   controls easy to find on desktop and mobile.
3. **Make access explicit.** Administrators see eligible servers. Other users
   need server and action grants, within their role's permissions. Permission
   checks belong in the backend as well as the UI.
4. **Protect game data.** Operations must recover safely after interruption.
   Backups, restores, and updates preserve the server's initial running state.
   Never reset existing game containers or persistent game data.

## Approach to changes

Solve the concrete problem with straightforward modules and explicit
dependencies. Avoid generic repository frameworks, dependency-injection
containers, and workflow engines. Keep a feature's contracts, behavior, UI, and
verification together; coordinate changes to shared boundaries during parallel
work. These instructions should help carry out the maintainer's request, not
create extra approval stops.

## Vocabulary

- **Maintainer**: the person directing development of this repository.
- **User**: someone using Ludock, with an administrator, operator, or viewer role.
- **Server**: a logical game server identified by a UUID. Its Docker container
  may change without changing its logical history.
- **Container**: the physical Docker resource bound to a server.
- **Eligible**: included by discovery policy; this alone does not authorize a user.
- **Owning manager**: the tool or configuration that created the game container.
- **Registered project**: an explicitly approved Compose project Ludock may update.
- **Operation**: persisted work with progress, authorization, locks, and recovery.

## Mistakes to avoid

1. **Using a live Docker host for development.** Separate checkout databases and
   cookies do not isolate Docker containers or volumes. Connect development only
   to a dedicated daemon, and run one backend per Docker host.
2. **Trusting a stale container binding.** Revalidate logical identity before
   mutation. Ambiguous or materially changed identities must fail closed; a
   public server UUID is never a substitute for a Docker container ID.
3. **Crossing storage or credential boundaries.** Keep file access inside approved
   roots and preserve symlink protections. Reject unrelated databases and
   unsupported schemas without altering them. Never expose console credentials,
   API tokens, passwords, or session tokens.

## Check every affected path

- **Entry points:** server rows, detail panels, direct API calls, and scheduled
  work may reach the same behavior. Keep their policies consistent.
- **Permissions:** preserve administrator, operator, and viewer boundaries.
  Lifecycle grants must not imply console, logs, files, backups, or schedules.
- **States:** consider empty setup, unavailable Docker, stopped servers, active
  operations, revoked access, failures, and recovery where relevant.
- **Contracts:** update shared schemas and their producers and consumers together.
- **Interface:** preserve keyboard access, mobile usability, form drafts, and
  useful loading, empty, and error states.
- **Protocols:** console behavior belongs in a game-specific adapter; one command
  transport does not work for every game.

## Local development

- Use Node.js 24 (at least 24.15.0) and the pinned pnpm 10.28.2.
- `pnpm install --frozen-lockfile` installs the workspace dependencies.
- `pnpm dev` runs the managed development instance and watches shared contracts.
  Read the printed URLs and state path; do not assume a fixed port.
- `pnpm dev --print-config` shows configuration without starting services.
- Each checkout gets separate state and session cookies. Docker is disconnected
  unless `DOCKER_SOCKET` explicitly connects a dedicated development daemon.
- Stop the instance you started with Ctrl+C or its captured process ID. Do not
  kill processes by broad name matching or delete data to resolve a lock.

See [CONTRIBUTING.md](./CONTRIBUTING.md) for overrides and lock recovery.

## Test fixtures

Use isolated databases and disposable containers, volumes, and Compose projects.
Exercise initial setup and configured accounts or servers when the change affects
both. Cleanup must target only resources created by the test. Keep real credentials
and live application or game data out of fixtures.

## Verification

- Run focused checks while developing. Test meaningful behavior and failure
  cases; avoid tests that duplicate implementation details.
- Run `pnpm check` before committing. It covers types, lint, unit/integration
  tests, and builds.
- Browser tests are separate: `pnpm --filter @ludock/frontend test:e2e` after the
  frontend build. Use relevant browser checks for interaction changes.
- After container or deployment changes, run `docker build -t ludock:test .`.
  Validate runtime dependencies and helper images on both `linux/amd64` and
  `linux/arm64`; a build on one architecture does not verify the other.
- Follow [TESTING.md](./TESTING.md) for acceptance criteria. Report what actually
  ran and any remaining gaps; test presence alone is not validation.

## Commits and pull requests

Work continuously through coherent features. Useful, validated commits are
authorized without per-commit approval. Use Conventional Commits with a relevant
scope when one is clear.

Keep pull requests focused. Explain the problem, resulting behavior, and relevant
validation in plain language. Include visual evidence when it helps review a UI
change, and keep review-only artifacts out of the repository.

## Documentation

Update existing guidance when a change makes it inaccurate. User documentation
should help someone complete a task; architecture documentation should explain
important boundaries and decisions. Avoid duplicating code, enumerating every
control, or appending a development diary.

- [README.md](./README.md): product overview and quick start.
- [docs/OPERATIONS.md](./docs/OPERATIONS.md): deployment and administration.
- [docs/GAME-SERVERS.md](./docs/GAME-SERVERS.md): game discovery and integration.
- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md): ownership and transport boundaries.
- [CONTRIBUTING.md](./CONTRIBUTING.md) and [TESTING.md](./TESTING.md): development
  setup and validation.

## Temporary work

Keep implementation plans, progress notes, research, and scratch files outside
the repository. Do not add gitignore entries for private working material.
Commits and pull requests record the delivered change.

## Architecture and invariants

Discovery determines eligibility in this order: an invalid enable label excludes
the container with an administrator diagnostic; false excludes; true includes;
unlabeled Compose one-offs exclude; recognized images include automatically;
everything else excludes. Unknown images therefore require `ludock.enable=true`.
Authorization is a separate decision.

Feature HTTP handlers live in `packages/backend/src/routes`. Direct server
actions declare their capability through `serverAction`; preserve its
authorization, lock, and cleanup lifetime. Durable operations persist progress,
coordinate conflicting server/project/shared-root work, recheck authorization,
and recover safely after interruption.

Backups keep the server stopped throughout copying. Persist its initial running
state and restore it safely afterward. Parent update or restore operations own
state restoration for their nested backups. Live backups are not supported.

Updates and forced recreation are administrator-only and require explicitly
registered Compose projects. Validate transitive reads within approved roots,
invoke argument arrays without a shell, and preserve initially stopped state.

Server-detail UI belongs in its feature panels. Keep drafts across navigation
and pending requests; obsolete responses must not overwrite newer input.
Shared contracts remain authoritative across HTTP and WebSocket boundaries.

## Repository map

- `packages/backend/src`: API, authentication, Docker access, identity, console
  adapters, filesystem helpers, and durable operations.
- `packages/frontend/src`: React pages, reusable components, and feature panels.
- `packages/shared/src`: API schemas and shared types and helpers.
- `packages/backend/test`: backend unit and HTTP integration tests;
  `packages/backend/test/typechecks` contains compile-time boundary checks.
- `packages/frontend/test`: component and permission tests;
  `packages/frontend/e2e` contains browser workflows.
- `scripts/dev.mjs` and `scripts/test`: managed development and its tests.

## Code and interface style

Keep dependencies visible and feature logic near its owner. Reuse shared
contracts instead of duplicating request and response types. Add comments where
they explain a constraint or decision that the code cannot make clear.

Build a practical control panel: readable tables, compact actions, and clear
status. Distinguish dangerous actions from ordinary controls. Avoid promotional
copy, ornamental gradients, decorative metrics, excessive cards, and redundant
badges. Favor a clear next step over more explanatory UI.
