# Proposals: operator

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **Drop the retired-key scrub once every crypto.yaml is rewritten** (2026-10-30, #16): `cryptoHoldsRetiredKeys`
  (pkg/bootstrap/settings.go) makes generate rewrite a crypto.yaml that still holds `pki.kmaker_signing_ca`, so the
  kmaker CA private key leaves existing config dirs. Once every deployment has run a generate with it (after the first
  release that carries #16), the check is dead weight and can go; the proto's `reserved 7` stays.
- **Drop the shared-platform-service rule with per-tenant Stratum** (2026-11-01, #15): `DeployedServiceInstances`
  (pkg/bootstrap/services.go) fans out per tenant only the app services, because the helm generator renders every
  platform service shared, though registry.json marks stratum tenant_owned since stratum#2. When #2 renders Stratum per
  tenant, the generator and that rule have to change together (one source for "deployed per tenant"), and
  `helm.StratumSeedMembers` becomes per tenant. The old shape #2 removes is still rendered meanwhile:
  `STRATUM_MEMBER_ROLE`, `STRATUM_WS_SEED_MEMBERS`, the writers' volume and `disk_persistence`, which the stratum#2 image
  no longer reads.

## Promoted / declined
