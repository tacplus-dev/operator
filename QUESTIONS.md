# Open questions — operator StatefulSet recreate work (2026-09-28)

## 1. "State-bearing" has no signal the operator can gate on

Task B asked for kernel to be recreated so that each pod is Ready **and state-bearing**
before the next pod is touched. The operator can only see Ready:

- Kernel removed its `StateBearingLedger` in kernel `26f5a40` (2026-09-25, kernel-no-durability).
- Kernel's gRPC health goes `SERVING` as soon as `ClusterServer` finishes construction
  (`ClusterServer.java:469`, "Cluster is always SERVING once construction completes"). That
  happens right after the Hazelcast join, before partition migration or backups have finished.

The StatefulSet controller already rolls pods **one at a time**, waiting for each updated
pod to be Running and Ready, under either pod management policy. Pod management only
changes how pods are created and scaled. Most likely the 2026-09-28 loss (three pods replaced
"inside a minute") was that one-at-a-time roll gated on a Ready that means nothing about
state. It was probably not a burst of simultaneous replacements.

**Default taken:** kernel renders `podManagementPolicy: OrderedReady`. This stops Parallel from
creating every missing kernel pod at once, which is what happened on the 2026-09-23 0→3 scale.
The recreate path now actually runs, so the live Parallel kernel is switched by an orphan-delete
and its pods stay up. Pods are still gated on Ready and nothing more.

**Needs a decision (kernel-side, outside this directory):** make kernel readiness mean
state-bearing. For example, report `NOT_SERVING` until Hazelcast `PartitionService.isClusterSafe()`
(or `isLocalMemberSafe()`) is true, and have shutdown wait for it too. Once that is in place,
the StatefulSet controller's existing Ready gate becomes the state-bearing gate, and the
operator needs no extra code. Operator-side alternatives I did not build: `minReadySeconds`
is a timer, not a signal; an operator-driven `OnDelete`/partition roll still has no
state-bearing signal to wait on.

## 2. The chart's managed-by label is overwritten by Helm, so two other paths are also dead

Helm v3.17 `setMetadataVisitor` (install and upgrade, `force=true`) sets
`app.kubernetes.io/managed-by: Helm` on every resource it renders. It replaces the chart's
`schematic-operator`. The live cluster confirms this: every StatefulSet and Deployment in
`schematic` shows `managed-by=Helm`. This is why `reconcileImmutableStatefulSetFields` never
saw kernel. I fixed that path by switching it to Helm's release annotations. These still
select on `managed-by=schematic-operator`, so they never match anything live:

- `removeKernelHotfixDrift` (`reconciler.go`, `st.Labels[helm.ManagedByLabel] != helm.ManagedByValue`)
- `pruneStaleDeployments` (label selector on the same value)
- The comment on `helm.ManagedByValue` (`generator.go:36-42`) states the opposite of what Helm does.

The tests `TestRemoveKernelHotfixDriftIgnoresTheHelmManagedByValue` and
`TestPruneStaleDeploymentsSelectsTheChartsManagedByLabel` encode the wrong premise.

**Default taken:** left alone. They are adjacent and outside this task. Should they move to the
release-annotation gate as well?

## 3. Pre-existing `pkg/bootstrap` test failures (unrelated, present before this change)

`TestEnsureServiceDelegatesAppServiceGetsBroker`, `TestDefaultOPAPolicyUsesGeneratedGrantNames`
(ECHO_ANY/TASKS_ANY/QUERY_ANY not in the generated catalog) and `TestVolumeGroupsEmptyRegistryStaysEmpty`
(the embedded registry now declares 1 volume group). Untouched.
