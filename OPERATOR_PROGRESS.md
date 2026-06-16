# Operator Progress

## 2026-06-13

- Fanout failure: pipeline `2598313538` on tag `v1.2.183-main.190` failed all build targets at `pkg/bootstrap/example.go` because `kernel.Config` no longer has `SpiffePrefix` when regenerated from proto `v1.2.183-main.183`.
- Root cause: proto retired and reserved `kernel.Config.spiffe_prefix`; operator `main` still set the removed generated field in the kernel bootstrap config literal.
- Change: removed the stale `SpiffePrefix` assignment from `BuildKernelConfig`.
- Verification passed: temp CI-shape build in `/tmp/operator-proto-183-build` against proto tag `v1.2.183-main.183` with `tacplus build --src proto`, `go mod download`, and `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/cavalier-operator`.
- Entitlement-shape follow-up: default bootstrap OPA policy now emits flat
  `token_entitlements.grants` strings instead of service-keyed maps.
- Verification passed: `go test ./...`.

## 2026-06-16

- Demo redeploy blocker: new `argus` and `gateway` pods crash-looped at boot
  because generated service ConfigMaps still emitted the retired
  `spiffe_prefix: local/dev` field and omitted `spiffe_trust_domain`.
- Root cause: generated Rust service-manager code now fails closed when
  `grpcconfig.Config.spiffe_trust_domain` is empty, but operator backend config
  rendering still used the old field name.
- Change: backend service config rendering now derives the canonical
  `spiffe_trust_domain` and `location_path` from the kernel bootstrap config
  defaults and emits those fields for every service ConfigMap.
- Regression coverage: `TestBuildBackendFieldsEmitsCanonicalSpiffeConfig`
  asserts the new fields are emitted and the retired `spiffe_prefix` key is not.
- Verification passed: `go test ./pkg/bootstrap`, `go test ./...`, and
  `go build ./cmd/cavalier-operator`.
