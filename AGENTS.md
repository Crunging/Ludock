# Ludock

Ludock is a self-hosted web panel for Docker game servers that already exist.
Compose, Portainer, Dockge, or the Docker CLI owns each server's
configuration; Ludock never creates servers or edits their definitions.

It is one Go binary (`cmd/ludock`) serving server-rendered templ pages with
htmx; the code is in `internal/`. `cmd/fakedocker` is an in-memory Docker
Engine API for development and tests. Never point development or tests at a
Docker daemon running real game servers.

There are no users yet: don't add compatibility shims, legacy aliases, or
migrations for old development states unless asked. The schema starts at
version 1; add changes as new consecutive entries in
`internal/db/migrations.go` and never edit a released one. After editing
`.templ` files, run `go tool templ generate -path internal/web/views` and
commit the output.

## Checks and tests

Before finishing, run `gofmt -l cmd internal`, `go vet ./...`, the Go tests for
the packages you touched, and the journeys if you changed anything a user can
see (`cd e2e && npx playwright test`).

Prefer end-to-end tests. The Playwright journeys in `e2e/journeys` run the real
binary against `cmd/fakedocker`; add or extend one for any behavior a user can
reach in the browser. Write a Go unit test only when it is clearly the better
fit: logic with many edge cases (time math, parsers, path and tar handling),
requests the UI never sends (forged or cross-site requests, missing grants),
and failures or races a journey can't trigger (a database error, a revocation
between two steps). Name Go tests after the code and behavior in a few words,
such as `TestRestoreSpacePerDisk`, and describe the scenario in a comment above
the test.

## Comments

Agents write and read this code, and every comment costs context. Comment only
what the code can't say: an invariant, a safety or ordering constraint, a
non-obvious reason, or an outside fact such as a Docker quirk. Don't restate
the code, walk through alternatives, or list what something doesn't do. Keep
doc comments to a sentence or two, and skip them on names that explain
themselves. Tests keep their scenario comment.

## Commits

Commit with Conventional Commits. Release Please owns versions,
`CHANGELOG.md`, and `.release-please-manifest.json`; don't edit them or move
published tags.

## Safety rules

- **Discovery is not permission.** Only grants make a server visible to
  non-admins, within their role's ceiling. Check capabilities again while long
  work runs (`startAction`, console `Access.Allowed`, `Jobs.Actor`).
- **Logical IDs are not Docker IDs.** URL IDs never reach Docker. Right before a
  mutation, confirm the binding with `Servers.ResolveAuthorized` or
  `Identity.AssertObservedBinding`.
- **Admin writes recheck the admin.** Admin-only changes run inside
  `db.AdminMutation`, which checks the exact session and role in the same
  transaction.
- **Paths stay inside approved roots.** File reads go through file
  descriptors that don't follow symlinks.
- **Secrets stay out of output.** Environment values, labels, tokens, webhook
  URLs, and host paths never appear in logs, pages, audit details, or public
  errors (`apperr`). Console and log output goes through redaction.
- **Operations survive restarts.** Persist them before they run, and reconcile
  interrupted ones instead of replaying them. Hold locks until helpers that
  may still be writing (any failed or uncertain command) are confirmed removed;
  idle helpers left by a successful command exit on their own. Backups and
  restores keep the server stopped, then return it to its initial running
  state. Requests can't take locks until startup recovery finishes
  (`locks.Table.Fence`).
- **Transactions read only through themselves.** Write transactions take the
  SQLite write lock immediately and the pool has four connections, so a read
  through another connection inside one can wait on a writer queued behind it.
  Run checks before the transaction and verify inside it through `tx`.
- **Updates need an admin** and go through the Engine API alone. The
  replacement is created under a temporary name discovery ignores and must
  start cleanly before the original is removed; until then the original stays
  the server and a failure restores it.
- **Forms are same-origin.** Every unsafe request carries the session's CSRF
  token and is re-authenticated after its body is read; pages never run inline
  scripts.
