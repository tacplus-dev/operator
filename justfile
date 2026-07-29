# Local development builds only — not used in CI/CD.

TACPLUS := env_var_or_default("TACPLUS_BIN", "tacplus")

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
  {{TACPLUS}} build --tag latest
  go mod tidy
