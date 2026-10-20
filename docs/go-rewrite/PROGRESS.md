# Go rewrite progress

Option B from [PLAN.md](./PLAN.md): Go backend, server-rendered templ pages,
htmx 2 + SSE extension, tiny vanilla JS (or Alpine CSP build) for local UI
state, xterm.js for the console. One long-lived branch `go-rewrite`, one PR
at the very end (not opened yet). Update this file after every area.

## Status by area

| # | Area | State |
|---|------|-------|
| 1 | Scaffold (go.mod, `cmd/ludock`, config, logging, health, Go CI) | done |
| 2 | DB (modernc sqlite, fresh initial schema v1, identity key, account repos) | done (v0.5.0 compatibility dropped 2026-10-19) |
| 3 | Auth/security core (argon2id, sessions, setup window, throttles, origin, client IP, grants, logical-server identity) | done |
| 4 | Docker layer (Engine client, discovery, observation, events hub, fake Docker) | done (container log streaming moves to area 8) |
| 5 | Helper subcommands replacing `bun -e` helper containers | done (network-console helper moves to area 8) |
| 6 | Files / tar / backups (path containment, 512 KiB preview cap) | done |
| 7 | Schedules / jobs / operations / locks / notifications / monitoring / history | done |
| 8 | Console runtimes (attach, RCON/telnet) + compose via `docker compose` exec | done |
| 9 | Pages: layout/auth (cookie + CSRF), dashboard SSE, server detail, console/logs, schedule editors, files/backups, admin | done |
| 10 | Playwright journeys against the real Go binary with a fake Docker | done (e2e/, 14 journeys, CI job "Journeys") |
| 11 | Dockerfile (multi-arch, small, `ludock healthcheck`), CI cutover | done (container journeys on amd64 + arm64 in CI) |
| 12 | Delete Bun/TS code, docs, release-please `simple` | done |
| 13 | UI redesign ("harbor chart": top harbor bar, signal-flag states, Archivo) | in progress: pages restyled; demo seed, README and PR screenshots next |

## Layout

- `cmd/ludock`: `serve` (default), `recover <username>` (reads
  `LUDOCK_RECOVERY_PASSWORD`), `healthcheck`, `version`. Helpers go here as
  `ludock helper <name>`.
- `internal/config`: env vars, same names as the Bun releases.
- `internal/logging`: console lines in the old format
  (`ISO LEVEL [component] message {json}`), redaction (`Redact`,
  `IsSensitiveKey`), 1000-entry ring buffer for the admin log page (`List`).
- `internal/apperr`: public errors (`Code`, `Status`, `Message`, `RetryAfterMs`).
- `internal/db`: `Open`, migrations (initial schema v1, `ApplicationID`
  "LDCK"), identity key
  (`LoadKey`, #49 mode check), fingerprints, users/sessions/login_attempts/
  audit/settings repos, guarded mutations (`CreateFirstUser`,
  `ChangePassword`, `AdminMutation`, `RemovesLastAdmin`).
  `internal/db/dbtest`: `Open`.
- `internal/textutil`: `Length` (UTF-16 units, like HTML minlength and
  maxlength), `Compare`/`CompareFold` (x/text collation for display order).
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
  tzdata). The ported DST cases pass (fall-back, spring gap, Lord Howe,
  Casey rollback).
- `internal/schedules`: `Input` (stored JSON shape, `Validate`,
  `ParseSaved` strict + defaults, `Capabilities`, `EditCapabilities`),
  `Service` (`List`, `Create`, `Delete`, `Update`, `SetEnabled`, `Run`,
  `Announce`, `Wait`), owner authority with `UnavailableError.Reason`,
  missed one-time runs recorded, enqueue + association in one tx,
  `Broadcaster` interface for countdown warnings (implemented in area 8).
  Requester capability checks run before transactions because denial
  audits write through another connection.
- `internal/history`: `Service.Operations` (access-filtered, cursor bound
  to filters), `Service.Audit`, `PublicActor` (api-token hiding).
- `internal/attention`: `Service.List` (binding, availability, schedule,
  operation items; `DiscoveryUnavailable`).
- `internal/netconsole`: Source RCON, Rust WebRCON (coder/websocket),
  Telnet; `Control{Authorize, AssertAccess}` before each write.
- `internal/console`: `Runtime.GameCommand` / `Shell` (in-container
  watchdog script, cancel exec, wait for exit before release, 4 MiB cap,
  stdin attach, network target resolution, network helper container in
  the game's netns), `Redactor`, `LogDecoder`, `Follow`, `Sessions`
  (`Authorize`/`Access.Allowed` per frame + 2 s idle inspect, `Logs`,
  `Console` over a transport-neutral `Channel`), `Broadcaster` for
  schedules.
- `internal/compose`: `Runner.Run` (exec, own process group, stderr
  dropped, 8 MiB stdout), `ParseFile` (yaml.v3 nodes: merge keys,
  duplicate keys, ≤20 aliases, tag allowlist), `ReadApprovedFile`,
  `DiscoverSource`, `Snapshots.Create`, `Updates.Validated`,
  `AssertSingleContainer`, `Capability`, `ValidateService`.
- `internal/backups` service: `List`, `Get`, `Open`, `Delete`,
  `SavedSettings`, `SaveSettings`, `Initialize` (auto-enable single root),
  `ForBackup`, `Status`, `Preflight`, `AssertReady`,
  `AssertNoOtherWriters`, `AssertSeparateDestination`, `Record`, `Prune`.
- `internal/jobs`: `Jobs.Register(runner)` for start/stop/restart (±backup),
  backup, restore (journals, rollback, fresh helper after a stopped one),
  update (Compose pull/up/verify); `Actor` (schedule revision recheck),
  save-world, stop reconciliation, recovery under saved lock keys.
  Tests run whole jobs on fakedocker with in-process helpers.
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
  same JSON protocols and fixed failure texts as the Bun helper scripts; all
  walks use `openat` with `O_NOFOLLOW` from pinned descriptors (no
  `/proc/self/fd` paths); uploads commit only after the exact trailer;
  rename uses `RENAME_NOREPLACE`; tar headers ported byte-for-byte
  (ustar + PAX path records, base-256 sizes). `helper.Command(name, args…)`
  builds `[/usr/local/bin/ludock helper name json…]`; `helper.HostRoot` is
  `/host` (tests override).
- `internal/launch` (backend side): `Images` (`LUDOCK_HELPER_IMAGE` pinned
  digest, else self container's `sha256:` image ID, else fallback),
  `ValidateImage`, `Exec` (helper exec with stdin/trailer, access checks
  between chunks, 60 s / 30 min deadlines, 4 MiB stdout cap, 5 s upload
  cleanup grace, never blocks on a stuck input), `MountProof` and
  `AssertMountIdentities`, `HelperEnv`.
- `internal/app`: `New` builds every service; `Start` runs ops recovery
  then a 15 s `Tick` (refresh → schedules Run/Announce → monitor Check;
  backup auto-setup and notification delivery run beside it); `Stop`
  (events hub, runner, background, announcements, locks drained). `serve`
  shuts HTTP down first, then `App.Stop`, then closes the DB.
- `internal/web`: `Server{App,…}.Handler()` with security headers, cross-origin
  rejection for unsafe methods, panic recovery, `/api/v1/health` (5 s cache).
  `session.go`: `authed` (session; for unsafe methods bounded form read,
  CSRF check, then the session again) and `admin`; `fail` renders only
  public errors (Alert fragment for htmx, else ErrorPage; 401 → login).
  `action.go`: `startAction` = the old `serverAction` (capability + binding
  recheck, locks, 1 s revalidation, `Assert`/`AssertObserved` right before
  Docker calls). `routes.go` lists every page, fragment, and action.
  `pages_test.go` covers sessions, CSRF, discovery-not-permission, and a
  session revoked while the body arrives, against `app.New` + fakedocker.
- `internal/web/views`: templ components (`layout`, `auth`, `common`,
  `dashboard`; `display.go` holds the formatting ported from the frontend).
  Regenerate with `go tool templ generate -path internal/web/views` and
  commit the `_templ.go` files.
- `internal/web/assets`: embedded; `views.Asset(name)` returns
  `/assets/name?v=<hash>` (immutable cache); LICENSE files and dotfiles are
  not served. `ludock.css` is the 16 frontend CSS files concatenated in
  import order plus a short "Go server-rendered additions" block at the
  end. `ludock.js` is the only script of ours (data-attribute behaviors,
  no inline handlers). `vendor/` holds htmx 2.0.11, htmx-ext-sse 2.2.4,
  xterm 6 + fit/web-links addons, with their licenses.
- `internal/datasize`: `Parse`/`Format` from shared `data-size.ts`.
- `cmd/fakedocker`: dev daemon for hands-on page checks:
  `go run ./cmd/fakedocker -dir /tmp/ludock-run/docker -backups /tmp/ludock-run/backups`
  seeds a running Minecraft (Compose labels, RCON) and a stopped Valheim,
  plus the `ludock-dev` self container; helper execs run `helper.Main`
  in-process. Point Ludock at it with `LUDOCK_DOCKER_SOCKET=<dir>/docker.sock
  LUDOCK_SELF_CONTAINER=ludock-dev LUDOCK_BACKUP_ROOTS=<backups>
  LUDOCK_HELPER_IMAGE=ghcr.io/crunging/ludock@sha256:<64 hex>
  LUDOCK_LISTEN=127.0.0.1:3950 LUDOCK_DATA_DIR=<dir>`.

## Decisions

- Image: static binary built on the native platform and copied into Alpine
  with `docker-cli` and `docker-cli-compose`; `ENTRYPOINT ludock`, `CMD
  serve`, `LUDOCK_DATA_DIR=/data`, healthcheck `ludock healthcheck`. The
  release build passes `VERSION` for `ludock version`. `.dockerignore` is an
  allowlist (go.mod, go.sum, cmd/, internal/).
- No built-in fallback helper image: helpers reuse Ludock's own image, and
  native runs set `LUDOCK_HELPER_IMAGE` (a digest baked into the binary
  would always lag it).
- Container suite: `e2e/docker` journeys deploy the repository's
  `compose.yaml` with a test override against the CI runner's disposable
  daemon and a tag-based Alpine "game" stack (graceful on SIGTERM; Ludock
  refuses backups of servers that need forced termination, and updates need
  a pullable tag). They replace the Bun `scripts/test-*.mjs` harnesses.
- CI (`ci.yaml`): Go (gofmt, templ drift, vet, race tests), Journeys,
  Workflows (actionlint, `scripts/check-pins.sh`), Container (amd64, arm64;
  Trivy scan via `scripts/scan-image.sh`). The `go-rewrite` push trigger
  comes out at merge. Weekly security: govulncheck, npm audit of `e2e/`, scan
  of the published image. Release Please uses `release-type: simple`.

- Tests (Logan, 2026-10-19: prefers end-to-end tests): the Playwright
  journeys in `e2e/` are the main suite. Each covers one real user flow
  against the real binary and `cmd/fakedocker` (sign-in, dashboard, server
  page, backups/restore, schedules, console, files, sharing with an
  operator, account, history/logs/settings). Go unit tests stay only for
  tricky logic (schedule time, parsers, tar, paths, sorting), security
  checks (auth, CSRF, grants, sessions, sockets, path traversal, identity
  key, secrets in output), failure and recovery paths journeys cannot
  reach, and the DB schema. Tests that only re-checked a happy path were
  removed.
- Journeys use `@playwright/test` (plain JavaScript, its own `e2e/`
  package and lockfile) rather than playwright-go: playwright-go downloads
  its driver from CDNs that no longer host it. Global setup builds both
  binaries, starts them on fresh temp data, completes setup over HTTP, and
  saves the admin's session; journeys run serially in one worker. Run
  locally with `cd e2e && npm ci && LUDOCK_E2E_CHROMIUM=/path/to/chrome npx
  playwright test` (`LUDOCK_E2E_LOGS=1` shows server output,
  `LUDOCK_E2E_KEEP=1` keeps the temp data).

- Module path `github.com/Crunging/Ludock`, Go 1.27, `CGO_ENABLED=0` friendly
  (modernc). Pool of 4 connections, `_txlock=immediate`, per-connection
  pragmas `foreign_keys`, `busy_timeout(5000)`, `synchronous(FULL)` via DSN;
  WAL set after migrations. `:memory:` uses one connection.
- No compatibility with v0.5.0 databases (Logan, 2026-10-19; PLAN.md §1
  "SQLite schema"). One initial schema (version 1) replaces the Bun chain
  6–9; later changes go in as consecutive migrations. The SQLite
  `application_id` is "LDCK" (Bun used "LUDK"), so Bun databases are refused
  like any foreign file. Binding fingerprints use `encoding/json` and
  `path.Clean` instead of JS encodings; external identities use
  `url.QueryEscape`. The data model is otherwise unchanged.
- Existing DBs are inspected read-only (`immutable=1` unless a WAL/journal
  exists) before anything writes.
- Read-then-write sequences that were atomic in single-threaded JS are now
  single transactions or compare-and-swap updates (first-admin insert,
  password change + session replacement, admin mutations recheck the
  session and admin role inside the transaction, hash upgrade CAS, login
  throttle counters).
- `LUDOCK_API_TOKEN` and the `/api/v1` JSON API are dropped (option B has no
  JSON API). `/api/v1/health` stays for monitors and the image healthcheck.
  Operation and audit actors are always account IDs. Mention the API
  removal in the PR description.
- Configuration (Logan, 2026-10-19: rethink freely, no compatibility):
  every setting is a `LUDOCK_*` variable read and validated once by
  `config.Load`; invalid values stop startup with the variable's name
  (never its value). Services get values through constructors
  (`launch.NewImages`, `backups.NewStorage`, `compose.NewRunner`,
  `files.SetSensitivePaths`); nothing else reads the environment except
  `ludock recover` (`LUDOCK_RECOVERY_PASSWORD`) and compose's `PATH`.
  Set: `LUDOCK_LISTEN` (`:3000`, replaces HOST + PORT), `LUDOCK_DATA_DIR`
  (`data`; image sets `/data`; DB is `<dir>/ludock.db`, replaces
  LUDOCK_DB_PATH), `LUDOCK_LOG_LEVEL`, `LUDOCK_TRUSTED_PROXIES`,
  `LUDOCK_SETUP_CODE`, `LUDOCK_DOCKER_SOCKET`, `LUDOCK_SELF_CONTAINER`
  (default HOSTNAME), `LUDOCK_BACKUP_ROOTS` (default `/backups`; empty turns
  backups off), `LUDOCK_COMPOSE_ROOTS`, `LUDOCK_DOCKER_CONFIG`,
  `LUDOCK_SENSITIVE_PATHS`, `LUDOCK_HELPER_IMAGE`, `LUDOCK_MAX_UPLOAD_SIZE`
  (2 GiB). Dropped: `AUDIT_LOG_MAX_ROWS` (fixed 100,000 entries),
  `FILE_SENSITIVE_PATHS`, `LUDOCK_API_TOKEN`, `NODE_ENV`. The image must use
  `ENTRYPOINT ["/usr/local/bin/ludock"]` with `CMD ["serve"]` so
  `docker compose run --rm ludock recover <user>` works (OPERATIONS.md).
- Argon2id: m=65536, t=2, p=1, 32-byte salt, 32-byte key, PHC format.
  Tests lower `argonMemoryKiB`.
- Pages: CSRF token = sha256("ludock:csrf:v1:" + session token), sent as
  the `_csrf` field or the `X-CSRF-Token` header (`hx-headers` on
  `.app-layout`), on top of the Origin / Sec-Fetch-Site checks and
  SameSite=Strict cookies. No `hx-on`, no inline scripts, htmx
  `allowEval:false`; vanilla JS instead of Alpine.
- htmx swaps 4xx/5xx responses (except 401, which follows `HX-Redirect` to
  /login), so handlers answer errors with an `Alert` fragment.
- Dashboard live updates: one SSE stream (`/events`) per tab; `server`
  events trigger fragment reloads (`sse:server`), `expired` sends the tab
  to /login. Actions answer `HX-Trigger: servers-changed`.
- Server detail: tabs are links (`?tab=`), not client state; a remembered
  tab per server (React kept one in localStorage) is not carried over.
  Live regions (`#detail-live`, `#overview-live`, `#activity-list`,
  `#selected-operation`) reload themselves on `sse:server-<id>`,
  `sse:refresh`, `servers-changed`, and a poll (2 s while work is active,
  else 10 s); forms and dialogs sit outside them so drafts survive.
  `/events` sends `server` plus `server-<id>` (or `refresh`) per change.
  Actions posted from the detail page target `#detail-status`
  (`HX-Target`), so stop/restart answer `HX-Redirect` to the operation.
- Schedule editor is server-rendered: the form posts to itself and comes
  back with the draft on errors; a revision conflict shows the saved
  settings with "Keep my draft" (adopts the new revision without saving)
  or "Load saved settings" (`?edit=`). The next-run preview is a fragment
  (`POST /fragments/servers/{id}/schedules/preview`) refreshed on form
  changes. Generic `data-*` behaviors in ludock.js: `data-shown-by`,
  `data-hidden-by`, `data-shown-when="field=value"`, `data-days`,
  `data-local-timezone`, `data-default-local-timezone`,
  `datalist[data-timezones]`, `data-confirm-value`.
- Console: `/console/{id}?mode=logs|game|shell` is server-rendered (mode
  tabs are links; drafts and history live only in the page) and
  `assets/console.js` drives xterm. `/ws/console/{id}?mode=` (coder/websocket,
  same-origin only, session cookie) keeps the old limits: 12 per user, 8 per
  session, 100 total with 10 reserved for administrators, 64 KiB messages,
  1 MiB queued output (then close 1013), session recheck every 15 s and per
  message (1008 on revocation). The page treats the first server message as
  proof of authorization. `Server.CloseSockets` runs before HTTP shutdown;
  the handler waits for the close handshake before cancelling, or coder
  drops the close code.
- Files: `/files/{id}?root=&path=&q=&sort=` lists one folder; filter and
  sort run on the server (`textutil.CompareNatural`, folders first, dashes
  last). Dialogs (new folder, rename, delete, preview) are fragments opened
  with `data-open-on-load`; errors re-render the dialog with the draft,
  success retargets an alert to `#file-feedback` and fires `files-changed`.
  The preview is server-side (`files.Service.Preview`, at most 512 KiB read).
  Uploads (`assets/files.js`) are raw `PUT /files/{id}/upload` bodies with
  `X-CSRF-Token`, one file at a time, capped by `LUDOCK_MAX_UPLOAD_SIZE`.
- History pages (`/operations`, `/audit`) keep filters and the cursor in
  the URL; From/To are Unix ms in hidden fields that `data-ms-field`
  date inputs edit in local time. "Newer" returns to the first page (React
  kept an in-memory stack of earlier cursors). `/logs` filters the process
  ring buffer (1,000 entries) on the server and polls every 2 s.
- Users: every account change re-renders `#users`; the grant editor is a
  per-user fragment. ludock.js mirrors the old toggle rules; the server
  normalizes the same way (`views.GrantInputs`) and `SetUserGrants`
  enforces role ceilings.
- Settings: four forms post to themselves (`hx-target="this"` always
  with `hx-disinherit="*"`, or lazily loaded children replace the form).
  `servers.SetConnectionHost` validates and normalizes the host. Hands-on
  checks never enable Discord delivery: webhooks must be discord.com.
- `Referrer-Policy` is `same-origin` (was `no-referrer`): with no-referrer
  Chrome sends `Origin: null` on same-origin form posts, which the
  cross-origin check rejects.

- Docker: a small own Engine API client instead of the official SDK (plan
  said SDK). Reasons: the SDK pulls a large dependency tree into a static
  binary, the TS code already defined the exact behavior (API 1.44–1.55
  negotiation, no proxy, no daemon text in errors, hijack validation,
  request-started tracking), and the fake Docker can then be a plain HTTP
  server that the real binary talks to in Playwright journeys.
- Helper containers run the Go binary (`Entrypoint [/usr/local/bin/ludock]`,
  `Cmd [helper …]`), with `GOMAXPROCS=1`, `GOMEMLIMIT=96MiB`, and
  `PidsLimit` 64 (Bun helpers used 32) because Go runtimes start several threads.
- `launch.FallbackImage` is empty until a Go image is published: native
  runs outside a container need `LUDOCK_HELPER_IMAGE` for file/backup
  helpers. Pin the published digest before the final PR.
- Live updates use SSE instead of WebSockets for the dashboard; the events
  hub is transport-neutral (`Subscriber.Send` must not block).

- Redesign (area 13): not bound by the React look. A navy top "harbor bar"
  replaces the sidebar; all pages share tokens in `ludock.css`. The theme is
  dark only (Logan's preference, 2026-10-20); there is no light variant. Text is Archivo (variable width; headings
  run wide), code and the terminal use Atkinson Hyperlegible Mono. Server
  state is a small signal flag (`Flag`/`Pip` in `dashboard.templ`, tones
  ok/idle/active/attention/failed) next to the word, never color alone.
  Sign-in shows "LUDOCK" in signal flags (`hoist.templ`, CSS unfurl,
  reduced motion respected). The dashboard is one grid with subgrid rows so
  header and row columns line up. A dashboard refresh that would replace an
  open `<dialog>` is deferred until it closes (`htmx:beforeSwap` in
  `ludock.js`), so SSE updates can't destroy a confirmation mid-click.

## Gotchas

- Use `textutil.Length` for user-facing limits so they agree with the
  form's minlength/maxlength (UTF-16 units).
- Never edit the version 1 schema once released; add a migration.
- Chain verification and `git commit` with `&&` on one line: a failing
  `go vet` on an earlier line does not stop a later commit command.
  `gofmt -l` exits 0 even when it lists files: gate with
  `test -z "$(gofmt -l internal cmd)"`.
- Writing `\ufeff` through a heredoc/tool can produce a literal BOM in Go
  source (vet: illegal byte order mark); escape it in Python instead.
- `pkill -f ludock-bin` kills your own shell (the pattern is in the command
  line); stop test servers by PID.
- Elements toggled with the `hidden` attribute need the global
  `[hidden] { display: none !important; }` rule: React rendered them
  conditionally, so component CSS sets `display` and overrides `hidden`.
- templ: a `.` right after `@Component(...)` is a parse error; wrap the text.
- htmx inherits `hx-swap` and `hx-target`: self-reloading regions set
  `hx-disinherit="*"`, or buttons inside them swap the wrong way.
- Run post-swap JS on `htmx:load` (fires per new element, also for
  outerHTML swaps), not `htmx:afterSettle` with `detail.elt`.
- Local checks: `GOMAXPROCS=2 go test ./internal/<pkg>`; CI runs
  `.github/workflows/go.yaml` (gofmt, vet, `go test -race ./...`) on pushes
  to `go-rewrite`. The Bun `ci.yaml` only runs on PRs and `main`.

## Next steps

1. Before merging: remove the `go-rewrite` branch trigger from `ci.yaml`,
   decide whether `docs/go-rewrite/` stays, and open the single PR (Logan
   asked not to open it yet). Before/after screenshots of every screen are on
   the orphan `pr-assets` branch under `go-rewrite/` for the PR description.
2. Mention in the PR: no `/api/v1` JSON API or API token, a fresh database
   (old data volumes are refused), renamed `LUDOCK_*` settings, and the
   container suite replacing the Bun harnesses.
