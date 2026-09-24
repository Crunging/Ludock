# Development and CI

## Local development

Install the Bun version in [`.bun-version`](../.bun-version), then run:

```sh
bun install --frozen-lockfile
bun run dev
```

Open <http://127.0.0.1:3000> and use the setup code printed in the output.
The frontend hot-reloads and forwards `/api` and `/ws` to the backend on port
3001, which restarts when its source changes. Data lives in `data/dev/`
(override with `LUDOCK_DB_PATH`); `LUDOCK_DEV_PORT` and `PORT` change the
frontend and backend ports.

Development has no Docker connection by default. To manage containers, set
`DOCKER_SOCKET` to a dedicated test daemon with disposable game data, never one
running real servers.

Bun runs all JavaScript; TypeScript 7 and Oxlint use native executables. Keep
`[run].bun = true` in `bunfig.toml`. Bun's built-in `node:fs`, `node:path`, and
`node:os` APIs are supported.

## Checks

```sh
bun run check
bun run --filter @ludock/frontend build
bun run --filter @ludock/frontend test:e2e
```

Use `bun run lint:scripts` to lint repository scripts separately.

Browser cases run on desktop by default. Use `@responsive` to also run on mobile,
or `@mobile` for dedicated mobile cases.

Docker acceptance uses `ludock:test`. Run the
[Linux](../scripts/test-linux.mjs), [file](../scripts/test-files.mjs),
[backup](../scripts/test-backups.mjs), [packaged](../scripts/test-packaged.mjs),
and [Compose](../scripts/test-compose.mjs) harnesses sequentially against a
dedicated daemon. Validate Linux AMD64 and ARM64 and report any emulation.
The harnesses apply the example deployment's filesystem and capability
restrictions, including to packaged file, backup, and Compose operations.
For just the production startup check, run
`bun scripts/test-linux.mjs --smoke-only`.

## Builds and releases

`bun run build` builds the backend and frontend. Keep `packages/backend/dist`
together: its entry points share generated chunks and source maps.

Every PR, including the release PR, runs the full CI suite on native AMD64 and
ARM64. Release Please keeps a release PR open on `main`; merging it tags the
version, publishes the GitHub release with its changelog, and pushes the
multi-platform image to `ghcr.io` as `X.Y.Z`, `X.Y`, `X`, and `latest`. Release
Please owns versions, `CHANGELOG.md`, and the release manifest; preserve
published tags. A weekly workflow audits dependencies and scans the latest
published image.

PRs opened with the default `GITHUB_TOKEN` do not trigger CI. For CI on release
PRs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a fine-grained personal
access token restricted to this repository, with Contents, Issues, and Pull
requests write permissions. Wait for the release PR's checks before merging.

## Tool updates

```sh
bun outdated --recursive
bun audit
```

Update `.bun-version` and the Bun image pin in the Dockerfile together; CI
installs the version in `.bun-version`. File and backup helpers reuse the running
production image automatically. Native runs use the reviewed
`FALLBACK_HELPER_IMAGE` pin in `packages/backend/src/runtime-images.ts`; update
it when the Bun minimum changes.

Workflows pin every action to a full commit SHA (with its release as a comment)
and every image to a digest; `scripts/test/security-pins.test.mjs` enforces
this. Image checks in [`scripts/ci/containers.mjs`](../scripts/ci/containers.mjs)
reject fixable medium, high, and critical vulnerabilities in the production
runtime and fallback helper.
