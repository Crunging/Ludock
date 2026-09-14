# Architecture and feature boundaries

Ludock is a TypeScript modular monolith: a Bun backend, a React frontend,
and shared Zod contracts. Ordinary functions and explicit dependencies connect
feature modules. Docker, SQLite, filesystem helpers, and Compose are the external
boundaries.

## Runtime and persistence

Bun supplies the runtime, package installation, builds, and tests. `.bun-version`
selects the major; `package.json` declares the minimum and `bun.lock` records
resolved dependencies. Shared contracts are compiled for both applications.
The backend bundles `index.ts` and the account-recovery entry point for Bun;
the frontend bundles browser assets from `index.html`.

The production image includes Bun, Docker CLI, Compose, and production
dependencies on Alpine. Builds keep build-platform tools separate from target
runtime artifacts and publish one image index for AMD64 and ARM64. Helper
containers use the immutable Bun Alpine image in
[`runtime-images.ts`](../packages/backend/src/runtime-images.ts). A validator's
host-root mount remains privileged even when read-only and network-disabled;
helper overrides must be trusted and digest-pinned.

`database.ts` uses `bun:sqlite`. It inspects nonempty databases read-only before
enabling WAL or applying supported migrations, rejecting unrelated or unsupported
storage without writes. The supported schema starts at version 2.

Identity and Compose-source fingerprints use domain-separated HMACs with an
installation key outside SQLite. A database-only disclosure therefore cannot
verify guesses of low-entropy game credentials or environment values. The
mode-0600 key is published in a mode-0700 `.identity-key` directory beside the
canonical database path. The key and directory are synced before dependent
schema changes commit.
Missing, unsafe, or mismatched keys fail closed. Database backups must include
that directory; see [application backup](./OPERATIONS.md#application-backup-logs-and-account-recovery).

New passwords use Argon2id. Bounded legacy scrypt verification upgrades a hash
only after rechecking the enabled account and unchanged password. Queued API-token
work stores a separate HMAC of the credential generation, so rotation rejects
previously queued privileged steps while still allowing recovery cleanup.

Filesystem confinement uses pinned descriptors and Linux `/proc/self/fd` checks.
Dependencies at these boundaries supply specific controls:

- `yaml` rejects duplicate keys and bounds alias expansion in Compose inputs.
- `tar-stream` exposes archive-entry streams for validating paths, types,
  ownership, modes, and byte limits with backpressure.
- Dockerode handles Docker exec/attach upgrades and multiplexed streams.

These controls must survive dependency or runtime changes. See the
[maintenance procedure](./TESTING.md#dependency-and-image-updates).

## Shared contracts

`packages/shared/src` groups Zod schemas and inferred types by feature. Backend
routes validate inputs and pass explicit public projections to `respond`, which
checks response types and validates values before serialization. Malformed
producer responses become generic server errors. Schema validation does not
replace credential redaction or permission filtering.

Frontend `apiJson` and `apiResponse` validate responses without displaying raw
unexpected values. Event and console WebSocket messages are validated too.
`LogicalServerId` and `DockerContainerId` are distinct branded types: public server
routes use logical UUIDs, and Docker wrappers require inspected or revalidated
physical bindings. Compile-only tests protect those boundaries and route-policy
signatures.

## Backend routes and domain logic

`index.ts` owns the Bun listener and coordinated shutdown. `app.ts` composes
routes, authentication, request limits, security headers, and static assets.
Handlers in `packages/backend/src/routes` receive typed request contexts:

| Module | HTTP responsibility |
| --- | --- |
| `accounts.ts` | Setup, sessions, passwords, and account administration |
| `servers.ts` | Server lists/details and direct lifecycle actions |
| `files.ts` | Browsing, transfers, and file mutations |
| `access.ts` | Grants and binding review |
| `backups.ts` | Backup settings, archives, creation, and restore requests |
| `compose.ts` | Update capability and update requests |
| `schedules.ts` | Schedule management and next-run previews |
| `status.ts` | Operation progress and availability configuration |
| `settings.ts` | Deployment guidance, notifications, diagnostics, and integrations |

`identity.ts` and `servers.ts` reconcile logical history with Docker observations;
`authorization.ts` owns capability and role ceilings. Routes call these policies
instead of duplicating them.

Direct lifecycle and file handlers use `serverAction(capability, handler)`.
It resolves the authorized binding and retains resource locks until both the
response and cleanup finish, including cancelled transfers. Its `assertAccess`
callback rechecks sessions, grants, and bindings after asynchronous preparation,
immediately before dispatch. Lifecycle actions also check the final inspected
observation. Console transports apply the same rule before sending credentials
or commands.

`websocket-server.ts` owns upgrades, session revalidation, and shutdown. Socket
channels bound buffering and preserve cleanup lifetimes; adapters own protocol
behavior. RCON and Telnet handle deadlines and partial writes, closing late
connections after cancellation. The CLI exits only after application work and
SQLite drain; the imported server's `shutdown()` does not exit the process.

Minecraft Docker-exec commands remain literal argument-array values. An
in-container watchdog bounds execution, output is capped, and revocation or
disconnection requests cancellation while retaining the lock through cleanup.
A lost `exec.start` response cannot prove Docker rejected the mutation, so an
HTTP timeout alone cannot safely release its lock. Exact transport limits live
in [`game-console-runtime.ts`](../packages/backend/src/game-console-runtime.ts).

Uploads use an exclusive temporary sibling under a pinned destination directory.
Replacement requires the exact payload and a completion token after authorized
input ends; ordinary ownership and mode are preserved. Temporary-file cleanup
remains possible after cancellation or revocation.

`operations.ts` and `jobs.ts` persist execution and interruption recovery.
Workers recheck the saved actor and binding revision and acquire server,
project, and shared-root locks. Enqueueing does not confer authority over the
eventual job. Revocation blocks new privileged steps while preserving rollback,
helper cleanup, and safe restoration of initial running state. Parent update or
restore operations own state restoration for their nested backups, keeping the
server stopped between backup and mutation.

Backup readiness is a read-only advisory snapshot. It shares destination,
capacity, and static root validation with execution, without creating helpers,
stopping containers, or reserving capacity. Execution still rechecks authority,
bindings, roots, writers, and space under operation locks. Storage totals use
the same recorded-archive accounting as the global execution limit; available
disk space is read from the approved pinned destination and can be unknown.
Latest-success metadata is scoped to backup access and contains only the date
and size of a retained complete archive.

History queries filter in SQLite and use cursors to continue through older
records. Both histories order by creation time and use identifiers to break
ties. Operation reads resolve current server-view
authority before pagination, including for missing or suspended bindings, so
inaccessible work cannot contribute rows or pagination metadata. Audit remains
administrator-only. Public actors omit API-token fingerprints, and links between
audit events and operations use recorded identifiers. An audit status describes
the recorded action's outcome, independently of the operation's current status.

Compose updates derive sources from Docker observations and validate an immutable
snapshot for each confirmed operation. `ludock.compose.source` preserves original
paths across recreation; it never expands approved roots or enters public server
DTOs. There is no registration API or project approval state. The unused
`compose_projects` table and reserved null fingerprint field remain for storage
and identity compatibility. Validation includes transitive source reads; Compose
runs argument arrays with a restricted environment.

Schedules use expected revisions to reject stale edits and invalidate queued
work after editing or pausing. Due-slot evaluation and previews share timezone
logic: missed/spring-forward slots are skipped and repeated fall-back slots run
once. The consumed slot survives edits and pauses. Enqueueing and associating an
operation are atomic; last-result responses project that operation's persisted
state. A later skipped attempt clears the association so older work cannot
replace the latest result. Saved previews recheck owner authority and binding,
returning public reason codes without private diagnostics.

`notifications.ts` owns the persisted Discord delivery queue and its public
history projection. Administrator routes enqueue fixed test messages and retry
existing deliveries; the worker handles all outbound sends using the current
saved enabled configuration. Delivery requests use [Discord's `wait=true` option](https://docs.discord.com/developers/resources/webhook#execute-webhook)
to wait for confirmation that the message was saved. Public history contains
status and timing metadata plus fixed failure messages derived from safe codes,
excluding webhook URLs, payloads, event keys, response bodies, and exception text. Explicit retries retain
lifetime attempt counts and reset a separate five-attempt automatic retry budget.
In-flight tracking and the persisted retry count reject duplicate retry requests.
Audits identify test/retry delivery IDs without copying notification contents.

## Frontend ownership

`App.tsx` keeps page selection and administrator-only requirements together.
Server routes select the console layout or Servers navigation state. Unknown and
malformed client routes return to the dashboard; backend authorization remains
authoritative. The history-based navigation provider is independent of request
and form state.

`ServerDetail.tsx` owns loading, polling, permissions, notices, and drafts.
Panels in `components/server-detail` receive explicit props and callbacks, so
tab changes preserve confirmation text and selections. List, file, and activity
filters operate on their displayed data without changing operation locks.

Asynchronous reads abort or discard obsolete work. Audit and Diagnostics use
`usePageRead` for one current read and explicit refresh, hiding prior data during
loading or failure. Diagnostics publishes system and integration responses
together. Live server snapshots and file reads retain their feature-specific
freshness, path, and authorization policies.

Audit and operation history keep applied filters and pagination in the URL.
Operation detail routes read their selected record independently of the recent
server snapshot; browsing historical work cannot change current operation locks.

Account changes and session expiry invalidate pending authentication reads.
Cookie-changing requests and form mutations are serialized. Successful mutations
clear only submitted drafts; settings and grant editors require a successful
initial read before saving. File-dialog errors stay with their dialog, and
uncertain writes retain only a name draft until a fresh confirmation follows
reconciliation. Cancelled work cannot update a later session.

## Development instances

`scripts/dev.mjs` gives each canonical checkout separate ports, state, and cookies.
It watches shared contracts and uses `watch-backend.mjs` to drain the old backend
before replacement, preserving operation locks and helper cleanup across reloads.
The frontend starts only after the backend identifies the expected checkout;
proxy requests carry that identity and its session cookie. The asset-only preview
server has no backend proxy, so browser fixtures cannot reach development data.

Checkout and database locks prevent overlapping runs, including directory aliases.
Docker itself is not isolated by checkout state: development has no Docker
connection by default, and only one backend may manage a Docker host. See
[development setup and lock recovery](./TESTING.md#development) and
[verification commands](./TESTING.md#checks).
