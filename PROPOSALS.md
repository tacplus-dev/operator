# Proposals: operator

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **Set the operator's metrics bind address on purpose** (2026-11-11, #31): `manager.New` in `pkg/bootstrap/init.go`
  sets no `Metrics`, so controller-runtime's metrics server serves HTTP/1 on its default address, which is how
  GO-2026-6859 (net/http server crash, fixed by Go 1.27.4 in #31) reached the operator's pod. Binding it to what the chart
  scrapes (its `metrics` port), or turning it off if nothing scrapes it, keeps the next net/http server advisory off a
  listener nobody chose.
- **Admit the gateway's edge port only from Google's proxy ranges on GKE** (2026-11-06, #22): the `gateway-edge`
  NetworkPolicy (`addDeploymentNetworkPolicies`) admits the gateway's TLS port from anywhere, which was the
  LoadBalancer's need. Behind the standalone NEG the connections come from Google's proxies and health checks
  (`35.191.0.0/16`, `130.211.0.0/22`; tacplus-dev/cloud README, The public entry), and cloud's node firewall already
  admits only those. Narrowing the policy too (an operator.yaml switch, since local clusters reach the pods otherwise)
  would hold if a node firewall rule is ever widened.
- **Local clusters reach the gateway by port-forward** (2026-11-06, #22): with no LoadBalancer Service, a kind or Docker
  Desktop cluster no longer gets the gateway on localhost by itself; `kubectl port-forward svc/gateway-edge 8443`
  does it. A note in the local docs (or a dev-only NodePort) if that turns out to bite.
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
- **Reconcile tenant-namespace StatefulSets' immutable fields** (2026-11-03, #2): `reconcileImmutableStatefulSetFields`
  (pkg/reconciler) lists the StatefulSets of the platform namespace only. `redo-restore` runs in the restore tenant's
  namespace, so a change to a field Kubernetes forbids patching there (its storage class, say) fails the helm upgrade
  instead of recreating the StatefulSet. Per-tenant Stratum (#2) will be StatefulSets in tenant namespaces too; the
  reconciler should walk every namespace the chart renders into.
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
- **Retire the one-namespace layout once seatrials has moved** (2026-11-02, #3): without `deployment`, generate still renders
  the fleet into `namespace` (default `schematic`), tenant instances included, with no isolation policies and the operator's
  own chart's ClusterRole. Once fleet-seatrials#4 has moved seatrials to `tac-seatrials`, `deployment` can become required,
  `namespace` reserved, and the legacy branches (`deployment.PlatformNamespace`'s fallback, the operator chart's
  non-deployment templates) go.
- **Prune stale workloads in tenant namespaces** (2026-11-02, #3): the reconciler prunes Deployments the chart no longer
  renders only in the platform namespace. A tenant namespace is deleted with its tenant, but a tenant-owned service dropped
  from `app_services` while the tenant lives leaves its Deployment there until helm's own release diff removes it; check
  that it does, and prune there as well if not.
- **Pull secret in tenant namespaces** (2026-11-02, #3): `gitlab-registry` is rendered only in the platform namespace,
  while tenant pods also name it as their imagePullSecret. Harmless for the public ghcr.io images; it goes with the
  GitLab pull secret proposal above.
- **Node pools and the gateway count in a deployment** (2026-11-02, #3): a deployment may not list the cluster's nodes, so
  its reconciler counts none: `topology.gateway_nodes` must be set (generate refuses 0), and pool placement and the
  PodDisruptionBudgets that come with it are off. Either the cluster pipeline publishes the pool facts into the deployment
  namespace (a ConfigMap), or pools become operator.yaml settings.
- **Watch tenant namespaces too** (2026-11-02, #3): the reconciler's cache and `replaceStuckOldRevisionPods` cover only the
  platform namespace; a tenant instance stuck on an old revision in its tenant namespace is not replaced.
- **Retire the per-service `serviceConfig` delivery** (2026-11-04, #21; it replaces the 2026-11-02 placeholder proposal
  from #3): letsencrypt was its only user and retired with the in-deployment ACME path (tacplus-dev/roadmap
  decisions/certificates-by-pipeline-2026-11-03.md). `serviceSpec.HasServiceConfig` (pkg/helm/generator.go) is set by no
  service now, and `Bundle.ServiceConfigs` (pkg/bootstrap/bundle.go) is always empty except when an older bundle carries
  one. Remove both, or keep them as the generic mechanism and add the missing values.yaml placeholder then.
- **gateway.yaml `letsencrypt` is no longer read** (2026-11-04, #21): `LoadGatewayOptions` used to accept
  `letsencrypt: true` in services/gateway.yaml as a second switch; it is gone with the mode. gateway's Config has no such
  field, so `BuildKernelConfigProto` already refuses the key as unknown (TestBuildKernelConfigRejectsUnknownServiceKey).
- **Remove the kernel-raft NetworkPolicy when kernel#42 lands** (2026-11-03, #20): `kernelRaftNetworkPolicy`
  (pkg/helm/generator.go) admits only kernel's pods to its Raft port, and `deployment-isolation` leaves kernel out of its
  selector for it, because the port carries Ratis's unauthenticated gRPC. tacplus-dev/kernel#42 moves that traffic into
  the authenticated member port and retires the port: then the policy, kernel's exclusion from the isolation selector
  and `helm.KernelRaftPort` go, and kernel is isolated like every other platform pod.
- **Confine kernel's Hazelcast member port to kernel's pods too** (2026-11-03, #20): the kernel-raft policy keeps kernel's
  Hazelcast port (5001) open to the whole deployment (to every source without one), as it was. Its member protocol is as
  unauthenticated as Raft (see `HazelcastNetworkPolicy`, stratum's), and admitting it only from `app: kernel` is now one
  line in the same policy; it was held back until verified against a live kernel cluster.

## Promoted / declined
