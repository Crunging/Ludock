# Development

## Local development

Install the Go version in [`go.mod`](../go.mod), then run:

```sh
scripts/dev.sh
```

Open <http://127.0.0.1:3000> and use the setup code printed in the output.
`scripts/dev.sh` builds Ludock and `cmd/fakedocker`, an in-memory Docker Engine
API with a running Minecraft server and a stopped Valheim server, and keeps
their data in `data/dev/`. Rerun it after changes. Set `LUDOCK_LISTEN` to use
another address. `fakedocker -demo` seeds the servers shown in the README
screenshots instead.

- **Templates:** pages are [templ](https://templ.guide) components in
  `internal/web/views`. After editing a `.templ` file, run
  `go tool templ generate -path internal/web/views` and commit the generated
  `_templ.go` files; CI fails when they are out of date.
- **Assets:** `internal/web/assets` is embedded into the binary: `ludock.css`,
  the small `ludock.js` (behaviors are `data-*` attributes; no inline
  scripts), page scripts such as `console.js` and `files.js`, and vendored
  htmx, the htmx SSE extension, and xterm.js with their licenses.
- **Real Docker:** set `LUDOCK_DOCKER_SOCKET` to a dedicated test daemon with
  disposable game data, never one running real servers. Native runs need
  `LUDOCK_HELPER_IMAGE` (a Ludock image pinned by digest) for file and backup
  helpers, since there is no running Ludock container to reuse.

## Checks

```sh
gofmt -l cmd internal
go vet ./...
go test ./internal/...                 # focused unit tests
cd e2e && npm ci && npx playwright test # browser journeys
```

The Playwright journeys in `e2e/journeys` are the main suite. They build the real
binary and `cmd/fakedocker`, start both on fresh temporary data, and walk through
the product as users do: signing in, the dashboard, a server's page, backups and
restores, schedules, consoles, files, sharing a server with an operator, account
changes, and the administrator pages. Locally, point them at an installed
Chromium with `LUDOCK_E2E_CHROMIUM=/path/to/chrome`, or run
`npx playwright install chromium` once. `LUDOCK_E2E_LOGS=1` prints the servers'
output and `LUDOCK_E2E_KEEP=1` keeps their temporary data.

Keep Go unit tests for what journeys can't show well: schedule time math,
parsers, tar and path handling, access control and other security checks,
failure and recovery paths, and the database schema.

### Container journeys

`e2e/docker` runs the built image against real Docker: it deploys
[`compose.yaml`](../compose.yaml) with a test override, starts a small Compose
game stack, and checks the hardened runtime, file helpers started from Ludock's
own image, backup and restore, the shell, a Compose update, and account
recovery. Run them only against a disposable daemon:

```sh
docker build --pull --no-cache-filter runtime -t ludock:test .
cd e2e && LUDOCK_DOCKER_E2E=disposable-daemon npx playwright test -c docker.config.mjs
```

CI runs them on native AMD64 and ARM64 runners. Release and CI builds bypass
the runtime stage's cache so `apk upgrade` installs available security fixes
even when the pinned base image is unchanged.

## Builds and releases

The image cross-compiles a static binary (`CGO_ENABLED=0`) and copies it into
Alpine with the Docker CLI and Compose plugin. The binary's subcommands are
`serve` (default), `recover <username>`, `healthcheck`, `helper …` (run inside
helper containers), and `version`.

- **CI:** every PR, including the release PR, and every merge to `main` runs
  formatting, vet, template, and race-enabled unit checks, the journeys, the
  workflow and pin checks, and the container journeys with an image scan on
  AMD64 and ARM64. A weekly workflow runs `govulncheck`, audits the journey
  dependencies, and scans the latest published image.
- **Nightly:** every push to `main` publishes the multi-platform image to
  `ghcr.io` as `nightly`.
- **Releases:** Release Please keeps a release PR open on `main`. Merging it tags
  the version, publishes the GitHub release and changelog, and publishes `X.Y.Z`.
  Stable releases also move `X.Y`, `X`, and `latest`. The version is passed to
  the build and reported by `ludock version`. Release Please owns versions,
  `CHANGELOG.md`, and the release manifest; don't edit them by hand or move
  published tags.

PRs opened with the default `GITHUB_TOKEN` don't trigger CI. For CI on release
PRs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a fine-grained personal
access token for this repository with Contents, Issues, and Pull requests write
permissions, and wait for the release PR's checks before merging.

## Dependency and tool updates

```sh
go list -u -m all
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
cd e2e && npm outdated
```

- **Go:** update `go.mod`'s `go` line and the Go image pin in the Dockerfile
  together. File and backup helpers reuse the running production image, so they
  always match the server.
- **Vendored scripts:** htmx, its SSE extension, and xterm.js are copied into
  `internal/web/assets/vendor` with their licenses; update them deliberately and
  rerun the journeys.
- **Pins:** workflows pin every action to a full commit SHA (with its release as
  a comment) and every image to a digest. `scripts/check-pins.sh` enforces this.
- **Image scans:** [`scripts/scan-image.sh`](../scripts/scan-image.sh) rejects
  fixable medium, high, and critical vulnerabilities.
