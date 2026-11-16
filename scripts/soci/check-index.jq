# True when an image index holds an x64 and an arm64 image, each with a SOCI
# index manifest v2 beside it. Fargate starts a task from an index with no
# SOCI index all the same, pulling the whole image first, so nothing later
# would notice one missing. Where every SOCI entry names its platform, each
# architecture needs its own; where none does, one per image is the most the
# index shows. The deploy runs it over the index it hands on, and the gate's
# soci job over the index it pushed.
[.manifests[] | select(.artifactType == null) | .platform.architecture] as $images
| [.manifests[] | select(.artifactType == "application/vnd.amazon.soci.index.v2+json")] as $soci
| ($images | index("amd64")) != null and ($images | index("arm64")) != null
  and ($soci | length) >= ($images | length)
  and (if all($soci[]; .platform.architecture != null)
       then all($images[]; . as $arch | ([$soci[].platform.architecture] | index($arch)) != null)
       else true end)
