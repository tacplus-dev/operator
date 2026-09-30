#!/usr/bin/env bash
# Record the image digests running on a cluster (dev) into a release manifest
# that another cluster's operator (pilot) deploys by digest.
#
# Usage:
#   scripts/promote.sh <kube-context> [namespace] [output]
#
#   namespace defaults to "schematic", output to "images.yaml".
#
# The output is an operator Config carrying only image_digests. Put it in the
# pilot operator's config dir next to operator.yaml: LoadConfig merges it, and
# every image it names renders as <repository>@sha256:... instead of :local.
# Images it does not name keep their tag.
#
# Only images tagged schematic/<name>:local are recorded, keyed by <name>. The
# digest comes from the pod's status imageID, which is a pullable manifest
# digest only when the node pulled the image from a registry. An image built
# straight into the node's store reports its local image ID instead
# (docker://sha256:...), which no registry can serve; that is an error, not a
# pin. So is one image running at two digests (a rollout in progress).
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 3 ]; then
  echo "usage: $0 <kube-context> [namespace] [output]" >&2
  exit 2
fi
context="$1"
namespace="${2:-schematic}"
output="${3:-images.yaml}"

statuses="$(kubectl --context "$context" -n "$namespace" get pods -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.name}{"\t"}{.image}{"\t"}{.imageID}{"\n"}{end}{end}')"

pins=""
while IFS=$'\t' read -r container image image_id; do
  [ -n "$image" ] || continue
  # <optional registry prefix>/schematic/<name>:local
  if [[ ! "$image" =~ (^|/)schematic/([a-z0-9-]+):local$ ]]; then
    continue
  fi
  name="${BASH_REMATCH[2]}"
  if [[ ! "$image_id" =~ @(sha256:[0-9a-f]{64})$ ]]; then
    echo "$name (container $container): imageID '$image_id' is not a registry digest; push the image to a registry and redeploy dev from it" >&2
    exit 1
  fi
  pins+="$name ${BASH_REMATCH[1]}"$'\n'
done <<< "$statuses"

pins="$(printf '%s' "$pins" | sort -u)"
if [ -z "$pins" ]; then
  echo "no schematic/<name>:local images running in $context/$namespace" >&2
  exit 1
fi
split="$(printf '%s\n' "$pins" | awk '{print $1}' | uniq -d)"
if [ -n "$split" ]; then
  echo "running at more than one digest (a rollout in progress?): $split" >&2
  exit 1
fi

{
  echo "# Written by scripts/promote.sh from $context/$namespace on $(date -u +%Y-%m-%dT%H:%M:%SZ)."
  echo "image_digests:"
  printf '%s\n' "$pins" | awk '{print "  " $1 ": " $2}'
} > "$output"

echo "wrote $(printf '%s\n' "$pins" | wc -l | tr -d ' ') digests to $output"
