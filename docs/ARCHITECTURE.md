# Architecture and feature boundaries

Ludock is a TypeScript modular monolith: a Bun backend, a React frontend,
and shared Zod contracts. Feature modules use ordinary functions and explicit
dependencies. Docker, SQLite, filesystem helpers, and Compose are the external
boundaries. Setup and recovery procedures live in [Operations](./OPERATIONS.md);
check commands and acceptance scenarios live in [Testing](./TESTING.md).

## Runtime and persistence

Bun installs dependencies, builds both applications, and runs tests.
`packages/shared` exports its TypeScript source directly to Bun and TypeScript;
it has no separate build or watcher. The backend bundles the server and account
recovery entry points with production dependencies external. The frontend bundles
browser assets from `index.html`.

The production image includes Bun, Docker CLI, Compose, and production
dependencies on Alpine. Build tools run on the build platform; the Bun executable
matches the target platform. Images support AMD64 and ARM64. Data helpers use
Bun's official **distroless** image, pinned in
[`runtime-images.ts`](../packages/backend/src/runtime-images.ts), and execute
Bun directly without shell utilities. A validator's host-root mount remains
privileged even when read-only and network-disabled; overrides must be trusted
and digest-pinned.

`database.ts` inspects nonempty SQLite databases read-only before enabling WAL
or applying supported migrations. It rejects unrelated storage and unsupported
schemas without writes. The supported schema starts at version 2.

Identity and Compose-source fingerprints use domain-separated HMACs with an
installation key outside SQLite. This prevents a database-only disclosure from
verifying guesses of game credentials or environment values. The mode-0600 key
lives in a mode-0700 `.identity-key` directory beside the canonical database path.
The key and directory are synced before dependent schema changes commit. Missing,
unsafe, or mismatched keys fail closed; database backups must include that directory.

Passwords use Argon2id. Bounded legacy scrypt verification upgrades a hash only
after rechecking the enabled account and unchanged password. Queued API-token work
stores an HMAC of the credential generation, so rotation rejects old privileged
steps while still allowing recovery cleanup.

At the storage boundary, `yaml` rejects duplicate keys and bounds Compose alias
expansion; `tar-stream` exposes archive entries for path, type, ownership, mode,
and byte-limit validation with backpressure. Preserve these controls during
[dependency updates](./TESTING.md#dependency-and-image-updates).

## Contracts and errors

`packages/shared/src` owns public schemas and inferred types. Routes validate
inputs and pass explicit public projections to `respond`, which checks response
types and validates values before serialization. Frontend `apiJson`, `apiResponse`,
and WebSocket consumers validate incoming data. Validation does not replace
credential redaction or permission filtering.

`LogicalServerId` and `DockerContainerId` are distinct branded types. Public routes
use logical UUIDs; Docker wrappers require inspected or revalidated physical
bindings. Compile-only tests protect these boundaries and route-policy signatures.

Expected failures extend `AppError` and share one HTTP renderer. Their message and
code are deliberately public; arbitrary exceptions stay generic and go to internal
diagnostics. Persisted operation failures use the same safe-error boundary.

## Backend ownership

| Boundary | Modules and responsibility |
| --- | --- |
| Application | `index.ts` owns listener/shutdown; `app.ts` composes routing, authentication, request limits, and security headers |
| Policy | `identity.ts` reconciles Docker observations; `authorization.ts` owns capabilities and role ceilings; `servers.ts` resolves authorized bindings and lock keys |
| HTTP | `routes/` translates requests into domain calls and public projections |
| Direct work | `routes/server-action.ts` retains authorization, resource locks, and cleanup through direct lifecycle and file requests |
| Queued work | `operations.ts` owns persisted execution/recovery; `jobs.ts` supplies handlers; backups and Compose modules own their recovery protocols |
| Data access | `file-storage.ts`, `backup-storage.ts`, and `mount-proof.ts` configure helper mounts and validate storage boundaries |
| Helpers | `helpers/` contains checked JavaScript programs embedded as text; `docker-helpers.ts` owns create/pull/remove behavior |
| Read models | `history.ts` filters/paginates in SQLite; `attention.ts` reads only the summaries needed for authorized dashboard items |
| Background work | `schedules.ts`, `monitoring.ts`, and `notifications.ts` own scheduling, availability, and delivery state |

### Direct requests

`serverAction(capability, handler)` resolves the binding and holds server,
project, and shared-root locks until the response **and cleanup** finish, including
cancelled transfers. Its `assertAccess` callback rechecks sessions, grants, and
bindings after asynchronous preparation and immediately before dispatch.
Lifecycle actions also recheck the final inspected observation.

Filesystem confinement uses pinned descriptors and Linux `/proc/self/fd` checks.
Uploads write an exclusive temporary sibling; replacement requires the exact
payload and a completion token after authorized input ends. Cleanup must remain
possible after cancellation or revocation and preserve ordinary ownership/mode.

`websocket-server.ts` owns upgrades, revalidation, and shutdown; adapters own
protocol behavior. Channels bound buffering and keep locks through cancellation.
Minecraft commands are literal Docker-exec arguments with an in-container watchdog.
A lost `exec.start` response does not prove Docker rejected a mutation, so an HTTP
timeout cannot release its lock. Limits live in
[`game-console-runtime.ts`](../packages/backend/src/game-console-runtime.ts).

### Queued work and recovery

Enqueueing grants no lasting authority. Workers recheck the saved actor, binding,
and schedule revision under resource locks. Revocation blocks new privileged steps
while preserving rollback, helper cleanup, and safe restoration of initial running
state. Invalid persisted job data becomes a failed operation without wedging the queue.

Backups keep the server stopped throughout copying. Parent update/restore jobs own
state restoration for nested backups. Restore journals record each root's progress;
recovery validates their shape and exact roots before acting. Uncertain recovery
keeps the server stopped. Read-only backup preflight shares planning checks with
execution but reserves no capacity and confers no authority.

Compose updates validate an immutable snapshot, including transitive source reads,
and invoke argument arrays with a restricted environment. `ludock.compose.source`
preserves owner paths across recreation without expanding approved roots or entering
public DTOs. The unused `compose_projects` table and reserved null fingerprint field
remain for storage/identity compatibility; there is no registration API.

Schedules persist consumed timezone slots and expected revisions. Enqueueing and
linking an operation are atomic; last results reference persisted operation state.
Monitoring shares operation suppression and locks. Notifications use a persisted
queue and the current saved configuration; public history contains safe status/timing
metadata, never webhook URLs, payloads, event keys, or raw response bodies.

### Reads and outages

History queries filter in SQLite before pagination, order by creation time with ID
tie-breakers, and resolve current access before returning rows or cursor metadata.
Attention checks the most recent 100 operations per server through a narrow SQL
projection, without loading full operation histories or decoding unused results.

Failed discovery preserves logical identity and returns an explicit unavailable
flag. Authorized saved server snapshots remain readable with unknown live state;
mutations still require fresh discovery and binding validation.

## Frontend ownership

`App.tsx` owns page selection and administrator-only requirements. The history-based
navigation provider is independent of requests and form state. Detail query
parameters select allowed tabs and specific operations/schedules; destination
permission checks still apply.

`ServerDetail.tsx` owns the live server/operation snapshot, permissions, and drafts.
Backup and schedule histories load only for their active permitted panel and report
failures locally. Their mutations require fresh panel state; a history failure does
not disable unrelated lifecycle controls. Drafts live above panels so tab changes
preserve selections and confirmation text.

`usePageRead` owns cancellable reads, explicit refresh, and obsolete-response
rejection. Settings sections and Account reuse it. A null reader disables a panel;
polling panels may retain data while refreshing but clear it on failure. Core live
server and file reads retain their specialized freshness/authorization policies.
Backup and notification settings own separate reads, drafts, and save lifetimes.

Cookie-changing requests and form mutations are serialized within their owner.
Successful saves clear only submitted drafts. Read failures block the affected
editor; late work cannot update a later session. Historical operation detail has
its own reader and never determines current server locks.

## Development and preview

`scripts/dev.mjs` assigns checkout-specific ports, state, and cookies.
`watch-backend.mjs` watches backend and shared source and drains the old backend
before replacement. The frontend starts only after verifying the backend's checkout
identity; proxy requests carry that identity and its session cookie.

Production and the asset-only browser preview use the same `static-files.ts`
handler. Missing assets and API/WebSocket paths never fall back to the SPA document.
Preview has no backend proxy, keeping browser fixtures away from development data.

Checkout/database locks prevent overlapping runs, including directory aliases.
Docker itself is not isolated: development defaults to no connection, and one backend
may manage a Docker host. See [development setup](./TESTING.md#development).
