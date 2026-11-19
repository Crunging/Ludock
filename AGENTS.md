# Ludock

Ludock is a self-hosted web panel for Docker game servers that already exist.
Compose, Portainer, Dockge, or the Docker CLI owns each server's
configuration; Ludock never creates servers or edits their definitions.

## Product

Ludock is the simple, plug-and-play panel for game servers people already
run. Panels such as Pterodactyl own their servers; with Ludock the user stays
in control and keeps using their own tools beside it. Portainer knows
containers; Ludock knows games: who is online, saving before a stop, warning
players before a restart. It is for one person and a few friends first;
larger communities should still work, without their needs shaping the
everyday pages. Hold every change to that:

- **It works with no setup.** A new install finds its servers and is useful
  before anyone opens Settings. Detect what can be detected instead of asking
  for it.
- **Defaults do the right thing.** Every option ships with a value most people
  never change. Don't add a setting where one good behavior will do.
- **Advanced is optional and out of the way.** Options for unusual setups go
  behind an "Advanced" disclosure, a container label, or an environment
  variable. Nothing requires them, and everyday pages don't show them.
- **One obvious way.** Give a task one clear path, in plain words rather than
  Docker terms, with the common choice already made.
- **Nothing depends on Ludock.** It operates servers and never defines them.
  Removing it leaves every server as it was, and a backup is a plain tar
  archive that restores without it.
- **It acts on its own only where that is safe to assume.** Watching servers
  is on from the start. Anything that stops or restarts one, such as backups
  or automatic recovery, is offered in one click, not turned on silently.

The README is the pitch and quick start, and `docs/OPERATIONS.md` is the
reference: everyday tasks come first there, advanced options after.

## Code

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

## Frontend

The browser runs htmx 4, not 2, vendored with its SSE and preload extensions.
Attributes inherit only with `:inherited`, events are named like
`htmx:after:swap`, and a response updates other regions with `<hx-partial>`
(`views.Partial`). Scripts load in order at the end of `<body>`, never with
`defer`: a deferred htmx can start before its extensions register, which
leaves live updates unconnected. The Content Security Policy allows no inline
or evaluated script, which rules out `js:` values, `hx-trigger` filters,
`hx-on`, and `hx-live`; behaviors are `data-*` attributes handled in
`ludock.js`. After changing `internal/web/assets/vendor`, run
`scripts/check-vendor.sh`.

## Checks and tests

Before finishing, run `gofmt -l cmd internal`, `go vet ./...`,
`go tool staticcheck ./...`, the Go tests for the packages you touched, and the
journeys if you changed anything a user can see
(`cd e2e && npx playwright test`).

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

## Errors

Error strings follow Go style: lowercase, without a trailing period. Text
meant for people is an `apperr` error; pages, consoles, and startup logs show
its message and replace any other error with a generic one.

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
  token and is re-authenticated after its body is read; pages never run inline or
  string-evaluated scripts.
