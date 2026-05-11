# Build local binary and container image
default: local-binary container

# Build a local binary at ./target/cavalier-operator
local-binary: install
    mkdir -p target
    go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o target/cavalier-operator ./cmd/cavalier-operator

# Build linux binary and container image (arm64 for cluster)
container: install
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o cavalier-operator ./cmd/cavalier-operator
    docker buildx build --platform linux/arm64 -t registry.gitlab.com/secondstarcavalier/schematic/platform/operator/cavalier-operator-1-1:latest --load .
    rm -f cavalier-operator

install: proto-sync
    tacplus build --skip-post --src proto
    go mod tidy
    go install -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" ./cmd/cavalier-operator

proto-sync:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -d proto ] && [ -d proto/.git ]; then
        git -C proto fetch --quiet
        git -C proto reset --hard --quiet origin/HEAD
    elif [ ! -d proto ]; then
        git clone --quiet https://gitlab.com/secondstarcavalier/schematic/platform/proto.git proto
    fi
