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
  # The hosted E2E job can use upstream releases for its exact tool pins.
  # Other platforms and newer versions keep the normal source install.
  asset=
  if [[ ${DNS_CI_RELEASE_TOOLS:-0} == 1 && $(uname -sm) == 'Linux x86_64' ]]; then
    case "$package@$version" in
      github.com/kyverno/chainsaw@v0.2.15)
        asset="kyverno/chainsaw/releases/download/$version/chainsaw_linux_amd64.tar.gz"
        digest=295d226c89f126c0a97775d364be149f47a810c8a3f9829ee410583d0c1abe3c
        ;;
      sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0)
        asset="kubernetes-sigs/controller-tools/releases/download/$version/controller-gen-linux-amd64"
        digest=5df5d2cced0621d7d8d8040ef20482f5c6e2ced32f1b1ad825f1bdf52f433161
        ;;
      sigs.k8s.io/kustomize/kustomize/v5@v5.7.1)
        asset="kubernetes-sigs/kustomize/releases/download/kustomize/$version/kustomize_${version}_linux_amd64.tar.gz"
        digest=ea375e7372f9aa029129d4b2d16c66b7750b7f1213c4f66f910d981c895818d8
        ;;
    esac
  fi
  if [[ -n $asset ]]; then
    stage=$(mktemp -d "$bin_dir/.install.XXXXXX")
    trap 'rm -rf "$stage"' EXIT
    curl --fail --silent --show-error --location --retry 3 "https://github.com/$asset" -o "$stage/asset"
    printf '%s  %s\n' "$digest" "$stage/asset" | sha256sum -c -
    if [[ $asset == *.tar.gz ]]; then
      tar -xOzf "$stage/asset" "$name" > "$stage/binary"
    else
      mv "$stage/asset" "$stage/binary"
    fi
    chmod 755 "$stage/binary"
    mv "$stage/binary" "$target"
  else
    GOBIN=$bin_dir go install "$package@$version"
    mv "$link" "$target"
  fi
fi
ln -sfn "$target" "$link"
