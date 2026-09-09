# Ludock v2 Repository Guide

Ludock manages existing Docker game servers. Recognized images are discovered
automatically unless opted out; unknown images require `ludock.enable=true`.
It does not provision servers or edit their owning manager's configuration.

## Structure

- `packages/backend`: Express API, authentication, Docker access, console
  adapters, and file operations.
- `packages/frontend`: React and Vite control panel.
- `packages/shared`: authoritative API schemas and cross-package types.
- `packages/backend/test`: backend unit and HTTP integration tests.
- `packages/frontend/test`: frontend component and permission tests.
- `docs/ARCHITECTURE.md`: current feature ownership and transport boundaries.
- `scripts/dev.mjs`: isolated checkout development state, ports, and processes.
- `TESTING.md`: automated and manual acceptance criteria.

## Development

- Requires Node.js 24 and pnpm 10.
- Run focused checks during development and `pnpm check` before committing.
- Run `docker build -t ludock:test .` after container or deployment
  changes.
- Release images target `linux/amd64` and `linux/arm64`; validate runtime
  dependencies and helper images on both architectures.
- Work continuously through coherent features; no per-commit approval stops.
  Commits are authorized when useful and validated. Keep private implementation
  plans and progress notes outside the repository, without gitignore entries.
- Use straightforward modules and explicit dependencies. Avoid generic
  repository frameworks, dependency-injection containers, and workflow engines.
- Treat shared contracts as authoritative; update their producers and consumers
  together instead of duplicating request or response types.
- Keep feature HTTP handlers in `packages/backend/src/routes` and server-detail
  UI in its feature panels. Direct server actions declare their capability with
  `serverAction`; preserve its authorization, lock, and cleanup lifetime.
- Use `pnpm dev` for separate checkout state and session cookies. Docker is
  disconnected by default; connect only a dedicated development daemon and keep
  one backend per Docker host. Development state does not isolate Docker data.

## Constraints

- Eligibility precedence: invalid enable label excludes with an administrator
  diagnostic; false excludes; true includes; unlabeled Compose one-offs exclude;
  recognized images include automatically; all others exclude.
- Separate eligibility from authorization. Administrators see eligible servers;
  non-administrators need explicit server and action grants. Lifecycle grants
  must not imply console, logs, files, backups, or schedule permissions.
- Keep logical server UUIDs distinct from physical Docker container IDs. Resolve
  and revalidate bindings before mutation; fail closed on ambiguous or materially
  changed identities.
- Keep filesystem access within configured roots and preserve symlink traversal
  protections.
- Never expose console credentials, API tokens, passwords, or session tokens.
- Preserve administrator, operator, and viewer permission boundaries.
- Add protocol-specific console behavior through an adapter; do not assume one
  command transport works for every game.
- Backups require the server stopped throughout copying. Persist initial running
  state and restore it safely afterward. Parent update/restore operations own
  state restoration for their nested backups. Live backups are outside v2.
- Persist operation progress and recover safely after interruption. Coordinate
  conflicting server/project/shared-root operations and recheck authorization.
- Updates and forced recreation are administrator-only and use explicitly
  registered Compose projects. Validate transitive reads within approved roots,
  invoke argument arrays without a shell, and preserve initially stopped state.
- v2 uses fresh application storage; reject incompatible old databases without
  destroying them. Never reset existing game containers or persistent game data.

## Frontend

Build a practical control panel with clear status, readable tables, compact
actions, and useful empty/error states. Avoid promotional copy, ornamental
gradients, decorative metrics, excessive cards, and redundant badges. Preserve
keyboard access, mobile usability, and clear distinctions between dangerous
actions and ordinary controls. Enforce permissions in the backend as well as UI.
