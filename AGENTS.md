# Ludock

Ludock is a self-hosted web panel for Docker game servers that already exist.
Compose, Portainer, Dockge, or the Docker CLI stays the owner of each server's
configuration; Ludock never provisions servers or edits their definitions.

It is one Go binary (`cmd/ludock`) that serves server-rendered templ pages with
htmx; the code lives in `internal/`. There are no users yet. Don't add
compatibility shims, legacy aliases, or data migrations for old development
states unless asked. The database schema starts at version 1; add changes as
new consecutive entries in `internal/db/migrations.go` and never edit a
released one. After editing `.templ` files, run
`go tool templ generate -path internal/web/views` and commit the output.

Before finishing, run `gofmt -l cmd internal`, `go vet ./...`, and the tests
for packages you touched. The Playwright journeys in `e2e/` are the main test
suite; add or extend a journey for user-facing changes, and keep Go unit tests
for tricky logic, security checks, and failure paths journeys can't reach.
Setup, the fake Docker daemon, and release tooling are in
[docs/DEVELOPMENT.md](./docs/DEVELOPMENT.md). Never point development or tests
at a Docker daemon running real game servers.

Commit with Conventional Commits. Release Please owns versions, `CHANGELOG.md`,
and `.release-please-manifest.json`; don't edit them or move published tags.

## Safety rules

Tests cover these. Keep them intact when refactoring.

- **Discovery is not permission.** Only grants make a server visible to
  non-admins, within their role's ceiling. Check capabilities again while long
  work runs (`startAction`, console `Access.Allowed`, `Jobs.Actor`).
- **Logical IDs are not Docker IDs.** URL IDs never reach Docker. Right before a
  mutation, confirm the binding with `Servers.ResolveAuthorized` or
  `Identity.AssertObservedBinding`.
- **Paths stay inside approved roots.** File and Compose reads stay inside
  approved roots, through file descriptors that don't follow symlinks.
- **Secrets stay out of output.** Environment values, labels, tokens, webhook
  URLs, and host paths never appear in logs, pages, audit details, or public
  error messages (`apperr`). Console and log output goes through redaction.
- **Operations survive restarts.** Persist them before they run; reconcile
  interrupted ones instead of replaying them. Hold locks until helper containers
  are removed. Backups and restores keep the server stopped, then return it to
  its initial running state.
- **Updates need an admin** and a Compose source discovered from container
  labels, and run `docker compose` with argument arrays, never a shell.
- **Forms are same-origin.** Every unsafe request carries the session's CSRF
  token and is re-authenticated after its body is read; pages never run inline
  scripts.
