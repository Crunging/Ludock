# Ludock

A Bun backend, React frontend, and shared Zod contracts under `packages/`.
Ludock manages existing Docker game servers; their owning manager keeps control
of provisioning and configuration. User guidance lives in [README.md](./README.md)
and [Operations](./docs/OPERATIONS.md).

## Working defaults

- Follow the maintainer's request. Workflow guidance is a default, not an approval gate.
- Prefer simple modules and existing UI styles. Preserve keyboard/mobile access,
  form drafts, and protection against stale responses.
- There are no users yet; avoid compatibility layers and upgrade procedures unless requested.
- Keep this file short. Let code, tests, and workflows carry implementation details.

## Development

- Use Bun for development, builds, tests, and production; TypeScript 7's native
  `tsc` for types and Oxlint for linting. Keep `oxlint-tsgolint` aligned with TypeScript.
- Follow `.bun-version` and the minimum in `package.json`. Keep `[run].bun = true`
  in `bunfig.toml`; use `process.execPath` for JavaScript subprocesses.
  `node:*` imports are Bun compatibility APIs.
- Start with `bun install --frozen-lockfile`, then `bun run dev`.
  `bun run dev --print-config` shows settings; use the runner's printed URLs and state path.
- Development defaults to no Docker connection. Use a dedicated test daemon,
  disposable fixtures, and mock Discord endpoints. Run one backend per Docker host.
  Clean up only your own resources; stop processes by their captured PID or Ctrl+C.

## Core rules

- Keep logical server UUIDs separate from Docker IDs. Revalidate bindings before mutation.
- Discovery does not grant access. Enforce explicit server/action grants within
  role ceilings in both backend and UI; recheck authority during long-running work.
- Confine file and Compose reads to approved roots, including through symlinks.
  Keep secrets private. Never reset real or unrelated data to work around development issues.
- Persist operations and recover after interruption. Preserve authorization, locks,
  and cleanup lifetimes, including those owned by `serverAction`.
- Keep servers stopped throughout backup copying and restore their initial running state.
  Parent update/restore operations own state restoration for nested backups.
- Updates and forced recreation require an administrator and discovered Compose sources.
  Invoke argument arrays without a shell and preserve initially stopped state.
- Change shared schemas with their producers and consumers. Keep console protocols in adapters.

## Validation and dependencies

- Run focused tests, lint, and type checks; use `bun run check` for broad changes.
  Documentation-only edits need content, link, and diff checks. Report material gaps.
- For browser changes, build the frontend and run
  `bun run --filter @ludock/frontend test:e2e`. Regenerate README screenshots with
  `bun run --filter @ludock/frontend screenshots` and inspect the images.
- For Docker changes, build `ludock:test` and run affected harnesses under `scripts/`:
  `test-linux.mjs`, `test-compose.mjs`, `test-files.mjs`, and `test-backups.mjs`.
  Validate `linux/amd64` and `linux/arm64`; check Compose edits with `docker compose config`.
- Keep dependencies current during substantive work, including needed major updates.
  Use `bun outdated --recursive` and `bun audit`; update `bun.lock` and fix security findings.
- Pin Actions to full upstream SHAs and images to verified multi-platform digests.
  Prefer official images; check application and helper images on both architectures.

## Commits and releases

- Validated Conventional Commits are authorized. Keep scratch notes outside the repo.
- Leave versions and the release manifest to release-please; preserve published tags.
- After preparation merges, review the refreshed release PR, approve its latest CI run,
  and wait for all checks before merging it.
