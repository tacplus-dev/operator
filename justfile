# Local development builds only — not used in CI/CD.

TACPLUS := env_var_or_default("TACPLUS_BIN", "tacplus")

default: local-binary container

local-binary: generate validate-generated-output
    mkdir -p target
    go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o target/schematic-operator ./cmd/cavalier-operator

container: generate validate-generated-output
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X gitlab.com/secondstarcavalier/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o schematic-operator ./cmd/cavalier-operator
    docker buildx build --platform linux/arm64 -t registry.gitlab.com/secondstarcavalier/schematic/platform/operator/schematic-operator-1-1:latest --load .
    rm -f schematic-operator

# Generated-output gate. Every artifact `schematic-operator generate` emits is a
# serialized proto its consumer validates at boot; this asserts the emitted
# bytes still satisfy those protos' spec.assert_* constraints, so a field that
# became assert_required upstream fails here instead of crash-looping a pod.
# Runs as part of `local-binary` and `container`, after `generate` has
# refreshed model/ — the gate has to see the current generated code.
validate-generated-output:
  go test ./pkg/bootstrap/ -run 'TestGeneratedOutputSatisfiesConsumerProtos|TestGeneratedKernelConfigCarriesEveryRequiredField|TestValidateDeployConfigsNamesTheMissingField|TestEveryDeployArtifactIsClassified' -count=1

generate:
  #!/usr/bin/env bash
  set -euo pipefail
  {{TACPLUS}} build --tag latest
  # pkg/bootstrap embeds the registry so the binary is self-contained; model/ is
  # gitignored and absent in-cluster, and Go embed cannot reach a parent dir.
  cp model/registry.json pkg/bootstrap/registry.json
  go mod tidy
