# Repository Guide

Ludock is a pnpm monorepo for managing explicitly opted-in Docker
game servers.

## Structure

- `packages/backend`: Express API, authentication, Docker access, console
  adapters, and file operations.
- `packages/frontend`: React and Vite control panel.
- `packages/backend/test`: backend unit and HTTP integration tests.
- `TESTING.md`: manual acceptance criteria and product boundaries.

## Development

- Requires Node.js 24 and pnpm 10.
- Run `pnpm check` before committing.
- Run `docker build -t ludock:test .` after container or deployment
  changes.

## Constraints

- Only manage containers with `ludock.enable=true`.
- Keep filesystem access within configured roots and preserve symlink traversal
  protections.
- Never expose console credentials, API tokens, passwords, or session tokens.
- Preserve administrator, operator, and viewer permission boundaries.
- Add protocol-specific console behavior through an adapter; do not assume one
  command transport works for every game.
