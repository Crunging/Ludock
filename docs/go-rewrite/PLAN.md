# Ludock → Go migration plan (draft, planning only)

Baseline: main @ 42304e4 (v0.5.0). Backend ≈17.3k lines of TS across ~70 files in packages/backend/src. packages/shared has ~400 Zod schema uses. Frontend ≈8.6k lines of TSX (React 19, xterm.js), with 12 Playwright specs. All of them mock the API with `page.route`.
Workflow for every PR: Opus builds, then CI plus the GitHub review bots (Codex connector, CodeRabbit) review. No Astra/Codex review steps. Squash-merge after triage.

## 0. Facts that shape the plan
- Bun API usage in the backend: CryptoHasher (14 places: sha256 and HMAC), Bun.write/file, Bun.sleep, stdio, concatArrayBuffers, bun:sqlite (database.ts, migrations.ts, fingerprints.ts), Bun.connect (docker-transport hijack streams and network-console RCON/telnet), FetchSession over the unix socket (docker-transport.ts), Bun.spawn (`docker compose` in compose.ts), Bun.JSONL (json-lines.ts), Bun.password argon2id (password.ts), Bun.serve plus native routes and websocket upgrade (index.ts, app.ts, websocket-server.ts), and gzip/precompressed static serving (static-files.ts).
- **Helper containers run this image with `bun -e <script>`** (file-helper, mount-identities, mount-proof, restore-extract, restore-helper, network-console). They're the biggest hidden dependency. In Go they become **subcommands of the same binary** (`ludock helper file …`), which is cleaner because no code is shipped as text.
- The runtime image needs `docker-cli` and `docker-cli-compose` for compose.ts. Either keep shelling out to the CLI (image grows by ~60 MB) or embed compose-go/the compose v2 library (see the open decisions).
- 15 SQLite tables: users, sessions, login_attempts, server_grants, settings, docker_hosts, logical_servers, server_bindings, schedules, schedule_announcements, operations, backups, audit_log, availability, notification_deliveries.
- The current Playwright suite never hits the real backend. It only proves the UI. Backend parity has to come from the container suites (scripts/test-*.mjs), which test the image through HTTP and Docker, plus a small new real-backend smoke test.

## 1. Shared backend core (identical in options A and B)
Layout: `cmd/ludock` (serve, recover, and helper subcommands), `internal/{db,docker,compose,auth,identity,fingerprints,files,backups,tar,schedules,jobs,operations,console,monitoring,notifications,events,logging}`. HTTP/view packages sit on top, and they're the only part that differs between A and B.

| Area (TS module) | Bun API | Go choice |
|---|---|---|
| database.ts, migrations.ts | bun:sqlite | **modernc.org/sqlite** (pure Go, CGO_ENABLED=0, static). Keep WAL, foreign keys, busy_timeout. Plain SQL plus small hand-written repos; sqlc is optional. |
| docker-client/transport/stream, docker.ts, discovery, events, container-logs | FetchSession unix, Bun.connect hijack | **github.com/docker/docker/client** (official SDK) with stdcopy for multiplexed logs and the hijacked attach/exec. Drops most of docker-transport.ts. |
| compose.ts | Bun.spawn docker compose | os/exec `docker compose` at first (exact parity, same env handling). Library later is optional. |
| auth.ts, password.ts, routes/accounts.ts | CryptoHasher, Bun.password argon2id | golang.org/x/crypto/argon2. Keep the PHC `$argon2id$v=19$m,t,p` format so existing hashes verify. crypto/subtle for constant-time compares. |
| identity.ts, fingerprints.ts | CryptoHasher HMAC | crypto/hmac + sha256 with the same `hmac-sha256:` encoding. Port the #49 key-permission check (0600 / group / ACL warning / world → refuse) using golang.org/x/sys/unix plus xattr `system.posix_acl_access`. |
| request-security.ts, authorization.ts, server-socket-access.ts, approved-paths.ts | – | Pure-Go port. Origin/CSRF checks, grants, and path containment via filepath plus openat-style checks (os.Root in Go 1.24+). |
| file-storage.ts, files routes | Bun.file/write | os/io; preview cap 512 KiB via io.LimitReader. |
| backups.ts, backup-storage.ts, tar.ts | CryptoHasher, streaming | archive/tar + streaming sha256; compress/gzip (or klauspost/compress). |
| jobs, schedules, schedule-time, operations, operation-locks | Bun.sleep | goroutines, context, time.Timer; sync.Mutex per server lock. Schedule-time logic gets the most care (time zones). |
| helpers/* | `bun -e` | `ludock helper <name>` subcommands; the backend launches containers from its own image with that cmd. |
| network-console.ts, game-console(-runtime) | Bun.connect | net.Dial; RCON/telnet framing ported with unit tests. |
| json-lines.ts | Bun.JSONL | bufio.Scanner + encoding/json. |
| logger, application-logs | stdout | log/slog JSON handler. |
| static-files.ts | gzip variants | embed.FS plus precompressed .gz/.br lookup (option A only). |
| config | env | Same env vars (PORT, LUDOCK_DB_PATH, LUDOCK_BACKUP_ROOTS, LUDOCK_COMPOSE_ROOTS, …). |

HTTP: stdlib **net/http ServeMux** (Go 1.22+ method/path patterns), no framework. Websockets: **github.com/coder/websocket**. Middleware: request ID, no-store on /api, security headers, auth, rate limits.

### SQLite compatibility
Keeping the schema byte-compatible is **cheap**: the Go migrations package embeds the same CREATE statements and the user_version numbering, and modernc reads Bun-created files fine. The real cost is matching JSON column shapes and the fingerprint/HMAC encodings. Recommendation: keep compatibility (users keep their accounts, grants, schedules, backup index, audit history, and notification settings). If it breaks, they lose all of that, have to re-run first-run setup, and have to re-register servers and schedules. Backups on disk survive, but they need to be re-indexed. Add one container test that boots Go on a DB created by v0.5.0.

## 2. Option A: Go backend plus embedded React
- API-first. `/api/v1/*` and `/ws` stay exactly as they are, so React needs minimal changes.
- **Contract strategy:** keep **packages/shared Zod as the source of truth** at first, since the frontend already validates responses with it. Add a contract test: generate JSON Schema from Zod (`z.toJSONSchema`, Zod 4) and validate recorded Go responses in Go unit tests and the container suites. Later, optionally flip to OpenAPI generated from Go and TS types generated from that. Not needed for cutover.
- Build: Bun/Vite builds the frontend in CI/Docker, then `//go:embed dist` goes into the binary. Dev: `bun run dev` proxies to `go run ./cmd/ludock` (air optional, but it uses RAM; plain restart is fine).
- Playwright: the existing mocked journeys carry over unchanged (served by the Go binary instead of the Bun preview). Add 1–2 real-backend smoke journeys (login → dashboard → server detail) against the Go binary in the existing Browser job.
- Effort: about 10–12 PRs, mostly backend port. Lowest risk.

## 3. Option B: all-Go, server-rendered
Page-first: handlers render HTML. JSON exists only where JS needs it (the console socket). The backend core in §1 is shared. **What diverges:** there's no `/api/v1` contract layer (the routes/* handlers become page handlers plus small action endpoints), packages/shared and Zod go away (validation moves into Go form decoding), and auth becomes a cookie session plus CSRF token on forms. That's close to today, but there are no API tokens for the SPA. Request-race protections change shape: there's no client-side state to go stale, so most of the browser-test race guards (stale refresh, late access check) become server-side checks on every fragment request.

Libraries: **templ** (type-checked, compiles to Go, good for agents, catches errors at build time) over html/template. **htmx 2** (vendored, embedded) for partial swaps, plus its SSE extension. **Alpine.js** only for small local UI state (dialogs, toggles); vanilla JS also works. **xterm.js stays** for the console (one small embedded JS bundle, no React or build system beyond copying files). CSS: keep the existing stylesheet.

| Screen | How it'd work | Difficulty |
|---|---|---|
| Login / setup / account / users / grants | Plain forms + htmx post, server-side validation | Easy |
| Dashboard / server cards / live status / needs-attention | SSE stream from internal/events; htmx sse-swap replaces cards | Medium |
| Server detail (917-line page: overview, availability, update, backups, schedules panels) | Tabs as htmx-loaded fragments; operations status via SSE | Medium-hard (largest page) |
| Console (605 lines, xterm) | Keep xterm.js plus a tiny vanilla-JS websocket client; server-side identical | Medium |
| Container/application logs streaming | SSE appending lines; cap DOM rows | Medium |
| Schedule editors (one-time, recurring, announcements, time zones) | Server-rendered form with htmx live preview of next run; Alpine for repeated rows | Medium-hard |
| Files browser (956 lines) + preview (512 KiB cap) + actions | Directory list fragments, preview as server-rendered `<pre>` with cap, upload via multipart form | Medium-hard |
| Backups / restore confirm | Forms + SSE progress | Medium |
| Settings, notifications, deliveries, diagnostics, history | Forms/tables, pagination via htmx | Easy |

Playwright under B: `page.route` API mocks don't apply because pages come from the server. Journeys have to be rewritten to run against the real Go binary with a fake Docker (a test Docker double behind an interface, or the real daemon in CI's container jobs). Selectors and flows (role/label based) mostly carry over, but each spec has to be reworked. Keep about 8–10 journeys and drop the rest. The 6 race-condition tests become Go handler tests.

Effort: everything in A plus a full UI rewrite (~8.6k TSX lines → templ + bits of JS), roughly 18–22 PRs. Risks: UI regressions and lost polish on the live screens, larger rewrite surface at once, and htmx/SSE edge cases (reconnects, auth expiry mid-stream).

## 4. Recommendation
**Do A first, keep B open.** The core in §1 is ~70% of the work and identical for both, so build that with a thin JSON API. Ship it with React (fastest path to a shippable Go release). Then decide on B with real experience, and convert page by page if wanted (Go can serve templ pages and React side by side during the transition). If Logan already knows he wants B, go straight to B after PR 5 below, but expect about twice the UI effort and a longer gap before the next release.

## 5. PR sequence (main stays shippable; Bun stays the release artifact until cutover)
Go lives in `go/` (or the root go.mod) alongside packages/. CI adds one **Go job** (`go vet`, `go test ./...`, staticcheck, about 1–2 min). No new slow jobs.

Shared (A and B):
1. **Scaffold**: go.mod, cmd/ludock, config, slog, health endpoint, Go CI job. (S, ~400 lines)
2. **DB layer**: modernc sqlite, schema-compatible migrations, repos; test opening a v0.5.0 DB fixture. (M, ~1.2k)
3. **Auth & security core**: argon2id compat, sessions, rate limits, request security, authorization/grants, identity key + #49 permission check, fingerprints. Focused unit tests. (L, ~2k)
4. **Docker layer**: official SDK client, discovery, events, logs/stdcopy, attach/exec. (L, ~1.5k)
5. **Helpers as subcommands + files/tar/backups core**, preview limits, path containment. (L, ~2.5k)
6. **Schedules/jobs/operations/locks/notifications/monitoring/history.** (L, ~2.5k)
7. **Console runtimes** (docker attach, network RCON/telnet) + compose via exec. (M, ~1.5k)

Option A continues:
8A. **HTTP API + websocket** at full `/api/v1` parity, Zod JSON-Schema contract tests. (L, ~2.5k)
9A. **Embed frontend + new Dockerfile** (bun build stage → go build stage → small alpine/distroless runtime with docker-cli-compose if exec is kept). Container suites pointed at the Go image behind a flag; Browser job serves via Go plus 1–2 real-backend smoke tests. (M)
10A. **Cutover**: Dockerfile default → Go, delete packages/backend and Bun backend scripts, update docs/AGENTS.md, release-please (switch `release-type` to `simple` or keep `node` for the frontend package.json; version file stays). Release as 0.6.0 (or 1.0). (M, mostly deletions)
11A. Cleanup: port container harness scripts from Bun .mjs to Go tests where cheap; optional OpenAPI. (S–M)

Option B continues (instead of 8A–11A):
8B. Layout, templ setup, embedded assets (htmx, Alpine, xterm, CSS), auth pages, cookie/CSRF. (M)
9B. Dashboard + live status via SSE. (M)
10B. Server detail panels. (L)
11B. Console + logs streaming. (M)
12B. Schedules editors. (L)
13B. Files + preview + backups/restore. (L)
14B. Users/grants/settings/notifications/diagnostics/history. (M)
15B. Rewrite the Playwright journeys against the real binary + fake Docker; Dockerfile without Bun at all. (L)
16B. Cutover: delete packages/*, Bun, Zod, release-please → `simple`. (M)
(Until 16B, the Bun app remains what ships. B pages could be developed behind `/next/` routes.)

## 6. CI / Docker / release
- Dockerfile: `golang:1.2x-alpine` build with CGO_ENABLED=0 and `-trimpath -ldflags "-s -w -X version"`, cross-compiled per arch via GOARCH (no QEMU). Runtime: alpine plus docker-cli(+compose) if compose stays exec. Otherwise distroless/static (~20–30 MB). Keep `read_only`, cap_drop, and the HEALTHCHECK (add a `ludock healthcheck` subcommand so wget isn't needed).
- Compose file unchanged (service, port 3000, /data, /backups, socket mount, env).
- Release: release.yaml builds the same multi-arch image. release-please needs a version source (keep package.json in A; `version.txt` + `simple` in B).
- Govulncheck added to security.yaml; CodeQL adds Go.

## 7. Testing (Logan's prefs)
- Go unit tests only for tricky or security logic: password hashes, session/rate limits, grants, path containment, preview cap, key permissions, fingerprints, schedule time math, tar streaming, RCON framing, operation locks, and the race cases (stale refresh/late access → server checks).
- Existing container suites (runtime/compose/files × amd64/arm64) are the backend parity gate. Repoint them at the Go image without rewriting.
- Playwright: A keeps the mocked suite plus 1–2 real smoke tests. B uses ~8–10 real-backend journeys.
- No local full builds: targeted `go test ./internal/x` while working, CI for the rest. Go builds stay within the RAM budget (set GOMAXPROCS/`-p 2` locally; check for other builds first).

## 8. Open decisions for Logan
1. **A vs B** (recommendation: A first, B later page-by-page).
2. **Keep DB compatibility?** (recommended yes; cheap.)
3. **Compose:** keep shelling out to `docker compose` (bigger image, exact behavior) or embed the compose library (smaller, riskier). Recommendation: exec first.
4. **Release number** for the Go cutover: 0.6.0 vs 1.0.0.
5. Repo layout: Go module at root vs `go/` subdir (recommendation: root `cmd/` + `internal/`, frontend stays in packages/frontend for A).

## 9. Biggest risks
- Behavior parity in security logic (auth, grants, path containment, key checks): mitigated by porting the tests' cases first.
- Helper-container protocol (`bun -e` → subcommands) and multiplexed Docker streams/console hijack.
- Schedule/time-zone semantics and the JSON shapes the frontend expects (A). Contract tests mitigate.
- B only: UI regressions on the live screens, the Playwright rewrite, and a long time before the next release.
- Long-lived parallel codebases: keep fixes to the Bun app minimal during migration.
