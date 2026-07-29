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

The repository contains an Express backend in `packages/backend` and a React
frontend in `packages/frontend`.

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

- Manage only containers labeled `ludock.enable=true`.
- Keep file operations inside configured roots, including through symlinks.
- Preserve administrator, operator, and viewer permissions.
- Implement new console protocols as adapters.

Use [TESTING.md](./TESTING.md) for manual acceptance checks.
