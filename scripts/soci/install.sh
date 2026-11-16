#!/usr/bin/env bash
#
# Install the soci CLI and crane into the directory named by $1.
#
# The deploy's join job and the gate's soci job both run this before
# scripts/soci/convert-and-push.sh. soci converts the runner's index into one
# with a SOCI index for each architecture, and crane pushes it with every
# manifest's bytes unchanged (docs/constraints/deploy.md).
#
# Each is one release by number for an x64 runner, checked against its
# sha256 before it is unpacked: the deploy's job holds the deploy role, so an
# unchecked binary would run with it. The pins below are the only ones. A
# caller may set them in the environment, which only the workflows and the
# tests do.
set -euo pipefail

: "${SOCI_VERSION:=0.16.1}"
: "${SOCI_SHA256:=2be9f26ad853455515da69885f8906e6aecc65684cc302f8689c25cad8417bc8}"
: "${CRANE_VERSION:=0.22.1}"
: "${CRANE_SHA256:=0ab7a1d6932a213aed964ce97666c3077fe691c8606413674a8b3e0b9ec4cda0}"

target="${1:?usage: install.sh <directory>}"
mkdir -p "$target"
download=$(mktemp -d)
trap 'rm -rf "$download"' EXIT

# fetch <url> <sha256> <file in the tarball>
fetch() {
  local tarball="${download}/${1##*/}"
  curl -fsSL --retry 3 -o "$tarball" "$1"
  echo "${2}  ${tarball}" | sha256sum -c -
  tar -xzf "$tarball" -C "$target" "$3"
}

fetch "https://github.com/awslabs/soci-snapshotter/releases/download/v${SOCI_VERSION}/soci-snapshotter-${SOCI_VERSION}-linux-amd64.tar.gz" \
  "$SOCI_SHA256" soci
fetch "https://github.com/google/go-containerregistry/releases/download/v${CRANE_VERSION}/go-containerregistry_Linux_x86_64.tar.gz" \
  "$CRANE_SHA256" crane
