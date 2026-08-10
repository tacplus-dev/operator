# Local development builds only — not used in CI/CD.

TACPLUS := env_var_or_default("TACPLUS_BIN", "tacplus")

default: local-binary container

local-binary: generate
    mkdir -p target
    go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o target/schematic-operator ./cmd/schematic-operator

container: generate
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o schematic-operator ./cmd/schematic-operator
    docker buildx build --platform linux/arm64 -t registry.gitlab.com/secondstarcavalier/schematic/platform/operator/schematic-operator-1-1:latest --load .
    rm -f schematic-operator

generate:
  #!/usr/bin/env bash
  set -euo pipefail
  {{TACPLUS}} build --tag latest
  # pkg/bootstrap embeds the registry so the binary is self-contained; model/ is
  # gitignored and absent in-cluster, and Go embed cannot reach a parent dir.
  cp model/registry.json pkg/bootstrap/registry.json
  go mod tidy
