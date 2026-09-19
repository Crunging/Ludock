# Development and CI

## Local development

Install the Bun version in [`.bun-version`](../.bun-version), then run:

```sh
bun install --frozen-lockfile
bun run dev
```

Use the printed URLs and state path. `bun run dev --print-config` shows the
configuration without starting a backend. Development has no Docker connection
by default; use a dedicated test daemon with disposable game data.

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

Code PRs run the full CI suite. Release PRs validate the generated version,
manifest, and changelog, then build and smoke-test the image. Publish runs the
full suite on native AMD64 and ARM64 before publication. Release Please owns
versions and the release manifest; preserve published tags.

For automatic release PR runs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a
fine-grained personal access token restricted to this repository, with Contents
and Pull requests write permissions. Without it, GitHub requires run approval.
Review the refreshed release PR and wait for its latest checks before merging.

## Tool updates

```sh
bun outdated --recursive
bun audit
```

Update `.bun-version` and Bun image pins in the Dockerfile and local actions
together. File and backup helpers reuse the running production image automatically.
Native runs use the reviewed `FALLBACK_HELPER_IMAGE` pin in
`packages/backend/src/runtime-images.ts`; update it when the Bun minimum changes.
Keep [integration revisions](../scripts/ci/integration.mjs)
and [CI images](../scripts/ci/containers.mjs) pinned and validate both architectures.
Image checks reject fixable medium, high, and critical vulnerabilities in
the production runtime, fallback helper, and CI action runtime.
