#!/bin/sh
# Downloads xray-core for every platform the agent targets, verifies its
# SHA2-256 against the release's own .dgst file, and extracts just the
# binary (not geoip.dat/geosite.dat/wintun.dll - internal/checks.VLESSChecker
# doesn't use xray's geo-routing) into internal/checks/embedded/<goos>_<goarch>/.
# Run once before scripts/build-agent.sh's build loop - see
# internal/checks/xray_*.go's //go:embed directives for where these end up
# mattering.
#
# Nothing here runs on a deployed node - see
# docs/superpowers/specs/2026-08-12-vless-speedtest-check-design.md for why
# that's a hard requirement, not a convenience.
#
# Usage:
#   sh scripts/fetch-xray.sh              # fetch all 6 platforms
#   sh scripts/fetch-xray.sh linux amd64  # fetch just one (used by Dockerfile)
set -eu

cd "$(dirname "$0")/.."

XRAY_VERSION="v26.3.27"
BASE_URL="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}"

checksum() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

fetch_one() {
  goos="$1"; goarch="$2"; asset="$3"; binname="$4"
  dir="internal/checks/embedded/${goos}_${goarch}"
  mkdir -p "$dir"
  dest="$dir/$binname"

  if [ -f "$dest" ]; then
    echo "already have $dest, skipping"
    return
  fi

  tmpdir=$(mktemp -d)
  echo "fetching $asset ($goos/$goarch)..."
  curl -sL -o "$tmpdir/$asset" "$BASE_URL/$asset"
  curl -sL -o "$tmpdir/$asset.dgst" "$BASE_URL/$asset.dgst"

  want=$(grep '^SHA2-256=' "$tmpdir/$asset.dgst" | cut -d= -f2 | tr -d ' ')
  got=$(checksum "$tmpdir/$asset")
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $asset: want $want, got $got" >&2
    rm -rf "$tmpdir"
    exit 1
  fi

  unzip -p "$tmpdir/$asset" "$binname" > "$dest"
  chmod +x "$dest"
  rm -rf "$tmpdir"
  echo "-> $dest"
}

target_goos="${1:-}"
target_goarch="${2:-}"

fetch_matching() {
  goos="$1"; goarch="$2"; asset="$3"; binname="$4"
  if [ -n "$target_goos" ]; then
    [ "$goos" = "$target_goos" ] && [ "$goarch" = "$target_goarch" ] || return 0
  fi
  fetch_one "$goos" "$goarch" "$asset" "$binname"
}

fetch_matching windows amd64 Xray-windows-64.zip      xray.exe
fetch_matching windows 386   Xray-windows-32.zip      xray.exe
fetch_matching linux   amd64 Xray-linux-64.zip         xray
fetch_matching linux   arm64 Xray-linux-arm64-v8a.zip  xray
fetch_matching darwin  amd64 Xray-macos-64.zip         xray
fetch_matching darwin  arm64 Xray-macos-arm64-v8a.zip  xray

echo "done: internal/checks/embedded/"
