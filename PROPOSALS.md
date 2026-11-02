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
- **Retire the auto-upgrade checker, or make it follow release lines** (2026-11-02, #8): `pkg/upgrade` now polls
  `ghcr.io/tacplus-dev/<service>`, but it still selects SeaBeam's tag scheme (`vMAJOR.MINOR.PROTO_PATCH-BRANCH.LOCAL_PATCH`,
  `selectLatest`), which no tacplus-dev release uses (#6: `vMAJOR.MINOR.PATCH`, line tag `MAJOR.MINOR`). Its registry
  credentials still come from the `gitlab-registry` secret, which holds none for ghcr.io (public images need none). Fleets
  pin by release line and image digest (tacplus-dev/tacplus#180), so the poller may simply go. Its `series` field is unused
  since the images lost the `-<series>` suffix.
- **Retire or retarget the GitLab pull secret** (2026-11-02, #8): `gitlab_registry_token` (operator.proto field 14), the
  chart's `registryToken`, and the rendered `gitlab-registry` Secret give pull credentials for `registry.gitlab.com`.
  The operator no longer renders any image from there. This is a contract change (reserve field 14, rename the
  imagePullSecret), so it is not done here.
- **Publish the operator binaries with each release, or drop `upgrade`** (2026-11-02, #8): `schematic-operator upgrade`
  now downloads `schematic-operator-<os>-<arch>` from the GitHub release of tacplus-dev/operator. #6 publishes only the
  image, so the command finds nothing until a release attaches those assets.

## Promoted / declined
