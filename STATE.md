# State: operator

Overwritten to stay current (loopers and agents). Last updated: 2026-11-01 by the looper (#15).

- **Version / line:** unreleased (no tag yet, #6); tacplus floor 2.5 in `tacplus.yaml` (#15)
- **Works:** `generate` renders the platform services (kernel, gateway, eventbus, oauth, opa, stratum, letsencrypt) and the app's own services from operator.yaml and crypto.yaml. kmaker is retired (#16): no kmaker workload, config or RBAC, and no `pki.kmaker_signing_ca`; generate rewrites an older crypto.yaml that still holds that key. The fleet has one SPIFFE trust domain (#14): operator.yaml `spiffe_prefix`, else kernel.yaml `spiffe_trust_domain`, else `local.dev`, in kernel, every service config and kernel's SAN; the two set and differing is refused.
- **Released / deployed:** nothing released from this repository yet
- **Known gaps:** Stratum is rendered as before, one shared cluster (writers and search members), though stratum#2 marks it tenant_owned: per-tenant Stratum, without roles, volumes or env seeds, is #2. Until then its delegate stays shared and kernel seeds its `seed_members` with the rendered pods (#15)
- **Blocked on:** tacplus-dev/tacplus#188 (no `go` in the compiler image)
