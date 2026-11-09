#!/bin/sh
# Install bbctl from the latest GitHub release (Linux and macOS; Windows users
# download the .zip from the releases page, see README).
#
#   curl -fsSL https://backedblock.io/install.sh | sh
#
# BBCTL_VERSION=v0.1.0 pins a release; BBCTL_INSTALL_DIR overrides the target
# (default /usr/local/bin if writable, else ~/.local/bin).
set -eu

REPO=migrx-io/backedblock
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  mingw*|msys*|cygwin*)
    echo "install.sh supports Linux and macOS. On Windows, download bbctl_windows_${arch}.zip from" >&2
    echo "https://github.com/$REPO/releases/latest and put bbctl.exe on your PATH." >&2
    exit 1 ;;
  *) echo "unsupported OS: $os (download a release from https://github.com/$REPO/releases)" >&2; exit 1 ;;
esac

if [ -n "${BBCTL_VERSION:-}" ]; then
  base="https://github.com/$REPO/releases/download/$BBCTL_VERSION"
else
  base="https://github.com/$REPO/releases/latest/download"
fi

dir=${BBCTL_INSTALL_DIR:-}
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive="bbctl_${os}_${arch}.tar.gz"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
(cd "$tmp" && grep " $archive\$" checksums.txt | { command -v sha256sum >/dev/null && sha256sum -c - || shasum -a 256 -c -; } >/dev/null)
tar -xzf "$tmp/$archive" -C "$tmp" bbctl
install -m 0755 "$tmp/bbctl" "$dir/bbctl"

echo "installed $dir/bbctl"
"$dir/bbctl" version

# Not on PATH: print the line for the user's shell rather than edit their rc file.
case ":$PATH:" in *":$dir:"*) exit 0 ;; esac
case "$dir" in "$HOME"/*) pdir="\$HOME/${dir#"$HOME"/}" ;; *) pdir=$dir ;; esac
case "$(basename "${SHELL:-sh}")" in
  zsh) rc="~/.zshrc"; line="export PATH=\"$pdir:\$PATH\"" ;;
  bash) if [ "$os" = darwin ]; then rc="~/.bash_profile"; else rc="~/.bashrc"; fi
        line="export PATH=\"$pdir:\$PATH\"" ;;
  fish) rc="~/.config/fish/config.fish"; line="fish_add_path $pdir" ;;
  *) rc="~/.profile"; line="export PATH=\"$pdir:\$PATH\"" ;;
esac
echo
echo "$dir is not on your PATH. Add it with:"
echo
echo "  echo '$line' >> $rc"
echo
echo "then open a new terminal, or run bbctl as $dir/bbctl until then."
