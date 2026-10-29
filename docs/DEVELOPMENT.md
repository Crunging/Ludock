# Development

## Local development

Install the Go version in [`go.mod`](../go.mod), then run:

```sh
scripts/dev.sh
```

Open <http://127.0.0.1:3000> and use the setup code printed in the output.
The script builds Ludock and `cmd/fakedocker`, an in-memory Docker Engine API
with a running Minecraft server and a stopped Valheim server, and keeps their
data in `data/dev/`. Rerun it after changes, and set `LUDOCK_LISTEN` for another
address. `fakedocker -demo` seeds the servers shown in the README screenshots.

- **Templates:** pages are [templ](https://templ.guide) components in
  `internal/web/views`. After editing a `.templ` file, run
  `go tool templ generate -path internal/web/views` and commit the generated
  `_templ.go` files; CI fails when they are out of date.
- **Assets:** `internal/web/assets` is embedded into the binary: `ludock.css`,
  `ludock.js` (behaviors are `data-*` attributes; pages run no inline scripts),
  page scripts such as `console.js` and `files.js`, and vendored htmx, its SSE
  extension, and xterm.js with their licenses.
- **Real Docker:** set `LUDOCK_DOCKER_SOCKET` to a dedicated test daemon with
  disposable game data, never one running real servers. Native runs also need
  `LUDOCK_HELPER_IMAGE`, a Ludock image pinned by digest, for file and backup
  helpers.

## Checks

```sh
gofmt -l cmd internal
go vet ./...
go tool staticcheck ./...
go test ./internal/...
cd e2e && npm ci && npx playwright test
```

The Playwright journeys in `e2e/journeys` are the main suite. They build the
real binary and `cmd/fakedocker`, start one of each on fresh temporary data for
every worker, and use the product as people do. Add or extend a journey for
anything a user can reach in the browser; Go unit tests are for what a journey
can't reach. Workers run whole files, so a file must pass on a fresh server
without the files before it, and a file whose journeys each pass on their own
can set `test.describe.configure({ mode: "parallel" })`. `--workers=1` runs
every file on one server. CI retries a failed journey once to show whether it's
flaky, but a flaky journey still fails the run.

Locally, point the journeys at an installed Chromium with
`LUDOCK_E2E_CHROMIUM=/path/to/chrome`, or run `npx playwright install chromium`
once. `LUDOCK_E2E_LOGS=1` prints the servers' output and `LUDOCK_E2E_KEEP=1`
keeps their temporary data.

### Container journeys

`e2e/docker` runs the built image against real Docker. It deploys
[`compose.yaml`](../compose.yaml) with a test override, starts a small Compose
game stack, and checks the hardened runtime, file helpers started from Ludock's
own image, backup and restore, the shell, an image update, and account
recovery. Run them only against a disposable daemon:

```sh
docker build --pull --no-cache-filter runtime -t ludock:test .
cd e2e && LUDOCK_DOCKER_E2E=disposable-daemon npx playwright test -c docker.config.mjs
```

## CI and releases

The image cross-compiles a static binary (`CGO_ENABLED=0`) and copies it into
Alpine. The binary's subcommands are
`serve` (the default), `recover <username>`, `healthcheck`, `helper …` (run
inside helper containers), and `version`.

- **CI** runs on every PR and every push to `main` that changes more than
  Markdown or the license: formatting, vet, generated templates, race-enabled
  Go tests, the journeys, the pin check, the container journeys on AMD64 and
  ARM64 (one job per journey file), and an image scan. Runs reuse the Go
  build cache and image layers from `main`'s latest run, except the runtime
  stage, so `apk upgrade` picks up security fixes.
- **Weekly:** `govulncheck`, `npm audit` for the journey dependencies, and a
  scan of the latest published image.
- **Nightly:** every push to `main` publishes the image to `ghcr.io` as
  `nightly`.
- **Releases:** Release Please keeps a release PR open on `main`. Merging it
  tags the version, publishes the GitHub release and changelog, and publishes
  `X.Y.Z`; stable releases also move `X.Y`, `X`, and `latest`. Don't edit
  versions, `CHANGELOG.md`, or the release manifest by hand, or move published
  tags.

PRs opened with the default `GITHUB_TOKEN` don't trigger CI. For CI on release
PRs, set the Actions secret `RELEASE_PLEASE_TOKEN` to a fine-grained token for
this repository with Contents, Issues, and Pull requests write access.

## Dependency updates

```sh
go list -u -m all
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
(cd e2e && npm outdated)
(cd third_party/browser && npm outdated)
```

- **Go:** update `go.mod`'s `go` line and the Go image digest in the Dockerfile
  together. Helpers reuse the running Ludock image, so they always match it.
- **Vendored scripts:** htmx, its SSE extension, and xterm.js live in
  `internal/web/assets/vendor` with their licenses. Each file is an exact copy
  of the published file `third_party/browser/package.json` maps it to, so the
  security scan can audit those versions. To update one, change its version
  there, run `npm install --package-lock-only` in that folder, copy the new
  file from the package, run `scripts/check-vendor.sh`, and rerun the
  journeys.
- **Pins:** workflows pin every action to a commit SHA (with its release as a
  comment) and every image to a digest; `scripts/check-pins.sh` enforces this.
- **Image scans:** [`scripts/scan-image.sh`](../scripts/scan-image.sh) rejects
  fixable medium, high, and critical vulnerabilities.
