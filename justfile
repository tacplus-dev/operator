# Local development builds only — not used in CI/CD.

TACPLUS_IMAGE := "registry.gitlab.com/secondstarcavalier/schematic/tacplus/tacplus-1-2:latest"

default: local-binary container

local-binary: generate
    mkdir -p target
    go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o target/cavalier-operator ./cmd/cavalier-operator

container: generate
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o cavalier-operator ./cmd/cavalier-operator
    docker buildx build --platform linux/arm64 -t registry.gitlab.com/secondstarcavalier/schematic/platform/operator/cavalier-operator-1-1:latest --load .
    rm -f cavalier-operator

generate:
  #!/usr/bin/env bash
  set -euo pipefail
  if [ -d proto ]; then
    git -C proto fetch --quiet
    git -C proto reset --hard --quiet origin/HEAD
  else
    git clone --quiet https://gitlab.com/secondstarcavalier/schematic/platform/proto.git proto
  fi
  docker pull "{{TACPLUS_IMAGE}}" --quiet
  docker run --rm \
    -v "$PWD:/work" \
    -w /work \
    "{{TACPLUS_IMAGE}}" \
    tacplus build --src proto
  go mod tidy
