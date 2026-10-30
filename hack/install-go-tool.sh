#!/usr/bin/env bash
# Install a Go tool as BIN_DIR/NAME-VERSION with BIN_DIR/NAME symlinked to it,
# so a change to the pinned version replaces the tool on the next run.
set -euo pipefail

if [ $# -ne 4 ]; then
  echo "usage: $0 BIN_DIR NAME PACKAGE VERSION" >&2
  exit 2
fi
bin_dir=$1
name=$2
package=$3
version=$4

mkdir -p "$bin_dir"
bin_dir=$(cd "$bin_dir" && pwd)
target="$bin_dir/$name-$version"
link="$bin_dir/$name"

if [ ! -x "$target" ]; then
  echo "Installing $package@$version into $target"
  rm -f "$link"
  GOBIN=$bin_dir go install "$package@$version"
  mv "$link" "$target"
fi
ln -sfn "$target" "$link"
