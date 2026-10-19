#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
# Pass --race to run the same complete suite with the race detector.
TEST_FLAGS=(-count=1 -timeout=240s)
if [[ "${1:-}" == "--race" ]]; then
  TEST_FLAGS+=(-race)
  shift
fi
for module in core cli; do
  (
    cd "$ROOT_DIR/$module"
    GOWORK=off go test "${TEST_FLAGS[@]}" "$@" ./...
  )
done
