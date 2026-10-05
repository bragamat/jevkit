#!/bin/sh
# Installs the jev binary from the GitHub releases of bragamat/jevkit on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/bragamat/jevkit/main/install.sh | sh
#
# JEV_VERSION=0.1.1 pins a version (default: the latest release).
# JEV_INSTALL_DIR=/usr/local/bin changes the target directory (default: ~/.local/bin).
set -eu

repo="bragamat/jevkit"
dir="${JEV_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail "need curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "need sha256sum or shasum to verify the download"
fi

if [ -n "${JEV_VERSION:-}" ]; then
	base="https://github.com/$repo/releases/download/v${JEV_VERSION#v}"
else
	base="https://github.com/$repo/releases/latest/download"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "cannot download $base/checksums.txt"
# The checksums file names the archive, so the latest version needs no API call.
line="$(grep "_${os}_${arch}\.tar\.gz\$" "$tmp/checksums.txt" || true)"
[ -n "$line" ] || fail "no release archive for ${os}/${arch}"
want="${line%% *}"
archive="${line##* }"

echo "Downloading $archive"
fetch "$base/$archive" "$tmp/$archive" || fail "cannot download $base/$archive"
got="$(sha256 "$tmp/$archive")"
[ "$got" = "$want" ] || fail "checksum mismatch for $archive (got $got, want $want)"

tar -xzf "$tmp/$archive" -C "$tmp" jev
mkdir -p "$dir"
# Replace through a temporary name so a running jev is never left half-written.
cp "$tmp/jev" "$dir/.jev.new"
chmod 755 "$dir/.jev.new"
mv -f "$dir/.jev.new" "$dir/jev"
# The binary was called jev-cli before v0.3.0; point an old copy at the new one.
if [ -f "$dir/jev-cli" ] && [ ! -L "$dir/jev-cli" ]; then
  ln -sf jev "$dir/jev-cli"
fi

echo "Installed $("$dir/jev" --version) to $dir/jev"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "Add $dir to your PATH, for example: export PATH=\"$dir:\$PATH\"" ;;
esac
if [ -z "${TYPESAFE_API_KEY:-}" ]; then
	echo "Then set TYPESAFE_API_KEY (get a key at https://console.typesafe.ai)."
fi
