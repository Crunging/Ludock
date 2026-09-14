# Architecture and feature boundaries

Ludock is a TypeScript modular monolith: a Bun backend, a React frontend,
and shared Zod contracts. The backend uses ordinary functions and explicit
dependencies. Docker, SQLite, filesystem helpers, and Compose are the external
boundaries.

## Runtime and persistence

Bun supplies the application runtime, workspace installation, JavaScript builds,
and test runner. Development and CI follow the latest stable Bun 1 release,
selected by [`.bun-version`](../.bun-version), with 1.4.2 as the supported minimum.
Dependency manifests use compatibility ranges; `bun.lock` and the isolated
workspace linker define resolved versions. The backend build bundles `index.ts`
and `recovery.ts` for Bun with external production packages. The frontend build starts from
`index.html` and bundles its scripts, styles, and assets for browsers. TypeScript
checks types and emits the shared package's JavaScript and declarations.

Docker builds compile shared contracts once for both bundles on the build
platform, with source inputs separate from tests and local artifacts. The runtime
copies the Bun executable for the target platform and a separate production-only
dependency installation. CI checks both native Linux architectures before the
publish workflow builds one image index for nightly and any stable release tags.

`database.ts` uses `bun:sqlite` and validates nonempty databases read-only before
enabling WAL or applying supported migrations; unrelated or unsupported storage
is rejected without writes. `password.ts` uses Bun's Argon2id hashing for new passwords and
bounded verification of legacy scrypt hashes. Successful sign-in upgrades an
older hash only after rechecking that the account is enabled and its password
has not changed during asynchronous verification.

SHA-256 token hashes, operation keys, and backup checksums use `Bun.CryptoHasher`.
From the first supported schema (version 2), binding and Compose-source
fingerprints wrap their canonical digests in separate
domain-separated HMACs using an installation key kept outside SQLite. This
prevents a database-only disclosure from becoming an offline verifier for
low-entropy game credentials or environment-file values. The key is a mode-0600
`key` file inside an atomically published mode-0700 `.identity-key` directory
beside the canonical database path. The key and its directory entries are synced
before SQLite commits the schema that depends on them; missing, unsafe, or
mismatched keys fail closed. Unsupported pre-version-2 development databases are
rejected during read-only inspection. UUIDs and session-token randomness use Web
Crypto. HMAC, constant-time comparisons, and legacy scrypt verification use
`node:crypto`.

Queued API-token operations carry a separate domain-separated HMAC identifying
the configured credential generation. This is not a user-password verifier:
request authentication still checks the bearer token, while workers compare the
generation to reject work queued before token rotation. A database-only copy
does not contain the HMAC key. Administrative API tokens must still be random;
the minimum length alone does not guarantee entropy.

Filesystem confinement uses descriptor-based `node:fs` operations and Linux
`/proc/self/fd` checks in file, backup, restore, and Compose validation paths.
Several dependencies provide behavior needed at these boundaries:

- `yaml` rejects duplicate mapping keys and limits alias expansion to 20 in Compose
  inputs. [`Bun.YAML.parse`](https://bun.com/docs/runtime/yaml) currently does not
  expose equivalent parser controls.
- `tar-stream` exposes individual archive-entry streams so Ludock can validate
  paths, entry types, ownership, and modes while enforcing byte limits and
  backpressure.
  [`Bun.Archive.files()`](https://bun.com/docs/runtime/archive) returns an in-memory
  map of regular files and cannot provide that validation path.
- Dockerode handles Docker exec/attach connection upgrades and multiplexed streams.
  Replacing it with `fetch` would require maintaining a Docker protocol client.

Helper containers use the digest-pinned
`oven/bun:1-alpine` image, defined centrally in
[`runtime-images.ts`](../packages/backend/src/runtime-images.ts); the production
image copies the target platform's Bun executable into `alpine:3` alongside the
Docker CLI and Compose plugin. Build and helper image digests are updated together
after reviewing upstream releases. Digest-pinned helper overrides are required;
Docker verifies image content against the requested digest when pulling. The
validator's host-root mount remains a privileged trust boundary even with a
read-only mount and disabled networking, so only trusted helper images may be
selected. Updating tools, pins, and locked dependencies follows the maintenance
policy in [AGENTS.md](../AGENTS.md#dependencies-and-pins).

## Shared contracts

`packages/shared/src` groups public request schemas, response schemas, and their
inferred types by feature: access, authentication, servers, files, backups,
operations, schedules, monitoring, Compose, diagnostics, and logging. `index.ts`
only exports those modules.

Backend routes validate input with these schemas. The `respond` helper in
`packages/backend/src/routes/request.ts` checks response types at compilation
and validates their actual values before serialization. A malformed producer
response becomes a generic server error rather than a client validation error.
Build explicit public projections before calling it: a schema is not a substitute
for credential redaction or permission filtering.

Frontend JSON calls use `apiJson(path, responseSchema, options)` or `apiResponse`
for an existing fetch response. Return types come from the schema. Unexpected
response bodies produce a fixed error without displaying their values. Event
and console WebSocket messages are validated as well.

`LogicalServerId` and `DockerContainerId` are distinct branded types. Public
server routes use logical UUIDs. Docker wrappers accept physical references
obtained from an inspected container or a validated stored binding. Do not
convert a public UUID into a Docker reference merely to satisfy a type check.
Compile-only checks in `packages/backend/test/typechecks` protect these
boundaries and the route response/policy signatures.

## Backend routes and domain logic

`index.ts` owns the native `Bun.serve` listener and coordinated shutdown.
`app.ts` composes Bun's route table, authentication, request limits, security
headers, and static frontend responses. Feature handlers in `src/routes` receive
a typed request context and return native `Response` objects:

| Module | HTTP responsibility |
| --- | --- |
| `accounts.ts` | Setup, sign-in, sessions, passwords, and account administration |
| `servers.ts` | Server lists/details and direct lifecycle actions |
| `files.ts` | File browsing, transfers, and mutations |
| `access.ts` | Per-user grants and reviewed server bindings |
| `backups.ts` | Backup settings, metadata/downloads, creation, and restore requests |
| `compose.ts` | Automatic source discovery, update capability, and update requests |
| `schedules.ts` | Schedule creation, editing, pause/resume, next-run previews, and removal |
| `status.ts` | Operation progress and availability configuration |
| `settings.ts` | Notification settings and administrator diagnostics/integrations |

Compose updates resolve source paths from internal Docker observations and validate
an immutable snapshot for each confirmed operation. Original paths survive
recreation in `ludock.compose.source`; this label never expands approved roots
or enters public server DTOs. There is no registration API or project approval
state. The historical `compose_projects` table remains unused for database schema
compatibility, and the reserved null field in binding fingerprints preserves
existing server identities.

`websocket-server.ts` owns native Bun upgrades, connection and payload limits,
session revalidation, and shutdown. Console and log handlers use a socket channel
that preserves bounded buffering and connection cleanup; game-specific protocol
behavior stays in adapters. Response streams propagate cancellation and retain
the authorization, resource-lock, and helper-cleanup lifetimes of their work.

RCON and Telnet use `Bun.connect` with explicit connection and command deadlines,
queued partial writes, and cleanup after failure or completion. Protocol parsing
and authorization checks remain inside their adapters. Pending native connections
cannot be canceled until Bun exposes their sockets, so late connections are closed
and the CLI exits after coordinated shutdown has drained application work and
closed SQLite. Calling the imported server's `shutdown()` does not exit the process.

Minecraft's Docker-exec adapter keeps commands as literal argument-array values
and runs an in-container watchdog: 15 seconds before termination, then a
three-second kill grace period. Output is capped at 4 MiB, and a closed or revoked
console requests cancellation while retaining its lock through stream cleanup.
Waiting for exec creation is limited to five seconds and can be cancelled before
starting; late creation results are discarded without executing them. These bounds do not
cover an indefinitely stalled Docker start request. A disconnected `exec.start`
request cannot prove that Docker did not accept the mutation; releasing its lock
on an HTTP-only timeout would be unsafe. Docker availability and recovery remain
deployment responsibilities.

Direct lifecycle and file handlers use `serverAction(capability, handler)`.
Declaring the capability is required by its signature. The helper resolves the
authorized binding and passes a typed context to the handler. It holds resource
locks until both the response and handler cleanup have finished, and checks
session/grant revocation during long transfers. Authorization does not depend on
a separate table that guesses a capability from the request's URL.
Handlers pass the context's synchronous `assertAccess` callback into Docker and
file helpers so they recheck the request session, grants, and binding after
asynchronous preparation, immediately before dispatch. Lifecycle dispatch also
checks the final inspected observation. Console transports apply the same rule
before sending credentials or commands.

Uploads use an exclusive temporary sibling beneath the pinned destination
directory. The helper requires the exact payload and a completion token sent only
after a clean, authorized input end, then preserves ordinary ownership/mode and
replaces the destination. Writable file helpers add `CHOWN` for that preservation;
read-only helpers do not. Scoped temporary-file cleanup remains available after
request cancellation or access revocation.

Durable actions enqueue operations with their actor and binding revision.
`operations.ts` and `jobs.ts` own execution and recovery; their handlers recheck
authorization and acquire the server/project/shared-root locks. A queued request
does not acquire lifetime ownership of the eventual job. Backups, restore,
schedules, monitoring, and Compose validation remain in their domain modules.
Job dispatch rechecks the owner's current authority after preparation. Recovery
cleanup and restoration of initial running state remain possible after authority
is revoked; denying new work must not strand partially restored data.

Schedule edits and pause/resume use an expected revision to reject stale writes.
Each actual change advances that revision; queued scheduled operations carry the
revision they were created under and recheck it before dispatch. The last consumed
local-time slot is retained across edits and pauses so the same occurrence is not
replayed. Shared timezone helpers calculate both due slots and next-run previews,
including skipped spring-forward times and once-only repeated fall-back times.
Saved previews also check the owner's current authority and original binding.
Their public reason codes distinguish disabled owners, missing access, and binding
changes without returning private diagnostics. Each attempted slot records its
time and, when queued, its operation association. Enqueueing and associating that
operation are atomic. Schedule responses project the associated operation's
current persisted result, so normal completion and interruption recovery share
one source of truth. A later skipped attempt clears the association; an older
operation finishing cannot replace the latest schedule result.

`identity.ts` and `servers.ts` separate logical history from live Docker
observations. `authorization.ts` owns capability and role ceilings. File helpers
and mount proofs own filesystem confinement. Console adapters own their protocol
behavior. Feature routes should call these policies rather than duplicate them.

## Frontend ownership

`App.tsx` keeps page selection and administrator-only requirements in the same
route definitions. Server-tool routes also select the console layout or the
Servers navigation state. Unknown and malformed routes return to the dashboard;
backend authorization remains authoritative. The small history-based navigation
provider keeps routing independent of request and form state.

`ServerDetail.tsx` coordinates server loading, polling, permissions, notices,
and form drafts. Panels in `components/server-detail` render activity, backups,
schedules, updates, availability, and binding review through explicit props and
callbacks. Drafts remain above the panels so switching tabs does not discard
confirmation text or selections.

Asynchronous page reads own an abort controller and discard obsolete responses.
Audit and Diagnostics share `usePageRead` for one current read and explicit
refresh. It clears the previous snapshot while loading or after failure, and
aborts outstanding work when replaced, finished, or unmounted. Diagnostics
publishes its system and integration responses together. Live server snapshots,
file browsing, and mutation-driven refreshes retain their feature-specific
authorization and ordering policies.
Account changes and session expiry invalidate pending authentication reads.
Cookie-changing authentication requests and form mutations are serialized;
successful mutations clear only the draft values they submitted. Settings and
grant editors require a successful initial read before they can save.

The file browser keeps each folder location with its load result, dialog errors
with their dialog, and upload progress with its pending work. A server or account
reload resets the whole view state. Request owners remain separate so canceled
work cannot update a later session; uncertain writes retain only a name draft
and require a fresh confirmation after the folder is reconciled.

The dashboard's header and server rows share one CSS grid through subgrid.
Different action counts do not change a row's state or port column. Responsive
rules switch to compact rows at narrow widths.

## Development instances

`bun run dev` runs `scripts/dev.mjs`. It uses the checkout's canonical path to
select stable preferred ports, an external state directory, and a distinct
session cookie. It builds and watches shared contracts alongside the backend
and Bun's frontend development server. `scripts/watch-backend.mjs` watches backend source
and shared output, signals the current Bun child, and waits for its shutdown
before starting a replacement. This preserves active operation and helper cleanup
across reloads. Repeated stop signals retain the backend's in-progress drain.
The frontend starts after its backend identifies the expected checkout; proxy
requests carry that identity and only that checkout's session cookie.

The frontend's Bun development server provides HTML bundling and hot reloading,
plus streaming HTTP and WebSocket proxies. Its separate preview server serves
only built files and has no backend proxy, so browser fixtures cannot reach a
development backend through preview.

Checkout and database locks prevent overlapping managed development runs, even
when different checkouts specify the same database through directory aliases.
Explicit port conflicts fail; automatically selected ports may advance to a free
pair. The runner stops its children together and removes its own locks on exit.

Development has no Docker connection by default. Set `DOCKER_SOCKET` for a
dedicated development daemon when testing Docker features. Separate databases,
ports, and cookies do not isolate Docker: run only one backend against a given
Docker host. See [local development](./TESTING.md#development) for overrides and recovery
from an interrupted development run, and [the testing reference](./TESTING.md#checks) for checks.
