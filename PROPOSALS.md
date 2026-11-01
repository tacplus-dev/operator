# Proposals: operator

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **Drop the retired-key scrub once every crypto.yaml is rewritten** (2026-10-30, #16): `cryptoHoldsRetiredKeys`
  (pkg/bootstrap/settings.go) makes generate rewrite a crypto.yaml that still holds `pki.kmaker_signing_ca`, so the
  kmaker CA private key leaves existing config dirs. Once every deployment has run a generate with it (after the first
  release that carries #16), the check is dead weight and can go; the proto's `reserved 7` stays.
- **Service secrets in a service's Config move to its Secrets message** (2026-11-01, #17): oauth's providers carry
  `client_secret` on `oauth.Config` (and tenants' `OAuthTenantConfig`), so a sign-in secret travels in kernel's
  combined config seed, the leak `serviceSpec.SecretEnvVars` exists to prevent, and sits in the uni-config's committed
  part instead of under `secrets`. Moving them to `oauth.Secrets` (tacplus-dev/oauth) puts every secret of a
  deployment under `secrets` and lets validate refuse one anywhere else.
- **Retire the bundle path** (2026-11-01, #17): `build`, `bundle expand|pack` and `apply --bundle` are a second way to
  hand the operator a deployment, which the uni-config replaces; a bundle carries no service secrets. Drop them once no
  deployment uses them.
- **The operator's image pull secret leaves helm values** (2026-11-01, #17): the operator chart's `registryToken` value
  renders the `gitlab-registry` Secret from a helm value, so the token sits in the release record. The pipeline could
  apply that pull secret with the uni-config instead, or the image move off the private registry (#8) makes it moot.
- **tacplus: a dotted proto package breaks Go generation** (2026-11-01, #17): `package operator.v1;` fails in goimports
  (`helpers.go: expected ';', found '.'`) because the Go package is named after the proto package verbatim, so the
  uni-config's package is `operatorv1`. Worth a tacplus bug if versioned packages (`x.v1`) are to be the convention.
- **Uni-config size limits** (2026-11-01, #17): `UniConfig.services` and `opa_policies` values get tacplus's default
  32768-byte cap per value; a large `kernel.yaml` (many delegates) or policy file would be refused by validate. Raise it
  with an explicit per-value bound if a deployment comes near.

## Promoted / declined
