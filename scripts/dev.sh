#!/usr/bin/env bash
# Runs Ludock against the in-memory fake Docker daemon on fresh-or-kept data in
# data/dev. It never talks to a real Docker daemon. Rerun after changes; run
# `go tool templ generate -path internal/web/views` after editing templates.
set -euo pipefail
cd "$(dirname "$0")/.."
dir="$PWD/data/dev"
mkdir -p "$dir/bin"
go tool templ generate -path internal/web/views >/dev/null
CGO_ENABLED=0 go build -o "$dir/bin/" ./cmd/ludock ./cmd/fakedocker
"$dir/bin/fakedocker" -dir "$dir/docker" -backups "$dir/backups" >"$dir/fakedocker.log" 2>&1 &
fake=$!
trap 'kill $fake 2>/dev/null' EXIT
until [ -S "$dir/docker/docker.sock" ]; do sleep 0.1; done
export LUDOCK_LISTEN="${LUDOCK_LISTEN:-127.0.0.1:3000}"
export LUDOCK_DATA_DIR="$dir/data"
export LUDOCK_DOCKER_SOCKET="$dir/docker/docker.sock"
export LUDOCK_SELF_CONTAINER=ludock-dev
export LUDOCK_BACKUP_ROOTS="$dir/backups"
export LUDOCK_HELPER_IMAGE="ghcr.io/crunging/ludock@sha256:$(printf 'ab%.0s' {1..32})"
echo "Ludock: http://$LUDOCK_LISTEN (setup code below on first start)"
"$dir/bin/ludock" serve
