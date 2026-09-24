# Ludock

Ludock is a self-hosted web panel for Docker game servers that already exist.
It discovers containers, then offers start/stop, consoles, file access, backups,
schedules, availability alerts, and Compose-based updates. Compose, Portainer,
Dockge, or the Docker CLI stays the owner of each server's configuration;
Ludock never provisions servers or edits their definitions.

There are no users yet. Don't add compatibility shims, legacy aliases, or data
migrations for old development states unless asked. The database baseline is
schema version 6 (released in v0.3.0); add schema changes as new consecutive
entries in `packages/backend/src/migrations.ts`.

## Layout

- `packages/backend`: Bun HTTP and WebSocket server.
  - `src/index.ts` starts the server; `src/app.ts` handles routing, auth, and errors.
  - `src/routes/`: API handlers.
  - `src/docker-client.ts`, `src/docker-transport.ts`: minimal Docker Engine client over the socket.
  - `src/database.ts`, `src/migrations.ts`: SQLite storage.
  - `src/operations.ts`, `src/jobs.ts`: durable background work.
  - `src/helpers/*.js`: programs that run inside disposable helper containers,
    embedded as text by `src/helper-scripts.ts`.
- `packages/frontend`: React 19 single-page app. Routing is in `src/navigation.tsx`
  (no router library).
- `packages/shared`: Zod request/response contracts. When you change a schema,
  update its producer and consumers in the same change.
- `scripts/`: dev runner, backend build, Docker test harnesses (`test-*.mjs`),
  and CI helpers.
- `docs/`: [Operations](./docs/OPERATIONS.md) (user procedures) and
  [Development and CI](./docs/DEVELOPMENT.md).

## Commands

```sh
bun install --frozen-lockfile
bun run dev                      # prints URLs and state path; no Docker unless DOCKER_SOCKET is set
bun run dev --print-config
bun run check                    # typecheck, lint, all unit tests, builds
bun run --filter @ludock/backend test
cd packages/backend && bun test --isolate test/<name>.test.ts
bun run --filter @ludock/frontend build && bun run --filter @ludock/frontend test:e2e
```

Docker harnesses (`scripts/test-*.mjs`) need the `ludock:test` image and a
dedicated test daemon with disposable data. Never point development or tests
at a daemon running real game servers.

## Conventions

- Use Bun for everything: runtime, tests (`bun:test`), bundling, child processes
  (`process.execPath`). TypeScript 7 (`tsc`) checks types, and Oxlint handles linting.
- Prefer Web APIs and `Uint8Array` over Node `Buffer` and `node:stream`; lint enforces this.
- Throw `AppError(code, status, message)` for errors users should see. Any other
  error becomes a generic 500 with a request ID, so don't put secrets or host paths
  in `AppError` messages.
- Send API responses through `respond(schema, value)`. Record route audit events
  with `audit(ctx, action, serverId?, details?)`.
- Only backend `src/` is type-checked. Backend tests are not, so run them after
  changing exports.
- Commit with Conventional Commits. Release Please owns versions, `CHANGELOG.md`,
  and `.release-please-manifest.json`. Don't edit those by hand or move published tags.
- Pin GitHub Actions to full commit SHAs and container images to digests.

## Safety invariants

Tests cover these rules. Keep them intact when refactoring.

- **Discovery is not permission.** Only grants make a discovered server visible
  to non-admins, and grants stay within the user's role ceiling. Check capabilities
  on the server, and check them again while long work runs (`serverAction`,
  `authorizeServerSocket`, `jobActor`).
- **Logical IDs are not Docker IDs.** Logical server UUIDs and Docker container IDs
  are separate branded types. URL IDs never reach Docker. Right before a mutation,
  confirm the current binding with `resolveAuthorizedServer` or
  `assertObservedServerBinding`.
- **Paths stay inside approved roots.** File and Compose reads stay inside approved
  roots, and directory traversal goes through file descriptors that don't follow
  symlinks.
- **Secrets stay out of output.** Environment values, labels, tokens, and webhook
  URLs never appear in logs, API responses, or audit details. Console and log
  output passes through redaction.
- **Operations survive restarts.** They are persisted before they run. An operation
  interrupted by a restart is reconciled, not replayed. Locks are held until helper
  containers are removed. Backups and restores keep the server stopped, then return
  it to its initial running state.
- **Updates need an admin.** Updates and forced recreation require an
  administrator and a Compose source discovered from container labels. They run
  `docker compose` with argument arrays, never a shell.
