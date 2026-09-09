# Architecture and feature boundaries

Ludock is a TypeScript modular monolith: an Express backend, a React frontend,
and shared Zod contracts. The backend uses ordinary functions and explicit
dependencies. Docker, SQLite, filesystem helpers, and Compose are the external
boundaries.

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

`app.ts` installs shared HTTP handling and account endpoints. `routes.ts` and
`advanced-routes.ts` compose feature routers from `src/routes`:

| Module | HTTP responsibility |
| --- | --- |
| `servers.ts` | Server lists/details and direct lifecycle actions |
| `files.ts` | File browsing, transfers, and mutations |
| `access.ts` | Per-user grants and reviewed server bindings |
| `backups.ts` | Backup settings, metadata/downloads, creation, and restore requests |
| `compose.ts` | Registered projects, update capability, and update requests |
| `schedules.ts` | Schedule creation, listing, and removal |
| `status.ts` | Operation progress and availability configuration |
| `settings.ts` | Notification settings and administrator diagnostics/integrations |

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

Durable actions enqueue operations with their actor and binding revision.
`operations.ts` and `jobs.ts` own execution and recovery; their handlers recheck
authorization and acquire the server/project/shared-root locks. A queued request
does not acquire lifetime ownership of the eventual job. Backups, restore,
schedules, monitoring, and Compose validation remain in their domain modules.
Job dispatch rechecks the owner's current authority after preparation. Recovery
cleanup and restoration of initial running state remain possible after authority
is revoked; denying new work must not strand partially restored data.

`identity.ts` and `servers.ts` separate logical history from live Docker
observations. `authorization.ts` owns capability and role ceilings. File helpers
and mount proofs own filesystem confinement. Console adapters own their protocol
behavior. Feature routes should call these policies rather than duplicate them.

## Frontend ownership

`ServerDetail.tsx` coordinates server loading, polling, permissions, notices,
and form drafts. Panels in `components/server-detail` render activity, backups,
schedules, updates, availability, and binding review through explicit props and
callbacks. Drafts remain above the panels so switching tabs does not discard
confirmation text or selections.

Asynchronous page reads own an abort controller and discard obsolete responses.
Account changes and session expiry invalidate pending authentication reads.
Cookie-changing authentication requests and form mutations are serialized;
successful mutations clear only the draft values they submitted. Settings and
grant editors require a successful initial read before they can save.

The dashboard's header and server rows share one CSS grid through subgrid.
Different action counts do not change a row's state or port column. Responsive
rules switch to compact rows at narrow widths.

For parallel work, assign a feature's contract, router/domain changes, panel,
and focused tests together when practical. Coordinate edits to shared transport,
authorization, operation lifetime, and page orchestration explicitly. A feature
split does not weaken the need for one integrated `pnpm check`.

## Development instances

`pnpm dev` runs `scripts/dev.mjs`. It uses the checkout's canonical path to select
stable preferred ports, an external state directory, and a distinct session
cookie. It builds and watches shared contracts alongside the backend and Vite.
Node's built-in backend watcher follows imported modules and waits for shutdown
before restarting. Repeated stop signals retain the backend's in-progress drain.
The frontend starts after its backend identifies the expected checkout; proxy
requests carry that identity and only that checkout's session cookie.

Checkout and database locks prevent overlapping managed development runs, even
when different checkouts specify the same database through directory aliases.
Explicit port conflicts fail; automatically selected ports may advance to a free
pair. The runner stops its children together and removes its own locks on exit.

Development has no Docker connection by default. Set `DOCKER_SOCKET` for a
dedicated development daemon when testing Docker features. Separate databases,
ports, and cookies do not isolate Docker: run only one backend against a given
Docker host. See [CONTRIBUTING.md](../CONTRIBUTING.md) for overrides and recovery
from an interrupted development run, and [TESTING.md](../TESTING.md) for checks.
