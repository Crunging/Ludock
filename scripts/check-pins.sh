#!/usr/bin/env bash
# Workflows pin every action to a full commit SHA and every image to a digest.
set -euo pipefail
cd "$(dirname "$0")/.."
status=0
while IFS= read -r line; do
  ref="$(sed -E 's/.*uses:[[:space:]]*//; s/[[:space:]]+#.*//' <<<"$line")"
  if [[ "$ref" == docker://* ]]; then
    [[ "$ref" =~ @sha256:[0-9a-f]{64}$ ]] && continue
  elif [[ "$ref" =~ @[0-9a-f]{40}$ ]]; then
    continue
  fi
  echo "Unpinned: $line"
  status=1
done < <(grep -Hn "uses:" .github/workflows/*.yaml)
while IFS= read -r line; do
  [[ "$line" =~ @sha256:[0-9a-f]{64} ]] && continue
  echo "Unpinned: $line"
  status=1
done < <(grep -Hn -E "^ARG [A-Z_]+_IMAGE=|^FROM " Dockerfile | grep -v -E "FROM (--platform=\S+ )?\\\$\{" )
grep -q 'aquasec/trivy:[^@]*@sha256:[0-9a-f]\{64\}' scripts/scan-image.sh || { echo "Unpinned: scanner image"; status=1; }
exit $status
