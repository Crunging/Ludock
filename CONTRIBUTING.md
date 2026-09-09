# Contributing

Issues and focused pull requests are welcome. For security reports, follow
[SECURITY.md](./SECURITY.md) instead of opening a public issue.

## Development setup

Ludock requires Node.js 24 and pnpm 10.

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm dev
```

The repository contains an Express backend in `packages/backend`, a React
frontend in `packages/frontend`, and runtime-validated contracts in
`packages/shared`. Run one backend per Docker host; use disposable game data
for development. Rebuild the shared package after contract changes.

## Before opening a pull request

Run the full source check:

```bash
pnpm check
```

If the change affects the image, Compose configuration, or deployment, also
run:

```bash
docker build -t ludock:test .
```

Add or update tests for behavior changes. Keep these product boundaries intact:

- Discover recognized images automatically, respect explicit opt-out, and require
  `ludock.enable=true` for unknown images.
- Keep file operations inside configured roots, including through symlinks.
- Preserve role ceilings and independent per-server action grants in every API,
  WebSocket, transfer, operation, and schedule.
- Implement new console protocols as adapters.
- Preserve logical identity checks, stop-only backups, and safe recovery.
- Keep v2 application storage separate from incompatible older databases.
- Validate runtime/helper dependencies on both Linux AMD64 and ARM64 for
  deployment changes.

Use [TESTING.md](./TESTING.md) for manual acceptance checks.
