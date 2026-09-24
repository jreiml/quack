#!/bin/sh
set -eu

say() { echo "quack: $*"; }
die() { echo "quack: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m)" ;;
esac
case $os in
  darwin | linux) ;;
  *) die "unsupported OS $os" ;;
esac
say "detected $os/$arch"

dir=${QUACK_INSTALL_DIR:-$HOME/.local/bin}
url=https://github.com/jreiml/quack/releases/latest/download/quack_${os}_${arch}.tar.gz
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading $url"
curl -fsSL "$url" | tar -xz -C "$tmp" quack
say "installing to $dir/quack"
mkdir -p "$dir"
mv "$tmp/quack" "$dir/quack"
say "installed $dir/quack"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "add $dir to your PATH" ;;
esac

sudo=
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null; then
  sudo=sudo
fi

run() {
  say "running: $*"
  "$@"
}

install_tmux() {
  if command -v brew >/dev/null; then
    run brew install tmux
  elif command -v apt-get >/dev/null; then
    run $sudo apt-get update
    run $sudo apt-get install -y tmux
  elif command -v dnf >/dev/null; then
    run $sudo dnf install -y tmux
  elif command -v yum >/dev/null; then
    run $sudo yum install -y tmux
  elif command -v pacman >/dev/null; then
    run $sudo pacman -S --noconfirm tmux
  elif command -v apk >/dev/null; then
    run $sudo apk add tmux
  elif command -v zypper >/dev/null; then
    run $sudo zypper install -y tmux
  else
    die "no supported package manager found; install tmux 3.3 or newer yourself"
  fi
}

say "checking for tmux"
if command -v tmux >/dev/null; then
  say "found $(tmux -V)"
else
  say "tmux not found, installing it"
  install_tmux
  say "installed $(tmux -V)"
fi

version=$(tmux -V | sed -E 's/^tmux (next-)?([0-9]+)\.([0-9]+).*/\2 \3/')
set -- $version
if [ "$1" -lt 3 ] || { [ "$1" -eq 3 ] && [ "$2" -lt 3 ]; }; then
  say "warning: quack needs tmux 3.3 or newer, you have $(tmux -V)"
fi

say "done, run 'quack new' to start"
