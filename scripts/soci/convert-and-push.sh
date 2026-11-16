#!/usr/bin/env bash
#
# Convert an image index into one with a SOCI index manifest v2 for each
# image, and push it.
#
#   convert-and-push.sh <source> <destination> <work directory>
#
# <source> names the index to convert, as registry/repository@digest, and
# <destination> the tag to push the converted index to, as
# registry/repository:tag. The deploy runs this on the runner's joined index
# in ECR, and the gate's soci job on an index in a registry of its own, so a
# pull request runs the real tools on the same commands
# (docs/constraints/deploy.md). It needs soci and crane from
# scripts/soci/install.sh, and skopeo and jq.
#
# The caller logs in first: skopeo reads REGISTRY_AUTH_FILE when it is set,
# and crane reads Docker's config. TLS_VERIFY=false lets skopeo pull from a
# registry over plain http, which only the gate's own registry needs.
set -euo pipefail

source="${1:?usage: convert-and-push.sh <source> <destination> <work directory>}"
destination="${2:?usage: convert-and-push.sh <source> <destination> <work directory>}"
work="${3:?usage: convert-and-push.sh <source> <destination> <work directory>}"

# soci convert --standalone reads and writes an OCI image layout on disk and
# needs no containerd. skopeo pulls the index and every image in it to disk,
# and fails rather than rewrite a manifest: each SOCI index names its image's
# digest, so a rewritten image would lose its index.
skopeo copy --all --preserve-digests --src-tls-verify="${TLS_VERIFY:-true}" "docker://${source}" "oci:${work}/gzip"
soci convert --standalone --all-platforms --format oci-dir "${work}/gzip" "${work}/soci"

# soci writes the converted index as a Docker manifest list, whose format has
# no artifactType, the field that marks a SOCI index. It is relabelled here
# as an OCI index, with every entry kept as soci wrote it, artifactType
# included. Only the list's own digest changes, and nothing names it.
layout="${work}/soci"
list_type=$(jq -r 'if (.manifests | length) == 1 then .manifests[0].mediaType else "" end' "${layout}/index.json")
case "$list_type" in
  application/vnd.oci.image.index.v1+json) ;;
  application/vnd.docker.distribution.manifest.list.v2+json)
    list=$(jq -r '.manifests[0].digest' "${layout}/index.json")
    jq -c '.mediaType = "application/vnd.oci.image.index.v1+json"' \
      "${layout}/blobs/sha256/${list#sha256:}" > "${work}/list.json"
    sum=$(sha256sum "${work}/list.json" | cut -d' ' -f1)
    size=$(stat -c %s "${work}/list.json")
    mv "${work}/list.json" "${layout}/blobs/sha256/${sum}"
    jq --arg digest "sha256:${sum}" --argjson size "$size" \
      '.manifests[0] |= (.mediaType = "application/vnd.oci.image.index.v1+json" | .digest = $digest | .size = $size)' \
      "${layout}/index.json" > "${work}/layout-index.json"
    mv "${work}/layout-index.json" "${layout}/index.json"
    ;;
  *)
    echo "::error::soci convert wrote no single index for ${source##*/} (${list_type:-none})" >&2
    exit 1
    ;;
esac

# crane writes each manifest's bytes as they stand in the layout. skopeo
# re-serializes an index before it pushes it, and, told to keep every
# digest, refused this one. crane prints the pushed reference on stdout and
# logs it on stderr as it pushes. In the deploy it carries the account id,
# which the credentials step masks in every later line of the log.
crane push "$layout" "$destination" > /dev/null

# skopeo refused to push a manifest it would rewrite, and crane has no such
# check. The tag must name the index in the layout, whose entries are the
# images each SOCI index names.
want=$(jq -r '.manifests[0].digest' "${layout}/index.json")
got=$(crane digest "$destination" || true)
if [[ "$got" != "$want" ]]; then
  echo "::error::${destination##*/} names ${got:-nothing}, not the index soci wrote (${want})" >&2
  exit 1
fi
