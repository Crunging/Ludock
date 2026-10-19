# Go rewrite progress

Option B from [PLAN.md](./PLAN.md): Go backend, server-rendered templ pages,
htmx 2 + SSE extension, tiny vanilla JS (or Alpine CSP build) for local UI
state, xterm.js for the console. One long-lived branch `go-rewrite`, one PR
at the very end (not opened yet). Update this file after every area.

## Status by area

| # | Area | State |
|---|------|-------|
| 1 | Scaffold (go.mod, `cmd/ludock`, config, logging, health, Go CI) | done |
| 2 | DB (modernc sqlite, v6–v9 migrations, identity key, account repos, v0.5.0 fixture) | done |
| 3 | Auth/security core (argon2id, sessions, setup window, throttles, origin, client IP, grants, logical-server identity) | done |
| 4 | Docker layer (SDK client, discovery, events, logs, attach/exec) | next |
| 5 | Helper subcommands replacing `bun -e` helper containers | todo |
| 6 | Files / tar / backups (path containment, 512 KiB preview cap) | todo |
| 7 | Schedules / jobs / operations / locks / notifications / monitoring / history | todo |
| 8 | Console runtimes (attach, RCON/telnet) + compose via `docker compose` exec | todo |
| 9 | Pages: layout/auth (cookie + CSRF), dashboard SSE, server detail, console/logs, schedule editors, files/backups, admin | todo |
| 10 | Playwright journeys against the real Go binary with a fake Docker | todo |
| 11 | Dockerfile (multi-arch, small, `ludock healthcheck`), CI cutover | todo |
| 12 | Delete Bun/TS code, docs, release-please `simple` | todo |

## Layout

- `cmd/ludock`: `serve` (default), `recover <username>` (reads
  `LUDOCK_RECOVERY_PASSWORD`), `healthcheck`, `version`. Helpers go here as
  `ludock helper <name>`.
- `internal/config`: env vars, same names as v0.5.0.
- `internal/logging`: console lines in the old format
  (`ISO LEVEL [component] message {json}`), redaction (`Redact`,
  `IsSensitiveKey`), 1000-entry ring buffer for the admin log page (`List`).
- `internal/apperr`: public errors (`Code`, `Status`, `Message`, `RetryAfterMs`).
- `internal/db`: `Open`, migrations (verbatim SQL), identity key
  (`LoadKey`, #49 mode check), fingerprints, users/sessions/login_attempts/
  audit/settings repos, guarded mutations (`CreateFirstUser`,
  `ChangePassword`, `AdminMutation`, `RemovesLastAdmin`).
  `internal/db/dbtest`: `Open`, `OpenV050`, `V050Vectors`.
- `internal/jscompat`: JSON.stringify string quoting, encodeURIComponent,
  localeCompare (x/text/collate root), path.posix.normalize, UTF-16 length.
- `internal/identity`: logical servers (`Store.Reconcile`, `ResolveBinding`,
  `AssertObservedBinding`, `ReviewBinding`, `BindingDigest`).
- `internal/authz`: capabilities, role ceilings, `Effective`, `Assert`
  (404 hidden vs 403 forbidden, audits non-read denials), `SetUserGrants`.
- `internal/auth`: `Argon2` hasher, `PasswordWork` limiter, `SetupWindow`,
  `Service` (CompleteSetup, Login, Authenticate, Session, Recheck, Logout,
  ChangePassword, Sessions, RevokeSession, CreateUser, UpdateAccess,
  DeleteUser, ResetPassword, RecoverPassword), `IsSameOrigin`,
  `IsExternalHTTPS`, `ClientIP`, `SessionToken`.
- `internal/web`: `Server.Handler()` with security headers, cross-origin
  rejection for unsafe methods, panic recovery, `/api/v1/health` (5 s cache).

## Decisions

- Module path `github.com/Crunging/Ludock`, Go 1.27, `CGO_ENABLED=0` friendly
  (modernc). Pool of 4 connections, `_txlock=immediate`, per-connection
  pragmas `foreign_keys`, `busy_timeout(5000)`, `synchronous(FULL)` via DSN;
  WAL set after migrations. `:memory:` uses one connection.
- Existing DBs are inspected read-only (`immutable=1` unless a WAL/journal
  exists) before anything writes, as in v0.5.0.
- Read-then-write sequences that were atomic in single-threaded JS are now
  single transactions or compare-and-swap updates (first-admin insert,
  password change + session replacement, admin mutations recheck the
  session and admin role inside the transaction, hash upgrade CAS, login
  throttle counters).
- `LUDOCK_API_TOKEN` and the `/api/v1` JSON API are dropped (option B has no
  JSON API). `/api/v1/health` stays for monitors and the image healthcheck.
  Old operations whose actor is `api-token:…` will fail closed (no account).
  The `CredentialFingerprint` helper is kept only for display/compat.
  Confirm with Logan before the final PR; mention in the PR description.
- Default `PORT` is 3000 (was 3001 in dev; the image always set 3000).
- `LUDOCK_DB_PATH` defaults to `data/ludock.db`; the image sets
  `/data/ludock.db` (NODE_ENV no longer matters).
- Argon2id: m=65536, t=2, p=1, 32-byte salt, 32-byte key, PHC format, same
  as Bun; parameter bounds as v0.5.0. Tests lower `argonMemoryKiB`.
- Planned for pages: CSRF token = hash of the session token (sent as form
  field / `X-CSRF-Token` header via `hx-headers`), in addition to the
  existing Origin / Sec-Fetch-Site checks and SameSite=Strict cookies.
  Avoid `hx-on` and inline scripts (CSP `script-src 'self'`); Alpine needs
  its CSP build or vanilla JS instead.

## Gotchas

- Binding fingerprints hash a JS `JSON.stringify` encoding sorted with
  `localeCompare`; `identity.BindingDigest` reproduces it and
  `TestV050Fingerprints` checks it against vectors from the Bun code.
- `internal/db/testdata/v050/` is a real v0.5.0 DB + key generated by
  `generate.ts.txt` (run with Bun 1.4.3 from a temp dir against
  `packages/backend/src`). Regenerate only while the TS code still exists.
- The old code compared lengths in UTF-16 units (Zod); use
  `jscompat.Length` for user-facing limits.
- `pkill -f ludock-bin` kills your own shell (the pattern is in the command
  line); stop test servers by PID.
- Local checks: `GOMAXPROCS=2 go test ./internal/<pkg>`; CI runs
  `.github/workflows/go.yaml` (gofmt, vet, `go test -race ./...`) on pushes
  to `go-rewrite`. The Bun `ci.yaml` only runs on PRs and `main`.

## Next steps

1. Docker layer: `internal/docker` with the official SDK (`client.WithHost`
   unix socket from `DOCKER_SOCKET`), port docker.ts/docker-client.ts/
   discovery.ts/events.ts/container-logs.ts/docker-stream.ts, and an
   interface so tests and Playwright can use a fake Docker. Wire the health
   check (`web.Server.Docker`).
2. Then helpers (area 5) — read `packages/backend/src/helpers/*.js` and
   `helper-scripts.ts`, `runtime-images.ts`, `mount-proof.ts`.
