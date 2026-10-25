#!/usr/bin/env bash
# Every browser library in internal/web/assets/vendor is an exact copy of the
# published file third_party/browser/package.json maps it to, so auditing
# those versions audits what Ludock serves.
set -euo pipefail
cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp third_party/browser/package.json third_party/browser/package-lock.json "$work/"
(cd "$work" && npm ci --ignore-scripts --no-audit --no-fund --silent)
status=0
listed="$(node -e 'for (const [copy, source] of Object.entries(require(process.argv[1]).vendored)) console.log(copy + "\t" + source)' "$work/package.json")"
while IFS=$'\t' read -r copy source; do
  if ! cmp -s "internal/web/assets/vendor/$copy" "$work/node_modules/$source"; then
    echo "Not a copy of $source: internal/web/assets/vendor/$copy"
    status=1
  fi
done <<<"$listed"
for file in internal/web/assets/vendor/*; do
  name="$(basename "$file")"
  [[ "$name" == LICENSE.* ]] && continue
  if ! cut -f1 <<<"$listed" | grep -qxF "$name"; then
    echo "Not listed in third_party/browser/package.json: $file"
    status=1
  fi
done
exit $status
