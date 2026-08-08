#!/bin/sh
# Cross-compiles the node agent for every OS the architecture targets:
# Windows/Linux/macOS, on amd64 and arm64 (plus windows/386 for 32-bit Windows).
# Output goes to bin/.
set -eu

cd "$(dirname "$0")/.."
mkdir -p bin

build() {
  os="$1"; arch="$2"; ext="$3"
  out="bin/pingachock-agent-${os}-${arch}${ext}"
  echo "building $out"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -o "$out" ./cmd/agent
}

build linux   amd64 ""
build linux   arm64 ""
build darwin  amd64 ""
build darwin  arm64 ""
build windows amd64 ".exe"
build windows 386   ".exe"

echo "done: bin/"
