#!/usr/bin/env bash
# CI runs a job's steps one at a time. `start NAME COMMAND...` runs a command
# alongside the steps after it; `wait NAME` prints its output and exits with
# its status.
set -euo pipefail
dir="${RUNNER_TEMP:?RUNNER_TEMP must be set}/background"
mkdir -p "$dir"
case "${1:-}" in
  start)
    name="${2:?usage: scripts/background.sh start NAME COMMAND...}"
    shift 2
    (
      status=0
      "$@" || status=$?
      echo "$status" > "$dir/$name.tmp" && mv "$dir/$name.tmp" "$dir/$name.status"
    ) > "$dir/$name.log" 2>&1 < /dev/null &
    ;;
  wait)
    name="${2:?usage: scripts/background.sh wait NAME}"
    until [[ -f "$dir/$name.status" ]]; do sleep 1; done
    cat "$dir/$name.log"
    exit "$(cat "$dir/$name.status")"
    ;;
  *)
    echo "usage: scripts/background.sh start NAME COMMAND... | wait NAME" >&2
    exit 2
    ;;
esac
