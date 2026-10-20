#!/usr/bin/env bash
# Rejects fixable medium, high, and critical vulnerabilities in an image.
# Advisories download first; the analysis then runs offline on an exported copy,
# so the scanner never gets the Docker socket or the network.
set -euo pipefail
image="${1:?usage: scripts/scan-image.sh IMAGE}"
trivy="aquasec/trivy:0.74.0@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969"
temp="${RUNNER_TEMP:?RUNNER_TEMP must be set}"
scan="$(mktemp -d "$temp/ludock-scan-XXXXXX")"
trap 'rm -rf "$scan"' EXIT
cache="$temp/ludock-trivy-cache:/root/.cache/trivy"
docker image inspect "$image" >/dev/null 2>&1 || docker pull "$image"
docker image save --output "$scan/image.tar" "$image"
docker run --rm --volume "$cache" "$trivy" image --download-db-only --no-progress
docker run --rm --network none --volume "$cache" --volume "$scan:/scan:ro" "$trivy" \
  image --input /scan/image.tar --skip-db-update --offline-scan --scanners vuln \
  --severity MEDIUM,HIGH,CRITICAL --ignore-unfixed --exit-code 1 --timeout 8m
