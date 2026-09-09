# Testing Ludock v2

Tests describe expected behavior. Their presence is not evidence that a release,
Docker architecture, or live-game integration has passed them. Record the image
version, platform, fixture, and result during release validation.

## Automated checks

Use Node.js 24 and pnpm 10:

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm check
docker build -t ludock:test .
```

`pnpm check` builds shared runtime contracts, checks types and lint, runs
development-runner, backend unit/HTTP, and frontend component tests, then builds
production artifacts. Compile-only checks cover logical versus Docker identity
boundaries and required route capabilities/response contracts.
For focused work:

```bash
pnpm --filter @ludock/frontend test
pnpm --filter @ludock/backend test
```

Backend suites include discovery, identity, grants, migrations, protocol
adapters, filesystem boundaries, archive/restore validation, operation locks and
recovery, schedule authority/DST handling, monitoring, and notification retries.
Frontend tests cover independent action grants, role ceilings, runtime response
validation, confirmation/draft state, polling, and extracted server panels.
Deferred-request regressions cover password reset and session revocation during
hashing, authority changes during Docker/helper preparation, Unicode redaction
across stream chunks, deterministic Compose environment snapshots, and shared
bind-root overlap. Frontend cases cover stale session reads, failed editor loads,
overlapping mutations, preserved new drafts, and honest history/error states.
Development tests cover isolated state/cookies, shared database locks, port
collisions, and backend instance checks. Fixture tests do not replace real
Docker, Compose, or game-world validation.

## Browser regression checks

The browser suite serves a production frontend build and intercepts API and
WebSocket requests with per-test fixtures validated by the shared contracts. It
does not start a backend, connect to Docker, or read application storage. The
dedicated preview configuration has no backend proxy, and unexpected API or
external requests fail the test.

```bash
pnpm --filter @ludock/shared build
pnpm --filter @ludock/frontend build
pnpm --filter @ludock/frontend exec playwright install chromium
pnpm --filter @ludock/frontend test:e2e
```

The separate **Browser** CI job installs Chromium with its Linux dependencies
and runs both desktop and touch-mobile projects. Checks cover 320px/390px
layouts, state/port column alignment, touch targets, filters, independent grants,
native dialog focus and confirmation, More actions, detail-tab keyboard use,
and navigation continuity. File cases cover contextual create/delete dialogs,
rename failure recovery, cancellation of obsolete listings, and upload
cancellation. Live-state and console cases cover stale controls, denied access,
fresh snapshots on reconnect, manual retry after repeated disconnects, separate
command drafts, and commands never being resent automatically.
These browser fixtures verify frontend behavior;
backend authorization and actual Docker mutations remain covered separately.

For focused work, append a spec name or `--project=desktop` to `test:e2e`.
`LUDOCK_E2E_PORT` changes the isolated preview port (default `4179`). An occupied
port fails instead of reusing another process. Set
`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` to an installed Chrome/Chromium executable
when the Playwright browser download is unavailable; CI uses Playwright's bundled
Chromium. Failed checks save screenshots and traces in
`packages/frontend/test-results/`, with an HTML report in
`packages/frontend/playwright-report/`. CI retains these fixture-only artifacts
for seven days.

## Docker and architecture validation

Validate both `linux/amd64` and `linux/arm64` on their corresponding CI runners
or a suitable Buildx/emulation setup. A successful build on one architecture is
insufficient. Release/nightly images must contain both runtime platforms; check
the actual published tag with `docker buildx imagetools inspect` before release
sign-off.

For each platform:

- Build the production image and initialize fresh SQLite storage.
- Check `/api/v1/health`, the setup page, and static assets. Without a socket,
  health reports degraded; with the test daemon it reports healthy.
- Run the bundled `node`, `docker --version`, and `docker compose version`.
- Resolve and run the configured `node:24-alpine` helper on that platform; verify
  file access, stopped-server backup, and restore against disposable volumes.
- Exercise Compose source validation inside the Linux Ludock runtime. Host
  macOS/Windows unit execution cannot establish descriptor-path compatibility.
- Verify no build-host-specific native artifact entered the target runtime.
- Run the repository's image vulnerability and workflow checks.

Docker Desktop, rootless Docker, custom socket paths, and external managers
need separate documented acceptance results. The image starting does not prove
that a given host's Compose bind paths and mount permissions work.

## Docker integration harnesses

After installing dependencies and building `ludock:test`, run:

```bash
docker pull node:24-alpine
node scripts/test-linux.mjs
python3 scripts/test-compose.py
node scripts/test-files.mjs
node scripts/test-backups.mjs
```

The Node scripts require Node.js 24 and the installed workspace TypeScript
package. The Linux suite runs the backend tests against the production modules,
including descriptor-based path tests skipped on macOS. All scripts use `LUDOCK_TEST_IMAGE` when set; otherwise they run the
local `ludock:test` image. They create uniquely named fixture containers and
volumes and remove their fixtures afterward. The Compose harness checks current
images, forced recreation of running/stopped services, retained logical identity,
untouched dependencies, source drift, and literal environment values. The backup
harness executes archive/data-operation tests inside the Linux runtime against
disposable volumes and a separate mounted backup destination. The file harness
checks scoped helper CRUD, mount-source verification, and directory replacement
races against disposable data.

These harnesses require a reachable Docker daemon, a usable socket mount, and
host bind paths visible at their declared absolute locations. The fixture game
containers require the default helper image to be present; a Buildx build does
not necessarily leave its base image tagged in the daemon's image store. The
scripts mount `/var/run/docker.sock`; custom socket and remote-daemon setups
require adapting those fixture mounts. Docker Desktop can
canonicalize `/Users` and temporary paths through VM-specific aliases. Strict
mount/symlink validation may reject those layouts; use verified native Linux
paths and named game-data volumes for full data-operation acceptance. Do not
weaken root validation to make an unsupported layout pass.

## Disposable-server acceptance

Use a non-production Docker host, dedicated game data, a separate backup mount,
and fresh `/data`. Never exercise restore or forced recreation on the only copy
of a real world. Keep the owning Compose files mounted read-only at identical
absolute paths on the Docker host and inside Ludock.

### Fresh setup and authentication

- Fresh v2 storage opens first-administrator setup; v1 or unrelated databases are
  rejected without changing those files or any game data.
- Setup expires after five minutes and reopens on restart while no account exists.
- Sign-in requires a valid username and 15–128 character password; repeated
  failures are throttled and do not disclose whether a user exists.
- Cookies survive normal restart, are HttpOnly, and are Secure behind HTTPS.
- Sign-out and password changes revoke the correct sessions. Disabling an
  account or revoking access closes its streams and blocks further commands.
- A password reset or account change during password verification cannot revive
  old credentials or a stale role. Revoking an administrator's session during
  password hashing prevents its pending account mutation.
- The last enabled administrator cannot be disabled, deleted, or demoted.

### Discovery, identity, and server grants

- Unlabeled recognized images and private mirrors appear for administrators.
- False/invalid enable labels exclude; invalid labels produce diagnostics.
- True explicitly includes unknown images and Compose one-offs; other one-offs
  and unknown unlabeled containers remain excluded.
- New operators/viewers see no servers until assigned. Assign one friend
  `server.view`, `server.start`, and `server.stop` on two selected servers.
- That friend sees only those two and cannot restart, read logs/files, send
  commands, create backups, manage schedules, update, restore, or open a shell
  through the UI, direct HTTP calls, or WebSocket upgrades.
- Add console to one assigned server; verify it grants neither console on the
  second server nor log streams on either. Add file read independently from write.
- A viewer cannot mutate even if malformed stored grants contain write actions.
- Ordinary recreation preserves the logical UUID and grants. Missing servers
  preserve history. Duplicate identities fail closed. Standalone rename creates
  a new UUID. Changed game/mount bindings require review and suspend old grants
  and schedules; reviewing a binding does not auto-enable changed-data schedules.
- API resources, operation identifiers, schedules, and events do not disclose
  another user's inaccessible servers.

### Lifecycle, console, and files

- Independent start/stop/restart controls match grants and actual running state.
- Conflicting operations on a server, Compose project, shared named volume, or
  overlapping bind roots are blocked, including conflicting file mutations.
- Logs show up to 500 timestamped lines and follow output; pause is bounded.
- Exercise each configured adapter against a real disposable game or protocol
  fixture: Minecraft `rcon-cli`, Source RCON, Rust WebRCON, 7DTD Telnet, and
  Terraria process stdin. Authentication failures never reveal credentials.
- Only administrators can use the interactive container shell. Saved automation
  does not accept shell commands.
- Files work while running/stopped using isolated helpers. Reject unsafe roots,
  read-only mounts, system paths, socket paths, symlink chains, traversal, and
  unsafe nested mounts. File labels cannot expose the writable container layer.
- Create/upload/rename/delete/download remain bounded to their roots, audited,
  and subject to separate read/write grants. Parent-directory replacement races
  cannot redirect operations to outside data.
- Replacing the original game during helper startup rejects the request and
  removes the helper. Downloads keep conflicting-operation locks until helper
  cleanup completes, including when the client disconnects.
- Revoke access during helper/console preparation and verify no subsequent
  command or file operation is dispatched. Split multibyte text and credentials
  across stream chunks and verify complete redaction and readable output.
- Large downloads respect consumer backpressure. Verify long Unicode archive
  paths and files larger than 8 GiB without truncated names or size metadata.

### Backups and restore failure handling

- Backups are unavailable without administrator configuration and a verified,
  separate mounted destination inside `LUDOCK_BACKUP_ROOTS`.
- A running server stops before copying and restarts after success/recoverable
  failure. A stopped server remains stopped. Failed/forced stops prevent a
  completed backup; known running shared writers prevent copying.
- External restart/replacement during copying invalidates the operation.
- Shared-writer checks include root bind mounts and trailing-slash paths.
  Revoking authority during preparation blocks new backup/restore/update steps
  while still allowing rollback, helper cleanup, and safe state restoration.
- Exercise capacity/reserve failure, temporary output cleanup, checksum failure,
  retention, and protected restore-target retention using disposable data.
- A restore requires administrator authority and exact typed confirmation. It
  validates its binding and archive before changing game data, creates a safety
  backup, and stays stopped between backup and replacement.
- Reject traversal, duplicate/oversized entries, symlinks, hard links, special
  files, missing roots, and mismatched archive hashes.
- Inject failure before staging, while moving old data, during each replacement
  root, and during cleanup. Restart Ludock at those phases. Verify journal-based
  rollback/cleanup and the actual bytes in every root; a multi-root restore is
  not assumed atomic.
- Uncertain recovery keeps the server stopped with an actionable operation
  result. Do not delete recovery staging to make a test pass.

### Schedules, availability, and notifications

- Schedules require both `schedules.manage` and the independently granted action.
  Operators see/manage their own schedules only.
- Revoking an action, disabling/deleting an owner, or deleting a queued schedule
  prevents execution. Validated ordinary recreation continues schedules;
  materially changed data bindings suspend them until recreated after review.
- Verify explicit time zones, weekday selection, repeated fall-back time running
  once, and skipped spring-forward/missed times. No catch-up destructive runs.
- Monitoring is off by default. After enabling it, emit one outage after grace
  and one recovery. Docker health `starting` is not ready.
- Ludock stops, backup/restore/update operations, post-operation grace, and
  maintenance suppress alerts. An external stop after normal operation alerts.
- Docker unavailability reports an inability to verify state, retaining identities.
- Mock Discord delivery for routine tests. Validate write-only settings, no
  mentions, deduplication, bounded backoff/five-attempt cutoff, and secret-free
  errors. Use a dedicated test webhook only with its owner's authorization.

### Compose updates and owning managers

- Register ordered Compose files and explicit environment inputs inside approved
  roots. Reject outside-root paths, symlink traversal/races, transitive unsafe
  reads, malformed names, unsupported source features, and secret-bearing errors.
- Re-register after source changes. Verify selected project/service identity,
  registered source fingerprint, configured image, and current binding at execution.
- Recognized images show **Update server**; unknown images show **Update image**.
  Inaccessible/unregistered/standalone projects explain use of the owning manager.
- A running service returns running/healthy; a stopped service remains stopped.
- Current images report **Configured image is current** without asserting the
  game version is fresh. **Recreate anyway** requires fresh confirmation and the
  same backup/eligibility rules. A skipped backup requires the exact server name.
- Pre-update backups do not restart the server between copying and recreation.
- Pull failure leaves the original container unchanged; backup failure aborts
  before recreation; Compose failure reports actual service state and recovery
  guidance without cloning old configuration or swapping image digests.
- Dependencies are not recreated, builds/additional pulls/volume renewal are
  disabled, and image+build services use the pullable configured image.
- Confirm Portainer/Dockge/external Compose recreation rediscovery, retained
  declared named volumes/binds, and denial of unregistered manager-owned updates.

### Frontend and operational visibility

- Server lists use readable rows, compact controls, useful empty/error states,
  and keyboard-accessible actions; no permission is inferred from role alone.
- State and port columns align with their headings across running and stopped
  rows. Check widths around the content-width breakpoint, including wrapped
  actions and names; narrow layouts retain labeled ports without page overflow.
- Search and exact-state filters combine, report the visible count, and provide
  a clear-filters action when no servers match. A refresh does not reset filters.
- Returning from Files or Console restores the selected detail tab; returning to
  Servers restores the list filters. Sign-out clears these in-memory choices.
  A late operation response from a page that was left cannot change the new
  page's tab.
- Disconnected lists label retained data as stale and disable lifecycle actions.
  Every live connection refreshes the authorized snapshot before controls resume;
  denied access hides cached rows and requires revalidation.
- More actions support Tab, Escape, and outside dismissal. Stop and Restart name
  the server in a modal confirmation; Cancel has initial focus, Tab cannot enter
  the page behind the dialog, and dismissal restores focus. No request occurs before confirmation.
  A restart-only grant remains usable without a stop grant.
- Detail tabs have one tab stop, support Arrow keys/Home/End, and reference their
  panels. Switching tabs preserves drafts; action-driven changes move focus into
  the new panel when the triggering control disappears.
- Console modes support keyboard navigation and preserve separate command drafts
  through connection interruptions. Sending requires fresh access; failed sends
  keep their draft, and reconnecting never resends a command automatically.
- Changing a file root or folder hides old entries and ignores obsolete reads.
  File dialogs name the server and folder, retain drafts after ordinary errors,
  and require a new confirmation after reconciling an uncertain write result.
  Partial upload failures remain visible after refresh; canceling stops the
  remaining batch and explains that data already written is not rolled back.
- At 320px and 390px widths, navigation can scroll, server actions wrap, forms
  fit, and data tables remain usable. Console input and file controls do not
  overflow. Touch actions remain at least 44px tall and text inputs avoid zoom.
- The four-tile Ludock mark appears in navigation and authentication screens.
  Check the solid favicon at 16px on light and dark browser chrome. Primary
  controls, placeholder text, focus rings, and permission-limited views stay clear.
- Grant presets show the actual selected capabilities. Backup downtime, restore
  data replacement, force recreation, and the meaning of image-current results
  are explicit in the relevant flow.
- Failed settings or grant reads disable saving until a successful retry. A
  pending save cannot erase a newer draft. Changed roles, bindings, and operations
  invalidate obsolete action confirmations.
- Late session reads cannot restore a signed-out account. Failed sign-out shows
  an unconfirmed session state. Password-change success remains visible if the
  subsequent session refresh fails; failed history/diagnostic reads never claim
  an empty or healthy result.
- Activity reports actual persisted phases/outcomes. Audit and structured logs
  are administrator-only, redacted, and preserve actor/server attribution.

## Product boundaries

No provisioning, Compose editing, arbitrary container reconstruction, live
backups, scheduled updates, game-specific readiness guarantees, or general
operation-cancel API is included. A backup protects game data; it is not an image
or Compose rollback. One Ludock backend manages one Docker host; external
managers are not covered by Ludock's in-process locks.
