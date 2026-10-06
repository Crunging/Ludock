# Development

## Local development

Install the Bun version in [`.bun-version`](../.bun-version), then run:

```sh
bun install --frozen-lockfile
bun run dev
```

Open <http://127.0.0.1:3000> and use the setup code printed in the output. The
frontend hot-reloads and forwards `/api` and `/ws` to the backend on port 3001,
which restarts when its source changes.

- **Separate terminals:** `bun run backend:dev` and `bun run frontend:dev`. The
  package-level `dev` commands use the same defaults.
- **Data** lives in `data/dev/`; set `LUDOCK_DB_PATH` to move it.
  `LUDOCK_DEV_PORT` and `PORT` change the frontend and backend ports.
- **Docker** isn't connected by default. To manage containers, set
  `DOCKER_SOCKET` to a dedicated test daemon with disposable game data, never one
  running real servers.

Bun runs all JavaScript, including Bun's built-in `node:fs`, `node:path`, and
`node:os`. TypeScript 7 and Oxlint run as native executables. Keep
`[run].bun = true` in `bunfig.toml`.

## Checks

```sh
bun run check                                # typecheck, lint, unit tests, builds
bun run --filter @ludock/frontend build
bun run --filter @ludock/frontend test:e2e   # browser tests in Chromium
bun run lint:scripts                         # repository scripts only
```

Keep a small set of tests that prove the main integrations work, plus tests for
access control, data safety, recovery, and races. Browser tests cover a few
important workflows; check rendering, copy, and layout by hand.

### Docker acceptance tests

These run the `ludock:test` image against a dedicated Docker daemon. Run the
[Linux](../scripts/test-linux.mjs), [file](../scripts/test-files.mjs),
[backup](../scripts/test-backups.mjs), [packaged](../scripts/test-packaged.mjs),
and [Compose](../scripts/test-compose.mjs) harnesses one at a time, on both Linux
AMD64 and ARM64, and note any emulation. For only the production startup check,
run `bun scripts/test-linux.mjs --smoke-only`.

- **Choosing the daemon:** use a local Unix-socket Docker context or
  `DOCKER_HOST`; `DOCKER_CONTEXT` wins when both are set. Remote TCP and SSH
  contexts aren't supported.
- **Socket path:** Docker Desktop uses its VM's `/var/run/docker.sock`. For
  another mapping, set `LUDOCK_TEST_DOCKER_SOCKET` to its absolute path on the
  daemon host.
- Harnesses check that the mounted socket belongs to the selected daemon before
  doing any work, and apply the example deployment's filesystem and capability
  restrictions.

## Builds and releases

`bun run build` builds the backend and frontend. Keep `packages/backend/dist`
together; its entry points share generated chunks and source maps.

- **CI:** every PR, including the release PR, runs the full suite on native
  AMD64 and ARM64. A weekly workflow audits dependencies and scans the latest
  published image.
- **Nightly:** every push to `main` publishes the multi-platform image to
  `ghcr.io` as `nightly`.
- **Releases:** Release Please keeps a release PR open on `main`. Merging it tags
  the version, publishes the GitHub release and changelog, and publishes `X.Y.Z`.
  Stable releases also move `X.Y`, `X`, and `latest`. Release Please owns
  versions, `CHANGELOG.md`, and the release manifest; don't edit them by hand or
  move published tags.

PRs opened with the default `GITHUB_TOKEN` don't trigger CI. For CI on release
PRs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a fine-grained personal
access token for this repository with Contents, Issues, and Pull requests write
permissions, and wait for the release PR's checks before merging.

## Dependency and tool updates

```sh
bun outdated --recursive
bun audit
```

- **Bun:** update `.bun-version` and the Bun image pin in the Dockerfile
  together; CI installs the version in `.bun-version`. File and backup helpers
  reuse the running production image. Native runs use the reviewed
  `FALLBACK_HELPER_IMAGE` pin in `packages/backend/src/runtime-images.ts`; update
  it when the minimum Bun version changes.
- **Pins:** workflows pin every action to a full commit SHA (with its release as
  a comment) and every image to a digest. `scripts/test/security-pins.test.mjs`
  enforces this.
- **Image scans:** [`scripts/ci/containers.mjs`](../scripts/ci/containers.mjs)
  rejects fixable medium, high, and critical vulnerabilities in the production
  runtime and fallback helper.
