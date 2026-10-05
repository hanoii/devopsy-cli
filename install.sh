#!/bin/sh
# Installs the devopsy CLI.
#
#   curl -fsSL https://raw.githubusercontent.com/hanoii/devopsy-cli/main/install.sh | sh
#
# Variables:
#   DEVOPSY_VERSION      Git ref to install (branch or tag). Default: main.
#   DEVOPSY_INSTALL_DIR  Where to put the script. Default: /usr/local/bin when
#                        writable or when running as root, else ~/.local/bin.
set -eu

version=${DEVOPSY_VERSION:-main}
url="https://raw.githubusercontent.com/hanoii/devopsy-cli/$version/devopsy"

if [ -n "${DEVOPSY_INSTALL_DIR:-}" ]; then
  dir=$DEVOPSY_INSTALL_DIR
elif [ "$(id -u)" = 0 ] || [ -w /usr/local/bin ]; then
  dir=/usr/local/bin
else
  dir=$HOME/.local/bin
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$url" -o "$tmp"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$tmp" "$url"
else
  echo "install.sh: curl or wget is required" >&2
  exit 1
fi

# A wrong ref returns an HTML error page or nothing at all.
if ! head -n 1 "$tmp" | grep -q '^#!/bin/sh'; then
  echo "install.sh: $url did not return the devopsy script" >&2
  exit 1
fi

mkdir -p "$dir"
chmod 755 "$tmp"
mv "$tmp" "$dir/devopsy"
trap - EXIT
echo "Installed devopsy ($version) to $dir/devopsy"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Note: $dir is not in your PATH." ;;
esac

if ! docker compose version >/dev/null 2>&1; then
  echo "Note: 'docker compose' is not available. devopsy needs Docker with the Compose plugin."
fi
