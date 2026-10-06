#!/usr/bin/env bash
set -euo pipefail

# Test script for knot-core

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

echo "Running tests for knot-core..."

cd "$ROOT_DIR/core"
GOWORK=off go test ./... -v

echo "✓ All tests passed"
