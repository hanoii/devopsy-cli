#!/bin/sh
# Installs the devopsy CLI from its GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
#
# Variables:
#   DEVOPSY_VERSION      Release tag, like v0.2.0. Default: latest ("main" also
#                        means latest, for older setups).
#   DEVOPSY_INSTALL_DIR  Where to put the binary. Default: /usr/local/bin when
#                        writable or when running as root, else ~/.local/bin.
set -eu

repo=https://github.com/hanoii/devopsy-cli

die() {
  echo "install.sh: $*" >&2
  exit 1
}

version=${DEVOPSY_VERSION:-latest}
case $version in
  latest | main) base=$repo/releases/latest/download ;;
  *) base=$repo/releases/download/$version ;;
esac

case $(uname -s) in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "unsupported OS: $(uname -s)" ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
archive=devopsy_${os}_${arch}.tar.gz

if [ -n "${DEVOPSY_INSTALL_DIR:-}" ]; then
  dir=$DEVOPSY_INSTALL_DIR
elif [ "$(id -u)" = 0 ] || [ -w /usr/local/bin ]; then
  dir=/usr/local/bin
else
  dir=$HOME/.local/bin
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$2" "$1"
  else
    die "curl or wget is required"
  fi
}

fetch "$base/$archive" "$tmp/$archive" || die "could not download $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download $base/checksums.txt"

expected=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || die "$archive is not in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" devopsy
mkdir -p "$dir"
# Through a temporary name, so a running devopsy is never half-written.
cp "$tmp/devopsy" "$dir/.devopsy.new"
chmod 755 "$dir/.devopsy.new"
mv "$dir/.devopsy.new" "$dir/devopsy"
echo "Installed $("$dir/devopsy" version | head -n 1) to $dir/devopsy"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Note: $dir is not in your PATH." ;;
esac

if ! docker compose version >/dev/null 2>&1; then
  echo "Note: 'docker compose' is not available. devopsy needs Docker with the Compose plugin."
fi
