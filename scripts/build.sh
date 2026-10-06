#!/usr/bin/env bash
set -euo pipefail

# Build script for knot-core

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="${ROOT_DIR}/bin"

# Version information
VERSION="${VERSION:-0.1.0-dev}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"
BUILD_TIME="${BUILD_TIME:-$(date -u '+%Y-%m-%d_%H:%M:%S')}"
DIRTY="${DIRTY:-$(if git diff-index --quiet HEAD -- 2>/dev/null; then echo 'false'; else echo 'true'; fi)}"

# Create bin directory
mkdir -p "$BIN_DIR"

echo "Building knot-core..."
echo "  Version: $VERSION"
echo "  Commit: $COMMIT"
echo "  Time: $BUILD_TIME"
echo "  Dirty: $DIRTY"

# Build for current platform
cd "$ROOT_DIR/core"
GOWORK=off CGO_ENABLED=0 go build \
  -ldflags="-s -w -X knot-core/pkg/core.DefaultVersion=$VERSION -X knot-core/pkg/core.BuildCommit=$COMMIT -X knot-core/pkg/core.BuildTime=$BUILD_TIME -X knot-core/pkg/core.BuildDirty=$DIRTY" \
  -o "$BIN_DIR/knot-core" \
  ./cmd/core

echo "✓ Built: $BIN_DIR/knot-core"
