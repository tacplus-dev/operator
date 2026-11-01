# Local development builds only — not used in CI/CD.

TACPLUS := env_var_or_default("TACPLUS_BIN", "tacplus")

default: local-binary container

local-binary: generate validate-generated-output
    mkdir -p target
    go build -trimpath -ldflags="-s -w -X gitlab.com/seabeam/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o target/schematic-operator ./cmd/cavalier-operator

container: generate validate-generated-output
    mkdir -p bin
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X gitlab.com/seabeam/schematic/platform/operator/pkg/bootstrap.Version=v1.2.0" -o bin/operator ./cmd/cavalier-operator
    docker buildx build --platform linux/arm64 -t registry.gitlab.com/seabeam/schematic/platform/operator/schematic-operator-1-1:latest --load .
    rm -f bin/operator

# Generated-output gate. Every artifact `schematic-operator generate` emits is a
# serialized proto its consumer validates at boot; this asserts the emitted
# bytes still satisfy those protos' spec.assert_* constraints, so a field that
# became assert_required upstream fails here instead of crash-looping a pod.
# Runs as part of `local-binary` and `container`, after `generate` has
# refreshed model/ — the gate has to see the current generated code.
validate-generated-output:
  go test ./pkg/bootstrap/ -run 'TestGeneratedOutputSatisfiesConsumerProtos|TestGeneratedKernelConfigCarriesEveryRequiredField|TestValidateDeployConfigsNamesTheMissingField|TestEveryDeployArtifactIsClassified' -count=1

# The repository's gates: what CI's proto-validate and .build-binary enforce,
# plus gofmt and vet. `generate` runs first so every check sees freshly
# generated code; `go test ./...` includes the validate-generated-output tests.
gates: generate
  test -z "$(gofmt -l $(git ls-files '*.go'))"
  go vet ./...
  go build ./...
  go test ./... -count=1

generate:
  #!/usr/bin/env bash
  set -euo pipefail
  # tacplus.yaml (its floor, depends, states: all) says what to generate from;
  # the other services' contracts come through depends, not local proto trees.
  {{TACPLUS}} build --tag latest
  # pkg/bootstrap embeds the registry so the binary is self-contained; model/ is
  # gitignored and absent in-cluster, and Go embed cannot reach a parent dir.
  cp model/registry.json pkg/bootstrap/registry.json
  go mod tidy
