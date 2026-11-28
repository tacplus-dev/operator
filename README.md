# operator

The tacplus operator: `schematic-operator generate` renders a fleet's Helm chart (the platform services in
`tac-<deployment>`, each tenant's tenant-owned services in `tac-<deployment>-<tid>`) from operator.yaml and crypto.yaml,
and the in-cluster operator reconciles it. STATE.md says what works today.

## Isolation is the token at every door

Reachability does not imply authorization. Every listener checks the caller's bearer token (kernel-issued, its
SPIFFE subject naming its tenant or `/system`), so a tenant's service is refused at another tenant's door whatever the
network lets through (tacplus-dev/roadmap decisions/s2s-never-crosses-tenants-2026-10-31.md, addendum 2026-11-07 (2)).
The chart renders no NetworkPolicy to isolate deployments or tenants (#24).

The one exception is a protocol with no authentication of its own: kernel's Raft port (unauthenticated Ratis gRPC) and
kernel's Hazelcast member port (an unauthenticated member protocol holding kernel's state; #30, the decision's addendum
of 2026-11-10), which the `kernel-raft` NetworkPolicy opens to kernel's own pods only, in the deployment's platform
namespace where every kernel of the deployment runs. Kernel's gRPC port and health port stay open to every source. The
policy goes when tacplus-dev/kernel#42 moves Raft behind the authenticated member port on loopback
(tacplus-dev/tacplus#212) and kernel's member traffic follows stratum's there.

## A StatefulSet scaled to 0 keeps its volumes

Setting a StatefulSet component's count to 0 (`kernel_nodes` in operator.yaml) removes its StatefulSet, and its
PersistentVolumeClaims stay, on purpose (#54): so do the claims
past a StatefulSet's replicas after a scale-down. A count of 0 is also what a missing topology field or a services
filter gives, and deleting the record store's volumes on that would lose its data for good. The operator deletes none
of them: on each change in what it keeps it logs the warning `kept the volume claims of a StatefulSet the chart no
longer runs`, with the namespace, the StatefulSet, its claims and the command that removes them, in the form

    kubectl delete pvc -n tac-<deployment> data-stratum-0 data-stratum-1 data-stratum-2

That one is what an upgrade to per-tenant Stratum leaves (#2): the shared `stratum` StatefulSet and its writers' volumes
are gone, each tenant's Stratum is in memory with the redo service its durability, and nothing will bind those claims
again. redo's claim is `data-redo-0` in the platform namespace, `tac-<deployment>`. Kept claims cost
what their disks cost while unused, and a component scaled back up binds them again with whatever they hold: right for
a restart, wrong after a version or key change. Run the command before scaling back up when the data must not come
back, and once the component is gone for good.
