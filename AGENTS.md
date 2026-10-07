# Ludock

Ludock is a self-hosted web panel for Docker game servers that already exist.
Compose, Portainer, Dockge, or the Docker CLI stays the owner of each server's
configuration; Ludock never provisions servers or edits their definitions.

There are no users yet. Don't add compatibility shims, legacy aliases, or data
migrations for old development states unless asked. The database baseline is
schema version 6; add schema changes as new consecutive entries in
`packages/backend/src/migrations.ts`. When you change a contract in
`packages/shared`, update its producer and consumers in the same change.

Run `bun run check` before finishing. Browser tests run under Bun:
`bun --bun run --filter @ludock/frontend test:e2e`. Setup, Docker harnesses, and
release tooling are in [docs/DEVELOPMENT.md](./docs/DEVELOPMENT.md). Never point
development or tests at a Docker daemon running real game servers.

Commit with Conventional Commits. Release Please owns versions, `CHANGELOG.md`,
and `.release-please-manifest.json`; don't edit them or move published tags.

## Safety rules

Tests cover these. Keep them intact when refactoring.

- **Discovery is not permission.** Only grants make a server visible to
  non-admins, within their role's ceiling. Check capabilities again while long
  work runs (`serverAction`, `authorizeServerSocket`, `jobActor`).
- **Logical IDs are not Docker IDs.** URL IDs never reach Docker. Right before a
  mutation, confirm the binding with `resolveAuthorizedServer` or
  `assertObservedServerBinding`.
- **Paths stay inside approved roots.** File and Compose reads stay inside
  approved roots, through file descriptors that don't follow symlinks.
- **Secrets stay out of output.** Environment values, labels, tokens, webhook
  URLs, and host paths never appear in logs, API responses, audit details, or
  `AppError` messages. Console and log output goes through redaction.
- **Operations survive restarts.** Persist them before they run; reconcile
  interrupted ones instead of replaying them. Hold locks until helper containers
  are removed. Backups and restores keep the server stopped, then return it to
  its initial running state.
- **Updates need an admin** and a Compose source discovered from container
  labels, and run `docker compose` with argument arrays, never a shell.
