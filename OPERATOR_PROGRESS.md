# Operator Progress

## 2026-06-13

- Fanout failure: pipeline `2598313538` on tag `v1.2.183-main.190` failed all build targets at `pkg/bootstrap/example.go` because `kernel.Config` no longer has `SpiffePrefix` when regenerated from proto `v1.2.183-main.183`.
- Root cause: proto retired and reserved `kernel.Config.spiffe_prefix`; operator `main` still set the removed generated field in the kernel bootstrap config literal.
- Change: removed the stale `SpiffePrefix` assignment from `BuildKernelConfig`.
- Verification passed: temp CI-shape build in `/tmp/operator-proto-183-build` against proto tag `v1.2.183-main.183` with `tacplus build --src proto`, `go mod download`, and `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/cavalier-operator`.
