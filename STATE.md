# State: operator

Overwritten to stay current (loopers and agents). Last updated: 2026-10-30 by the looper (#16).

- **Version / line:** unreleased (no tag yet, #6); tacplus floor 2.4 in `tacplus.yaml`, but its dependencies now need 2.5 (#15)
- **Works:** `generate` renders the platform services (kernel, gateway, eventbus, oauth, opa, stratum, letsencrypt) and the app's own services from operator.yaml and crypto.yaml. kmaker is retired (#16): no kmaker workload, config or RBAC, and no `pki.kmaker_signing_ca`; generate rewrites an older crypto.yaml that still holds that key.
- **Released / deployed:** nothing released from this repository yet
- **Known gaps:** with the latest dependencies (tacplus 2.5.2), three delegate tests fail on stratum (`TestEnsureServiceDelegates*`), the same on main before #16, for #15; `spiffe_prefix` reaches no config since kmaker went (#14)
- **Blocked on:** `just gates` with the looper's tacplus: #15 (floor 2.5) and tacplus-dev/tacplus#188 (no `go` in the compiler image)
