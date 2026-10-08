#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$ROOT_DIR/core"

# Use the selected Go toolchain for both formatting and static analysis.
# An older prebuilt golangci-lint cannot analyze this module's Go version.
go version
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "Go files requiring gofmt:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi

GOWORK=off go vet ./...
