#!/usr/bin/env bash
# Fetches a pinned Go toolchain into a cache dir if none is on PATH, then
# tidies, vets, tests and builds. Used on build hosts without Go installed.
set -euo pipefail
GO_VERSION="${GO_VERSION:-1.27.1}"
if ! command -v go >/dev/null; then
  root="${GO_CACHE_ROOT:-$HOME/.cache/jevkit-go}/go$GO_VERSION"
  if [ ! -x "$root/go/bin/go" ]; then
    mkdir -p "$root"
    curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" | tar -xz -C "$root"
  fi
  export PATH="$root/go/bin:$PATH"
fi
go version
go mod tidy
test -z "$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files above need formatting"; exit 1; }
go vet ./...
if command -v gcc >/dev/null; then go test -race -count=1 ./...; else go test -count=1 ./...; fi
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION:-dev}" -o dist/jev-cli ./cmd/jev-cli
ls -la dist/jev-cli
