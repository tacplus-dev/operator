#!/usr/bin/env bash
# Resolve every schematic/<name>:<tag> image in an Artifact Registry repository
# to its digest and write the release manifest the operator deploys by.
#
# Usage:
#   scripts/resolve-digests.sh <registry> [tag] [output]
#
#   registry is the repository the fleet pulls from, the same value as the
#   operator chart's operator.localImages (e.g.
#   us-central1-docker.pkg.dev/<project>/seabeam). tag defaults to "local",
#   output to "images.yaml".
#
# The output is an operator Config carrying only image_digests, the same shape
# scripts/promote.sh writes. Pass it to the operator chart with
# --set-file config.images=<output>: every image it names renders as
# <repository>@sha256:..., so a build pushed under the same tag changes the pod
# templates and the fleet rolls on its own.
#
# A local cluster that shares the docker daemon has no registry digest to pin;
# it does not run this and stays tag-only.
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <registry> [tag] [output]" >&2
  exit 2
fi
registry="${1%/}"
tag="${2:-local}"
output="${3:-images.yaml}"

packages="$(gcloud artifacts docker images list "$registry" --format='value(package)' | sort -u)"

pins=""
while read -r package; do
  # <registry>/schematic/<name>
  [[ "$package" =~ /schematic/([a-z0-9-]+)$ ]] || continue
  name="${BASH_REMATCH[1]}"
  # An image never pushed under this tag is not part of the build; skip it.
  digest="$(gcloud artifacts docker images describe "$package:$tag" --format='value(image_summary.digest)' 2>/dev/null)" || continue
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "$package:$tag: '$digest' is not a sha256 digest" >&2
    exit 1
  fi
  pins+="$name $digest"$'\n'
done <<< "$packages"

if [ -z "$pins" ]; then
  echo "no schematic/<name>:$tag images in $registry" >&2
  exit 1
fi

{
  echo "# Written by scripts/resolve-digests.sh from $registry (:$tag) on $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo "image_digests:"
  printf '%s' "$pins" | awk '{print "  " $1 ": " $2}'
} > "$output"

echo "wrote $(printf '%s' "$pins" | wc -l | tr -d ' ') digests to $output"
