# Roadmap: operator

Owned by Jody; changed only by an agent at Jody's request. Loopers propose in PROPOSALS.md.

## Now
The operator renders a deployment: its platform services and each tenant's services; tenant namespaces go through nsproxy.
Unreleased; tacplus floor 2.4 declared in `tacplus.yaml` (#1 still open, blocked by tacplus#188).

## Next
- Nothing unblocked yet; everything waits on #1.

## Later
- Declare the tacplus floor 2.4 (#1), after tacplus#188
- Fallback entitlements policy drops the routed tenant for non-admin sign-ins (#5), after #1
- Release: tag v0.1.0 and publish the image (#6), after #1 and tacplus-dev/looper#12
- Port SeaBeam operator a70a319..f84a1158 (#7), after #1 and #6
- Work without SeaBeam: no gitlab.com/seabeam paths, sources or registries (#8), after #1
- Patch vulnerable golang.org/x/*, grpc, compress, spdystream, otel (#9), after #1
- helm/k8s/controller-runtime upgrade for 24 advisories (#10), after #1
- Add a `just gates` recipe (#11), after #1
- Several deployments per cluster: `tac-<deployment>` and `tac-<deployment>-<tid>` namespaces (#3), after #1
- Render per-tenant Stratum and the redo service (#2), after stratum#2, redo#1 and #1

## Not doing
- Creating tenant namespaces itself: a deployment has no rights outside `tac-<deployment>`; nsproxy does it (decisions/deployments-per-cluster-2026-10-26.md)
- A Stratum in the `restore` tenant: redo keeps its own disk (#2, Jody 2026-10-26)
