#!/usr/bin/env bash
set -euo pipefail

# Build both independent modules with the same release metadata.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
BIN_DIR="${ROOT_DIR}/bin"

# Version information
VERSION="${VERSION:-0.1.0-dev}"
COMMIT="${COMMIT:-$(git -C "$ROOT_DIR" rev-parse --short HEAD 2>/dev/null || echo 'unknown')}"
BUILD_TIME="${BUILD_TIME:-$(date -u '+%Y-%m-%d_%H:%M:%S')}"
DIRTY="${DIRTY:-$(if [[ -z "$(git -C "$ROOT_DIR" status --porcelain 2>/dev/null)" ]]; then echo 'false'; else echo 'true'; fi)}"

# Create bin directory
mkdir -p "$BIN_DIR"

echo "Building knot-core and knot..."
echo "  Version: $VERSION"
echo "  Commit: $COMMIT"
echo "  Time: $BUILD_TIME"
echo "  Dirty: $DIRTY"

# Native output and cross-build output share the same release metadata.
build_target() {
  local target_os="$1" target_arch="$2" suffix="$3" target_name="$4"
  (
    cd "$ROOT_DIR/core"
    GOWORK=off CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
      -ldflags="-s -w -X knot-core/pkg/core.DefaultVersion=$VERSION -X knot-core/pkg/core.BuildCommit=$COMMIT -X knot-core/pkg/core.BuildTime=$BUILD_TIME -X knot-core/pkg/core.BuildDirty=$DIRTY" \
      -o "$BIN_DIR/knot-core$target_name$suffix" ./cmd/core
  )
  (
    cd "$ROOT_DIR/cli"
    GOWORK=off CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
      -ldflags="-s -w -X knot-cli/internal/buildinfo.Version=$VERSION -X knot-cli/internal/buildinfo.Commit=$COMMIT -X knot-cli/internal/buildinfo.BuildTime=$BUILD_TIME -X knot-cli/internal/buildinfo.Dirty=$DIRTY" \
      -o "$BIN_DIR/knot$target_name$suffix" .
  )
  echo "Built: $BIN_DIR/knot-core$target_name$suffix"
  echo "Built: $BIN_DIR/knot$target_name$suffix"
}
if [[ "${1:-}" == "--all" ]]; then
  for target_os in linux darwin windows; do
    for target_arch in amd64 arm64; do
      suffix=""
      if [[ "$target_os" == "windows" ]]; then suffix=".exe"; fi
      build_target "$target_os" "$target_arch" "$suffix" "-$target_os-$target_arch"
    done
  done
else
  target_os="$(go env GOOS)"
  target_arch="$(go env GOARCH)"
  suffix=""
  if [[ "$target_os" == "windows" ]]; then suffix=".exe"; fi
  build_target "$target_os" "$target_arch" "$suffix" ""
fi
