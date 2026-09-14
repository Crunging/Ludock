# Development and testing reference

Use this reference when changing the corresponding feature. Choose scenarios
that exercise the affected behavior; this is not a checklist to complete for
every change. Start with the setup and check commands below.
These scenarios describe expectations, not recorded proof of passing tests.
Record the image digest, platform, fixture, and result for integration validation.

## Development

Use the latest stable Bun 1 release (`package.json` declares the minimum):

```bash
bun install --frozen-lockfile
bun run dev
```

The runner prints the frontend/API URLs and database path. Each checkout gets
separate ports, cookies, and persistent state under `~/.local/state/ludock/dev`.
Both servers bind to loopback, and Docker is disconnected by default. Set
`DOCKER_SOCKET` only for a dedicated test daemon; run one backend per Docker host
and use disposable game data.

Use `bun run dev --print-config` to inspect settings without starting services.
Overrides: `LUDOCK_DEV_HOME` (state directory), `LUDOCK_DEV_PORT` (frontend),
`LUDOCK_DEV_API_PORT` (backend; `PORT` also works), and `LUDOCK_DB_PATH` (database,
including `:memory:`). Never point development at production or unrelated data.

Stop with Ctrl+C; active operations may need time to finish. After a crash,
inspect the PID in `dev.lock` in the printed state directory or
`<database>.dev.lock` beside an overridden database. Remove a stale lock only
after verifying that its runner and backend have ended. Keep the database intact.

## Checks

```bash
bun run check                              # Types, lint, tests, and build
bun run --filter @ludock/backend test      # Focused backend suite
bun run --filter @ludock/frontend test     # Focused frontend suite
```

For browser changes, build and run the desktop/mobile fixtures:

```bash
bun run --filter @ludock/shared build
bun run --filter @ludock/frontend build
(cd packages/frontend && bun x --bun playwright install chromium)
bun run --filter @ludock/frontend test:e2e
```

Browser tests use mocked APIs and no Docker connection. Append a spec name or
`--project=desktop` for focused checks. `LUDOCK_E2E_PORT` overrides port 4179;
`PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` selects an existing browser. Failure traces
and reports are under `packages/frontend/test-results` and `playwright-report`.
Release-workflow tests also require Git, Bash, and `jq`.

For runtime or Docker integration changes, use a dedicated test daemon:

```bash
docker build --pull -t ludock:test .
docker pull "$(bun -p "(await import('./packages/backend/src/runtime-images.ts')).DEFAULT_HELPER_IMAGE")"
bun scripts/test-linux.mjs
bun scripts/test-compose.mjs
bun scripts/test-files.mjs
bun scripts/test-backups.mjs
```

The harnesses create and clean up their own fixtures. `LUDOCK_TEST_IMAGE`
overrides `ludock:test`. Socket-enabled harnesses mount `/var/run/docker.sock`
and require host bind paths visible at identical absolute paths; Docker Desktop
aliases and remote daemons may need a native Linux test host. Validate affected
runtime/helper behavior on both `linux/amd64` and `linux/arm64`. Mocked tests do
not establish live-game compatibility. Use `docker compose config` for Compose
edits; documentation-only edits need link and diff checks.

## Docker and architecture validation

Validate both `linux/amd64` and `linux/arm64` on their corresponding CI runners
or a suitable Buildx/emulation setup. A successful build on one architecture is
insufficient. Published images must contain both runtime platforms; check the
actual tag with `docker buildx imagetools inspect` before distribution.

For each platform:

- Build the production image and initialize fresh SQLite storage.
- Check `/api/v1/health`, the setup page, and static assets. Without a socket,
  health reports degraded; with the test daemon it reports healthy.
- Run the bundled `bun --version`, `docker --version`, and `docker compose version`.
- Resolve and run the digest-pinned `oven/bun:1-alpine` helper on that platform;
  verify file access, stopped-server backup, and restore against disposable volumes.
- Exercise Compose source validation inside the Linux Ludock runtime. Host
  macOS/Windows unit execution cannot establish descriptor-path compatibility.
- Verify no build-host-specific native artifact entered the target runtime.
- Run the repository's image vulnerability and workflow checks.

Docker Desktop, rootless Docker, custom socket paths, and external managers
need separate documented acceptance results. The image starting does not prove
that a given host's Compose bind paths and mount permissions work.

## Disposable-server acceptance

Use a non-production Docker host, dedicated game data, a separate backup mount,
and fresh `/data`. Never exercise restore or forced recreation on the only copy
of a real world. Keep the owning Compose files mounted read-only at identical
absolute paths on the Docker host and inside Ludock.

### Initial setup and authentication

- Empty application storage opens first-administrator setup. It requires the
  one-time code printed to the local container console (or the configured
  `LUDOCK_SETUP_CODE`) before account validation or password hashing. The code
  never appears in the application-log API/UI. Unrelated databases and
  unsupported schemas are rejected without changing those files or game data.
- Setup expires after five minutes and reopens with a newly generated code on
  restart while no account exists. A completed setup cannot be reopened with an
  old or configured code.
- Sign-in requires a valid username and 15–128 character password; repeated
  failures are throttled by source and credential without letting one source
  lock the account for everyone. Concurrent memory-hard password work is
  bounded and does not disclose whether a user exists.
- Cookies survive normal restart, are HttpOnly, and are Secure behind HTTPS.
- Sign-out and password changes revoke the correct sessions. Disabling an
  account or revoking access closes its streams and blocks further commands.
- A password reset or account change during password verification cannot revive
  old credentials or a stale role. Revoking an administrator's session during
  password hashing prevents its pending account mutation.
- The last enabled administrator cannot be disabled, deleted, or demoted.
- Configure uploads with `MAX_UPLOAD_SIZE=500 MB` or `1.5 GiB`; verify the
  configured limit and a readable oversized-upload error. The byte-only
  `MAX_UPLOAD_BYTES` setting is also accepted; malformed sizes fail startup clearly.
  The example Compose file uses `LUDOCK_PORT` for the published browser port.

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
- Incomplete, oversized, cancelled, or revoked uploads preserve an existing
  destination and do not leave a partial new file. Complete replacements retain
  ordinary ownership and permissions; cleanup removes only that upload's temporary
  sibling and never follows a substituted parent or destination symlink.

### Backups and restore failure handling

- Backups are unavailable without administrator configuration and a verified,
  separate mounted destination inside `LUDOCK_BACKUP_ROOTS`.
- A fresh example Compose deployment creates separate application and backup
  volumes. Settings offers `/backups`; selecting it does not save automatically.
  Saving the limits validates the mount. An explicitly empty backup-root setting
  still disables destination configuration.
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

- Discover ordered Compose files and environment inputs automatically inside approved
  roots, including the default `.env` when no CLI environment files are recorded.
  Reject outside-root paths, symlink traversal/races, transitive unsafe reads,
  malformed names, unsupported source features, and secret-bearing errors.
- Pick up source edits without registration. Verify selected project/service identity,
  confirmed source fingerprint, configured image, and current binding at execution.
  Abort on source changes after confirmation or during execution. Repeat updates
  after recreation and restart to verify original source paths survive snapshot cleanup.
- Recognized images show **Update server**; unknown images show **Update image**.
  Inaccessible/missing-source/standalone projects explain use of the owning manager.
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
  declared named volumes/binds, and automatic updates without project registration.

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
- A failed deferred page download shows recovery controls while navigation and
  remembered filters remain available. Reloading the document retries the failed
  download; the fallback does not expose raw errors or module paths.
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
  Filename filtering applies only to the loaded folder, retains its query on
  refresh, and clears it on folder/root navigation while preserving sort order.
  Folders remain first for every sort direction; missing metadata has stable
  placement and matching counts never describe stale entries.
  File dialogs name the server and folder, retain drafts after ordinary errors,
  and require a new confirmation after reconciling an uncertain write result.
  Partial upload failures remain visible after refresh; canceling stops the
  remaining batch and explains that data already written is not rolled back.
- Ludock log search combines with exact severity and component filters over the
  current buffer. Selected components survive buffer rollover and restarts;
  denied access clears cached entries and component names. Disabling Follow
  latest preserves scroll position while polling continues, and Pause stops
  polling independently. Resuming never replaces newer filter input.
- Activity filters survive refreshes and tab changes without altering operation
  locks. A notice keeps hidden active work visible and reachable, View progress
  reveals it, and schedule-linked operation details remain outside the filters.
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
- Schedule edits preserve drafts across tabs and refreshes. Pause/resume changes
  only the enabled state, and the schedule's state remains separate from its last
  result. Next-run previews use the chosen timezone, skip daylight-saving gaps,
  and do not repeat consumed fall-back slots. A stale edit requires reviewing the
  latest revision; queued work from an edited or paused revision cannot dispatch.
- Creating a paused schedule never queues a due run. Last-run status follows the
  persisted operation through completion or interruption, while a later skipped
  attempt supersedes an older result. View activity opens the selected operation
  even outside the recent list, with scoped access and obsolete-response guards.
  Unavailable previews explain disabled owners, missing grants, and changed
  bindings without exposing internal diagnostics.
- Backup storage fields allow clearing and typing decimal GiB values without
  changing them mid-edit. Saving untouched settings preserves exact stored sizes;
  a zero free-space reserve remains valid. File and backup sizes use consistent
  units.
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
