# operator

The tacplus operator: `schematic-operator generate` renders a fleet's Helm chart (the platform services in
`tac-<deployment>`, each tenant's tenant-owned services in `tac-<deployment>-<tid>`) from operator.yaml and crypto.yaml,
and the in-cluster operator reconciles it. STATE.md says what works today.

## Isolation is the token at every door

Reachability does not imply authorization. Every listener checks the caller's bearer token (kernel-issued, its
SPIFFE subject naming its tenant or `/system`), so a tenant's service is refused at another tenant's door whatever the
network lets through (tacplus-dev/roadmap decisions/s2s-never-crosses-tenants-2026-10-31.md, addendum 2026-11-07 (2)).
The chart renders no NetworkPolicy to isolate deployments or tenants (#24).

The one exception is a port with no authentication of its own: kernel's Raft port (unauthenticated Ratis gRPC), which
the `kernel-raft` NetworkPolicy opens to kernel's own pods only, in the deployment's platform namespace where every
kernel of the deployment runs. It goes when tacplus-dev/kernel#42 moves Raft behind the authenticated member port on
loopback (tacplus-dev/tacplus#212).
