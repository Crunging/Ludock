# Development and CI

## Local development

Install the exact Bun version in [`.bun-version`](../.bun-version), then run:

```sh
bun install --frozen-lockfile
bun run dev
```

Use the printed URLs and state path. `bun run dev --print-config` displays the
configuration without starting a backend. Development has no Docker connection
by default; connect only to a dedicated test daemon with disposable game data.
`bun run dev`, `bun run check`, CI setup, and Docker builds verify the pin.

All JavaScript executes with Bun. The `[run].bun = true` setting also applies to
package tools with Node shebangs. TypeScript 7 and Oxlint use their native
executables. There is no requirement to install Node, npm, or npx.

## Runtime boundaries

Use Bun and Web APIs for networking, streams, hashing, binary data, and ordinary
file I/O. Bun implements the remaining `node:fs`, `node:path`, and `node:os` APIs
itself. Those names do not launch Node. In particular, directory descriptors,
no-follow opens, ownership, permissions, atomic renames, and filesystem syncing
protect backups, file access, and Compose sources. Keep these built-in APIs
instead of maintaining a custom filesystem layer.

Bun's type definitions depend on `@types/node`; that package contains declarations,
not a runtime. Third-party tools may also use Node-compatible interfaces while
executing with Bun.

Backup archives use bounded Web Stream reads and a Bun file writer. The archive
reader preserves ownership and timestamp metadata and never extracts paths itself.
The restore helper still confines every filesystem operation through pinned
directories. `Bun.Archive` is useful for smaller in-memory archives, but its file
API does not provide the streaming metadata contract these backups require.

## Production bundles

`bun run --filter @ludock/backend build` bundles the server, account recovery
command, shared contracts, and their dependencies. Keep every file in
`packages/backend/dist` together: the entry points share generated chunks and
linked source maps. The production image runs these bundles without installed
`node_modules` or workspace sources. Development continues to run source files.

The build writes `dependencies.cdx.json`, a CycloneDX inventory of packages that
contributed code, and `THIRD-PARTY-NOTICES.txt` with their license notices. Trivy
discovers the inventory inside the image, so bundled dependencies remain visible
to image vulnerability scans. Published attestations also include the backend
build stage's installed inputs. Source CI audits the complete Bun lockfile.

## Checks

```sh
bun run check
bun run --filter @ludock/frontend build
bun run --filter @ludock/frontend test:e2e
```

The regular lint command includes repository build, development, CI, acceptance,
and benchmark scripts. Run just those checks with `bun run lint:scripts`.

Docker acceptance uses the `ludock:test` image and `scripts/test-linux.mjs`,
`scripts/test-files.mjs`, `scripts/test-backups.mjs`, `scripts/test-packaged.mjs`,
and `scripts/test-compose.mjs`.
Run them sequentially against a dedicated daemon. Validate on Linux AMD64 and
ARM64, and report when an architecture was emulated. The Linux harness verifies
that the production image contains no Node, npm, or npx executable on PATH.
It checks frontend assets, administrator setup, account recovery, and session
revocation before mounting source files and dependencies for the Linux suites.
The source mounts are test fixtures and are never included in the production image.
The packaged harness uses the image's normal command without source mounts and
checks binary upload/download, archive checksums, backup restoration, safety
backups, and preservation of initially running and stopped servers through HTTP.
It owns fresh game and backup volumes and removes them when finished.

Optional benchmark commands emit JSON measurements for a PR or CI artifact:

```sh
bun scripts/benchmarks/sources.mjs transfer /path/to/baseline "$PWD"
bun scripts/benchmarks/sources.mjs archive /path/to/baseline "$PWD"
bun scripts/benchmarks/startup.mjs ludock:baseline ludock:test linux/arm64
```

Use the same Bun release for both variants and install each source checkout's
locked dependencies. Run archive comparisons on Linux and build both images
before measuring startup. Use an idle machine and record whether it was emulated.

## CI and release tools

Workflows check out their exact commit with Git and temporary authentication;
credentials are not saved in the checkout. Bun is copied from a pinned official
image and checked against the exact version in `.bun-version` and the minimum
version in `package.json`. Buildx and Trivy run as native tools with pinned
container images.

Image scans export a local image and analyze that archive offline in Trivy.
Advisory downloads run separately without access to the image. The scanner never
receives the Docker socket, and temporary exports are removed after each scan.

Release Please and artifact uploads use their maintained upstream bundles and
companion assets from pinned revisions under Bun in the
[local container action](../.github/actions/bun-action/action.yaml).
Release Please continues to own versions and the release manifest. The Publish
workflow preserves immutable version tags and updates moving tags only when the
release owns them. The `node` release type in `release-please-config.json` selects
the package-version format; it does not select a JavaScript executable.

Container actions receive GitHub's artifact/cache credentials. The Bun action
exports masked cache credentials for the native Buildx steps and keeps browser
failure artifacts with seven-day retention. JavaScript actions that launch Node
are rejected by the CI policy tests.
Source CI also uploads a small receipt on successful runs and exercises the
unmodified Release Please bundle against a fixture GitHub API. It verifies
changelog rendering, package/manifest versions, and release PR generation without
accessing a live repository or using a real token.

When updating Bun, update `.bun-version` and its image pins in the Dockerfile, helper configuration,
and local actions together. Check the immutable bundle revisions in
[`scripts/ci/integration.mjs`](../scripts/ci/integration.mjs) and image digests in
[`scripts/ci/containers.mjs`](../scripts/ci/containers.mjs) during dependency work.
Run `bun outdated --recursive` and `bun audit`, and validate both architectures.
