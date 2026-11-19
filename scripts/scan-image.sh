#!/usr/bin/env bash
# Rejects fixable medium, high, and critical vulnerabilities in an image.
# Advisories download first; the analysis then runs offline on an exported copy,
# so the scanner never gets the Docker socket or the network. `--prepare` only
# downloads the scanner and advisories, so CI can do that while it builds.
set -euo pipefail
image="${1:?usage: scripts/scan-image.sh IMAGE | --prepare}"
trivy="aquasec/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb"
temp="${RUNNER_TEMP:?RUNNER_TEMP must be set}"
cache="$temp/ludock-trivy-cache:/root/.cache/trivy"
docker run --rm --volume "$cache" "$trivy" image --download-db-only --no-progress
if [[ "$image" == --prepare ]]; then
  exit 0
fi
scan="$(mktemp -d "$temp/ludock-scan-XXXXXX")"
trap 'rm -rf "$scan"' EXIT
docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
docker image save --output "$scan/image.tar" "$image"
docker run --rm --network none --volume "$cache" --volume "$scan:/scan:ro" "$trivy" \
  image --input /scan/image.tar --skip-db-update --offline-scan --scanners vuln \
  --severity MEDIUM,HIGH,CRITICAL --ignore-unfixed --exit-code 1 --timeout 8m
