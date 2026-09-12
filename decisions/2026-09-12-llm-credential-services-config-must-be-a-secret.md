# llm_credential_services configs are Secrets, not ConfigMaps

2026-09-12. Fixes Critical #1 of `decisions/security-audit-2026-09-12/CONSOLIDATED_REPORT.md`
— the structural, generalized form of the plaintext-`llm_credential` exposure found live
earlier the same day (`schematic-operator-config`). Code-only change in `pkg/helm/generator.go`
plus tests; no config, chart-template or deploy-logic change.

## The bug

`serviceSpec.ConfigHasKeyMaterial` (`pkg/helm/generator.go:288`) is the single switch that
decides how a service's rendered bootstrap config is packaged. It is consumed in two places:

- `generator.go:1338` — the config object's `kind` becomes `Secret`/`type: Opaque`/`stringData`
  instead of `ConfigMap`/`data`, and the template file is `secret-N.yaml` instead of
  `configmap-N.yaml`.
- `generator.go:1238` (`configVolumeSource`) — the pod's config volume mounts
  `secret: secretName: <name>-config` instead of `configMap: name: <name>-config`.

It was set `true` on exactly three specs — kernel (`:710`), kmaker (`:737`), gateway (`:755`)
— the three platform services known at write time to embed private keys.

But `pkg/bootstrap/deploy.go:256-274` injects the real `cfg.GetLlmCredential()` value into the
bootstrap config of **any** service named in `cfg.GetLlmCredentialServices()`. That list is
config-driven, not a fixed set of names, and those services are ordinary app services whose
`serviceSpec` is built generically in the `appServiceNames(topo)` loop — which never set
`ConfigHasKeyMaterial`. So opting a service into `llm_credential_services` was *guaranteed* to
render its API key into a plain ConfigMap: no encryption at rest, readable by anything holding
`configmap get` in the namespace. Not a misconfiguration — the generator had no path that could
produce anything else.

## The fix

Read the same list `deploy.go` reads, in the same generator function (`cfg` is already in
scope), and set the flag on the app-service `base` spec:

```go
llmCredentialServices := map[string]bool{}
for _, name := range cfg.GetLlmCredentialServices() {
    llmCredentialServices[name] = true
}
...
ConfigHasKeyMaterial: llmCredentialServices[name],
```

Deliberately the *same* signal, not a parallel one: any second list would drift from the list
that decides what actually gets injected, and drift here is silent and reintroduces the
exposure. Setting it on `base` (before the tenant fan-out) covers tenant-owned services too —
`deploy.go` writes the credential-bearing config to every `<svc>-<tenant>` instance, and each
instance spec is a copy of `base`.

Nothing else was needed. `WriteToDisk` (`generator.go:163-168`) already deletes a superseded
`configmap-N.yaml` whenever it writes a `secret-N.yaml`, generically for any service, so a
service added to the list after its chart was first rendered does not leave a stale ConfigMap
behind for helm to apply alongside the Secret.

## What was NOT changed

- kernel/kmaker/gateway's hardcoded `true` — already correct.
- `deploy.go`'s injection logic — correct about *what* is injected; the bug was only in how the
  result is packaged.
- Codex's separate Critical claim that private keys are committed in `charts/cavalier`'s
  ConfigMap templates: independently re-checked and wrong; those are empty placeholders. See
  the "Correction" section of the consolidated report.
- The report's two High items (`gen-backend-tokens` non-functional; stale-pending-release
  recovery gaps) — out of scope here, still open.

## Verification

`TestLlmCredentialServiceConfigsArePackagedAsSecrets` and
`TestLlmCredentialTenantOwnedServiceConfigsArePackagedAsSecrets` in
`pkg/helm/generator_test.go`: a service in the list renders `secret-0.yaml` (`kind: Secret`,
`type: Opaque`, `stringData`), renders no `configmap-0.yaml`, and its Deployment mounts the
Secret; a service not in the list is untouched (still `kind: ConfigMap`, mounted as a
configMap). Both fail against the unfixed generator with "tasks config is emitted as a
ConfigMap" — checked by reverting the fix and re-running.

`go test ./pkg/helm/` — 25 tests, all pass. `go test ./...` — two failures in `pkg/bootstrap`
(`TestEnsureServiceDelegatesAppServiceGetsBroker`, `TestDefaultOPAPolicyUsesGeneratedGrantNames`),
both pre-existing: they fail identically at HEAD with this change reverted.
