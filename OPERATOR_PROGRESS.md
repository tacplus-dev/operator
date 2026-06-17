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
- Follow-up blocker: new `opa` and `stratum` pods started with canonical
  configs but could not establish startup S2S connections to kernel. Their
  generated clients now verify kernel by canonical service SPIFFE URI
  (`spiffe://local.dev/system/services/kernel`), while operator-minted kernel
  node certs still carried the legacy URI SAN (`spiffe://local/dev/kernel`).
- Change: kernel node/edge TLS cert generation now uses generated
  `libs/spiffe` to build the canonical kernel URI SAN, and deploy config
  building regenerates stored kernel TLS material when existing certs lack that
  URI.
- Regression coverage: kernel SPIFFE URI tests assert canonical output,
  stale legacy SAN detection, and acceptance of freshly generated canonical
  certs.
- Verification passed: `go test ./pkg/bootstrap`, `go test ./...`, `go build
  ./cmd/cavalier-operator`, and `git diff --check`.

## 2026-06-17 demo-readiness drift cleanup

- Live blocker: kernel pods advertised the `.266` image but were still running
  `java -jar /hotfix/kernel-261.jar` from a hostPath mount, so the fixed image
  could not expose `kernel.Mint/MintX509Cert`.
- Root cause: the live StatefulSet had out-of-band command/volume drift not
  present in the generated Helm manifest; normal Helm upgrade reconciliation
  preserved the extra fields.
- First attempted change: in-cluster cavalier release upgrades used Helm force
  replacement so generated source state replaced drifted workload specs.
- Follow-up finding: global Helm force also tries to replace bound PVCs for
  eventbus/graphdb/stratum/timedb, which Kubernetes correctly rejects because
  PVC `volumeName` and `storageClassName` are immutable after binding.
- Corrected change: global Helm force is removed again; the operator now
  performs a targeted source-level cleanup of Helm-managed kernel StatefulSet
  hotfix drift (`/hotfix/kernel-261.jar`, `/hotfix` mounts, and
  `/opt/cavalier-hotfix` hostPath volumes) before normal Helm upgrade.
- Regression coverage: reconciler tests assert that the targeted cleanup removes
  kernel hotfix command/volume drift while preserving normal bootstrap/data
  mounts and skips non-Helm StatefulSets.
- Live blocker follow-up: after a mock operator redeploy, the inner `cavalier`
  Helm release was left at revision `2938` in `pending-upgrade`, so every
  reconcile failed with Helm's operation-in-progress lock.
- Change: the in-cluster reconciler now detects stale pending inner Helm
  releases. If the latest release has been pending for at least five minutes and
  a deployed revision exists, it rolls back to the deployed revision and lets the
  next reconcile apply the desired generated chart.
- Regression coverage: reconciler tests cover stale pending detection, fresh
  pending releases, and non-pending releases.
- Verification passed: `go test ./pkg/reconciler`, `go test ./...`, and
  `git diff --check`.

## 2026-06-16 demo-readiness follow-up

- Live blocker: OPA v187 pods could not verify kernel because the operator had
  regenerated a new `cluster-ca` in memory while the durable
  `cavalier-operator-crypto` Secret still held the old `cluster-ca`. Old OPA pods
  stayed "ready" but failed S2S calls; new pods received a different trust
  bundle.
- Root cause: the canonical SPIFFE migration reused `GenerateCrypto`, which
  always minted a new cluster intermediate CA. The later in-process PKI cache
  made this stable for one operator pod lifetime, but it still changed the
  trust anchor instead of rotating only the kernel TLS leaf certs.
- Change: existing cluster CA material is now preserved during generation, and
  the deploy-time canonical SPIFFE migration calls a targeted
  `GenerateKernelTLS` path when a cluster CA key already exists. This rotates
  only kernel node/edge TLS certs and keeps signing CAs/trust anchors stable.
- Live blocker: the operator also patched its own Deployment image from the
  pullable `services/operator/cavalier-operator:v1.2.187-main.202` image to
  non-pullable `services/operator/cavalier-operator:v1.2.27-main.4`.
- Root cause: self-upgrade resolved operator tags from
  `cavalier/cavalier/cavalier-operator` instead of the deployable
  `cavalier/services/operator/cavalier-operator` registry repository.
- Change: operator self-upgrade now resolves tags from the deployable
  `services/operator` namespace.
- Regression coverage: tests now pin cluster CA preservation, targeted kernel
  TLS regeneration under the preserved CA, signing CA preservation, and the
  operator registry repository path.
- Verification passed: `go test ./pkg/bootstrap ./pkg/upgrade`, `go test ./...`,
  and `git diff --check`.
- Redeploy follow-up: the in-cluster operator hit the rotation path but failed
  before applying because `/config/crypto.yaml` is mounted read-only.
- Change: deploy-config building now treats read-only/permission failures from
  `SaveCrypto` as non-fatal after in-memory crypto regeneration, while other
  save errors still fail. CLI/init paths keep strict persistence.
- Regression coverage: wrapped `EROFS` handling is tested so only read-only
  config-write failures are ignored.
- Verification passed: `go test ./pkg/bootstrap`, `go test ./...`, `go build
  ./cmd/cavalier-operator`, and `git diff --check`.

## 2026-06-17 proxy-policy bootstrap

- Live blocker: real Claude CLI calls reached the local Cavalier proxy but
  failed external-token minting because OPA had no evaluator for
  `data.proxypolicy.eval`; only the entitlement policy was loaded.
- Root cause: operator already had `ProxyPolicyDir()` but
  `BuildKernelConfigProto` never loaded `services/opa-proxy-policy/` into
  `opa.Config.proxy_policy`, so the deployed OPA config omitted the bundle.
- Change: operator now loads an optional `services/opa-proxy-policy/` bundle
  into `cfg.ProxyPolicy` with entry point `data.proxypolicy.eval`.
- Regression coverage: optional OPA policy loading now has a focused unit test.
- Verification passed: `go test ./pkg/bootstrap` and `go test ./...`.
- Live follow-up: `.200` successfully reached apply, but because the read-only
  config volume still contained stale certs, each 30-second reconcile
  regenerated a fresh PKI and produced repeated Helm upgrades.
- Change: the in-cluster service config provider now caches the regenerated PKI
  in-process and feeds it back into later deploy config builds, making the
  rendered service configs stable for the operator pod lifetime. Durable
  writable Secret persistence remains a post-demo hardening item.
- Verification passed: `go test ./pkg/bootstrap`, `go test ./...`, `go build
  ./cmd/cavalier-operator`, and `git diff --check`.
