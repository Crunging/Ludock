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
| 4 | Docker layer (Engine client, discovery, observation, events hub, fake Docker) | done (container log streaming moves to area 8) |
| 5 | Helper subcommands replacing `bun -e` helper containers | done (network-console helper moves to area 8) |
| 6 | Files / tar / backups (path containment, 512 KiB preview cap) | files, tar, backup storage done; backups.ts (job flows) after area 7 |
| 7 | Schedules / jobs / operations / locks / notifications / monitoring / history | next (locks done) |
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
- `internal/docker`: own Engine API client over the unix socket
  (`New`, `Ping`, list/inspect/start/stop/restart/remove/wait/stats/logs,
  exec create/start/inspect, attach, volumes, image inspect, events, `Pull`
  with per-record error checks, `CreateHelper`/`RemoveHelper`/
  `RemoveOperationHelper`), `APIError{Status}` (no daemon text),
  `StreamOpenError{RequestStarted}`, `Demux`/`Stdout` frame decoding,
  `DecodeJSONLines`. `internal/docker/fakedocker`: in-memory Engine on a
  unix socket (containers, events, logs, exec via `Engine.Exec`, attach,
  volumes, images, `Fail` switch, `Requests` log).
- `internal/games`: presets (`Integrations`, `Lookup`, `InferGameType`,
  `DisplayName`), console adapters (`ResolveAdapter`, `BroadcastTemplate`,
  `MinecraftExec`), discovery (`Evaluate`, `ParseAddress`, `ConnectionHost`,
  `ApprovedLabels`, `ComposeLabels`, `InvalidComposeIdentity`).
- `internal/files`: `Roots` (ludock.files / safe mounts), `SafeWritableDataMount`,
  `SensitiveSystemPath`, `Within`. Rest of file-storage comes in area 6.
- `internal/files` storage: `Service.Acquire` (fresh inspect, fresh roots,
  deepest owner mount, unsafe nested mounts blocked, mount proof, helper
  container, identity check, `check`, binding signature recheck), `List`
  (folders first, `LocaleCompareBase`), `Mkdir`, `Delete`, `Rename`,
  `Upload` (trailer commit + unconditional staging cleanup), `OpenDownload`
  (`*Download` ReadCloser verifies the helper exit on EOF), `Preview` /
  `ReadPreview` (512 KiB cap read server-side, NUL or invalid UTF-8 →
  binary), `NormalizeRelativePath`, `ValidateName`, `Error(code)`.
- `internal/locks`: `Table.Acquire(keys, background)` (path keys overlap
  parents/children, `BackgroundConflict.Cancel`), `NextRelease`,
  `ServerBusy`, `Drained`; `locks.Default`.
- `internal/tarstream`: port of tar.ts (`Walk` with `onChunk`,
  `DecodeHeader`, `EncodeHeader`, `Padding`, `End`, `FormatSeconds`).
  `testdata/v050-backup.tar` (written by the Bun encoder) is parsed and
  re-encoded byte-for-byte.
- `internal/backups` storage (port of backup-storage.ts): `Storage`
  (`ValidateSettings`, `ApprovedDirectory`, `AvailableBytes`,
  `AssertSpace`, `RemovePartial`, `Open`, `Remove`, `Validate`),
  `EntryMetadata`, `MappedHeader`, `ValidateEntry`, `ValidateStream`,
  `PlanRoots`, `HelperRoot`, `Helpers` (`NewDataHelper`, `Exec` 2 min,
  `WriteSnapshot`, `ExtractToStage`, `IsRunning`, wait-for-exit +
  terminate on expiry). Destination files are opened via `openat` on a
  pinned directory descriptor. backups.ts itself (jobs) comes after area 7.
- `internal/ops`: operation runner (port of operations.ts): `Runner`
  (`Register`, `Enqueue` with idempotency key + one-active-per-server in a
  tx, `Get`, `Start` reconciles `running` rows via `Handler.Recover` then
  marks them interrupted, `Stop`, `Wake`), lanes lifecycle=4 / data=1,
  `Context.Progress`, `Context.WithLocks` (yield + retry when another job
  holds keys, cancel background holders, fail on request-held keys),
  outcome + audit in one tx, `PublicActorID`.
- `internal/notify`: Discord webhook queue (`Configure` validation,
  `Notify` once per event key, `QueueTest`, `Retry`, `Deliver` ≤10 per
  call, 5-attempt cycles with backoff, no response/transport text stored,
  redirects refused, 30-day retention), `Deliveries`.
- `internal/monitor`: availability (`ParsePolicy`, `Get`, `Configure`,
  `SetIntentionalStop`, `Suppress`, `Problem`, `Check` per tick writing
  only transitions; Docker outage → `docker_unavailable`).
- `internal/schedtime`: port of shared schedule-time (`Slot`, `NextRun`,
  `OneTimeRun`, `OneTimeSlot`, `LocalDate`, `Location` with embedded
  tzdata). The v0.5.0 DST cases pass (fall-back, spring gap, Lord Howe,
  Casey rollback).
- `internal/schedules`: `Input` (stored JSON shape, `Validate`,
  `ParseSaved` strict + defaults, `Capabilities`, `EditCapabilities`),
  `Service` (`List`, `Create`, `Delete`, `Update`, `SetEnabled`, `Run`,
  `Announce`, `Wait`), owner authority with `UnavailableError.Reason`,
  missed one-time runs recorded, enqueue + association in one tx,
  `Broadcaster` interface for countdown warnings (implemented in area 8).
  Requester capability checks run before transactions because denial
  audits write through another connection.
- `internal/compose`: only `SourceLabels` so far.
- `internal/servers`: `Observer` (List with 30 s inspection reuse and
  4 workers, Inspect, Stats, ChangeState, Diagnostics), `StopTimeoutSeconds`,
  `StopRecoveryState`, `Secrets`; `Service` (single-flight `Refresh`,
  `List`, `ListSaved`, `Snapshot`, `ResolveAuthorized`, `VerifyAuthorized`,
  `LockKeys`, connection host setting) producing `View`s for templates.
- `internal/events`: `Hub` (Subscribe/Stop, Docker stream with reconnect,
  coalesced batches, one refresh per batch, per-subscriber recheck,
  content-free `refresh` events for former viewers).
- `internal/helper` (runs inside helper containers): `Main` dispatches
  `ludock helper file|idle|mount-proof|mount-identities|restore|restore-extract`;
  same JSON protocols and fixed failure texts as the v0.5.0 scripts; all
  walks use `openat` with `O_NOFOLLOW` from pinned descriptors (no
  `/proc/self/fd` paths); uploads commit only after the exact trailer;
  rename uses `RENAME_NOREPLACE`; tar headers ported byte-for-byte
  (ustar + PAX path records, base-256 sizes). `helper.Command(name, args…)`
  builds `[/usr/local/bin/ludock helper name json…]`; `helper.HostRoot` is
  `/host` (tests override).
- `internal/launch` (backend side): `Images` (FILE_HELPER_IMAGE pinned
  digest, else self container's `sha256:` image ID, else fallback),
  `ValidateImage`, `Exec` (helper exec with stdin/trailer, access checks
  between chunks, 60 s / 30 min deadlines, 4 MiB stdout cap, 5 s upload
  cleanup grace, never blocks on a stuck input), `MountProof` and
  `AssertMountIdentities`, `HelperEnv`.
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

- Docker: a small own Engine API client instead of the official SDK (plan
  said SDK). Reasons: the SDK pulls a large dependency tree into a static
  binary, the TS code already defined the exact behavior (API 1.44–1.55
  negotiation, no proxy, no daemon text in errors, hijack validation,
  request-started tracking), and the fake Docker can then be a plain HTTP
  server that the real binary talks to in Playwright journeys.
- Helper containers run the Go binary (`Entrypoint [/usr/local/bin/ludock]`,
  `Cmd [helper …]`), with `GOMAXPROCS=1`, `GOMEMLIMIT=96MiB`, and
  `PidsLimit` 64 (v0.5.0: 32) because Go runtimes start several threads.
- `launch.FallbackImage` is empty until a Go image is published: native
  runs outside a container need `FILE_HELPER_IMAGE` for file/backup
  helpers. Pin the published digest before the final PR.
- Live updates use SSE instead of WebSockets for the dashboard; the events
  hub is transport-neutral (`Subscriber.Send` must not block).

## Gotchas

- Binding fingerprints hash a JS `JSON.stringify` encoding sorted with
  `localeCompare`; `identity.BindingDigest` reproduces it and
  `TestV050Fingerprints` checks it against vectors from the Bun code.
- `internal/db/testdata/v050/` is a real v0.5.0 DB + key generated by
  `generate.ts.txt` (run with Bun 1.4.3 from a temp dir against
  `packages/backend/src`). Regenerate only while the TS code still exists.
- The old code compared lengths in UTF-16 units (Zod); use
  `jscompat.Length` for user-facing limits.
- Chain verification and `git commit` with `&&` on one line: a failing
  `go vet` on an earlier line does not stop a later commit command.
- Writing `\ufeff` through a heredoc/tool can produce a literal BOM in Go
  source (vet: illegal byte order mark); escape it in Python instead.
- `pkill -f ludock-bin` kills your own shell (the pattern is in the command
  line); stop test servers by PID.
- Local checks: `GOMAXPROCS=2 go test ./internal/<pkg>`; CI runs
  `.github/workflows/go.yaml` (gofmt, vet, `go test -race ./...`) on pushes
  to `go-rewrite`. The Bun `ci.yaml` only runs on PRs and `main`.

## Next steps

1. Area 7: `operations.ts`, `jobs.ts`, `schedules.ts` (+ shared
   schedule-time), `notifications.ts`, `monitoring.ts`, `history.ts`,
   `attention.ts`, `lifecycle.ts`, settings logic.
2. Then `backups.ts` job flows on top of `internal/backups` storage
   (tests: `storage_test.go` `startHelpers` maps `/mounts/<root>` back to
   host paths for in-process helpers).
