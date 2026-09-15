# Ludock

Ludock is a self-hosted panel for existing Docker game servers, built with Bun,
React, and shared Zod contracts. Compose, Portainer, Dockge, or the Docker CLI
continues to own provisioning and configuration.

## How we work

Solve the maintainer's actual problem with a clear design. Simplify existing code
when useful and introduce abstractions as needed. There are no users yet; skip
compatibility layers and upgrade procedures unless requested. This guide provides
defaults; the maintainer sets the direction.

Keep this file brief, local explanations near the code, and scratch notes outside
the repository. Update user docs when procedures change.

## What matters

- **Useful controls.** Keep actions clear and support keyboard and mobile use.
  Preserve drafts and prevent stale responses from replacing newer state.
- **Responsiveness.** Avoid redundant Docker reads, excessive polling, and expensive rendering.
- **Explicit access.** Discovery is separate from permission. Enforce server/action
  grants within role ceilings in the backend and UI, including during ongoing work.
- **Safe operations.** Keep logical UUIDs distinct from Docker IDs; revalidate bindings
  before mutation. Confine file and Compose reads to approved roots, including through
  symlinks. Protect secrets and never reset real or unrelated data.
- **Recovery.** Persist work and retain locks until cleanup finishes. Backups keep servers
  stopped and restore their initial state; parent operations own nested backup restoration.
  Updates and forced recreation require an administrator, discovered Compose sources,
  argument arrays without a shell, and preservation of initially stopped state.

## Development

- `bun install --frozen-lockfile`, then `bun run dev`. Use the printed URLs and state
  path. Inspect settings with `bun run dev --print-config`.
- Bun runs all JavaScript; use native TypeScript 7 and Oxlint with matching
  `oxlint-tsgolint`. Follow `.bun-version` and `package.json`; keep `[run].bun = true`
  in `bunfig.toml`. Use `process.execPath` for child scripts. Prefer Bun/Web APIs,
  typed arrays, and `bun:test`; keep remaining filesystem and tar compatibility
  confined to their existing boundaries.
- Use a dedicated test daemon, disposable fixtures, and mock Discord endpoints;
  development has no Docker connection by default. Run one backend per Docker host.
  Clean up your own resources; stop processes with Ctrl+C or their captured PID.

## Checking and shipping

- Run focused tests, lint, and type checks, covering failures and affected entry points.
  Use `bun run check` for broad changes. Docs need content/link/diff checks. Report gaps.
- Build the frontend before `bun run --filter @ludock/frontend test:e2e`.
  README captures use `bun run --filter @ludock/frontend screenshots`; inspect them.
- Docker changes use `ludock:test` and the relevant `scripts/test-*.mjs` harnesses
  on Linux AMD64 and ARM64. Validate Compose changes with `docker compose config`.
- Keep dependencies current during substantive work, including major updates.
  Use `bun outdated --recursive` and `bun audit`; update `bun.lock` and fix findings.
  Pin Actions to full SHAs and images to verified multi-platform digests; prefer official images.
- Validated Conventional Commits are authorized. Release-please owns versions and
  the release manifest; preserve published tags. After preparation merges, review
  the refreshed release PR, approve its latest CI run, and wait for all checks.

## Code and docs

- `packages/backend`: API, Docker, and persisted operations. Keep console protocols in adapters.
- `packages/frontend`: React pages and controls. `packages/shared`: Zod contracts;
  change schemas with their producers and consumers.
- [README.md](./README.md) and [Operations](./docs/OPERATIONS.md): setup and user procedures.
