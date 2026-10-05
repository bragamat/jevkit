#!/usr/bin/env bash
# Fetches a pinned, checksum-verified Go toolchain into a cache dir if none is on
# PATH, then checks tidiness and formatting, vets, tests and builds. Used on build
# hosts without Go installed. TIDY=fix rewrites go.mod/go.sum instead of failing;
# LINT=1 also runs golangci-lint and govulncheck.
set -euo pipefail
GO_VERSION="${GO_VERSION:-1.27.1}"
GO_SHA256="${GO_SHA256:-63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445}"
if ! command -v go >/dev/null; then
  root="${GO_CACHE_ROOT:-$HOME/.cache/jevkit-go}/go$GO_VERSION"
  if [ ! -x "$root/go/bin/go" ]; then
    mkdir -p "$root"
    tarball="$root/go.tar.gz"
    curl -fsSL -o "$tarball" "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
    echo "$GO_SHA256  $tarball" | sha256sum -c --quiet -
    tar -xzf "$tarball" -C "$root" && rm "$tarball"
  fi
  export PATH="$root/go/bin:$PATH"
fi
go version
if [ "${TIDY:-check}" = fix ]; then go mod tidy; else go mod tidy -diff; fi
test -z "$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files above need formatting"; exit 1; }
go vet ./...
if command -v gcc >/dev/null; then go test -race -shuffle=on -count=1 ./...; else go test -shuffle=on -count=1 ./...; fi
if [ "${LINT:-0}" = 1 ]; then
  export PATH="$(go env GOPATH)/bin:$PATH"
  command -v golangci-lint >/dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
  golangci-lint run ./...
  go run golang.org/x/vuln/cmd/govulncheck@latest ./...
fi
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION:-}" -o dist/jev-cli ./cmd/jev-cli
ls -la dist/jev-cli
