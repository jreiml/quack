#!/bin/sh
set -eu

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "quack: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
case $os in
  darwin | linux) ;;
  *) echo "quack: unsupported OS $os" >&2; exit 1 ;;
esac

dir=${QUACK_INSTALL_DIR:-$HOME/.local/bin}
url=https://github.com/jreiml/quack/releases/latest/download/quack_${os}_${arch}.tar.gz
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

curl -fsSL "$url" | tar -xz -C "$tmp" quack
mkdir -p "$dir"
mv "$tmp/quack" "$dir/quack"
echo "installed $dir/quack"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "add $dir to your PATH" ;;
esac
command -v tmux >/dev/null || echo "quack needs tmux 3.3 or newer: brew install tmux / apt install tmux"
