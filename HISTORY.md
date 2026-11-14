# History: operator

Append-only: add entries at the end, never edit or remove one (CI enforces it). One line each:
`- <YYYY-MM-DD> <who: looper|agent|jody> <kind: issue|decision|release|deploy> <one line> (<links>)`

- 2026-11-01 looper issue just generate fails at floor 2.4: stratum (latest) declares tacplus 2.5 (#15, 354e840)
- 2026-11-02 looper issue operator.yaml spiffe_prefix reaches kmaker only: kernel and every service take kernel.yaml's spiffe_trust_domain (#14, f1b1fb0)
- 2026-11-02 looper issue Fallback entitlements policy drops the routed tenant for every non-admin sign-in (#5)
- 2026-11-02 looper issue Work without SeaBeam: no gitlab.com/seabeam module paths, sources or registries (#8, 060a346)
- 2026-11-02 looper issue Patch vulnerable golang.org/x/*, grpc, klauspost/compress, spdystream and otel (40 advisories) (#9)
- 2026-11-02 looper issue justfile has no gates recipe: add just gates (#11)
- 2026-11-02 looper issue helm v3.17.3 and its docker/containerd deps carry 24 advisories; fixing needs a helm/k8s/controller-runtime upgrade (#10, dacd018)
- 2026-11-02 looper issue golang.org/x/net v0.60.0: GO-2026-6860 and GO-2026-6866 in x/net/html; bump to v0.62.0 (#13, d7a8f0d)
- 2026-11-03 looper issue Several deployments per cluster: tac-<deployment> and tac-<deployment>-<tid> namespaces (#3, 184523a)
- 2026-11-03 looper issue Interim: NetworkPolicy so only kernel pods reach kernel's unauthenticated Raft port (until kernel#42) (#20, 032ed80)
- 2026-11-05 looper issue just gates fails on main: deploy.go still sets gateway's retired letsencrypt mode (EXTERNAL_TLS_MODE_LETSENCRYPT, acme_backend) (#21, 4dc0a3d)
- 2026-11-06 looper issue The fleet gateway's Service is a standalone-NEG ClusterIP backend for the TCP proxy LB, not a type: LoadBalancer (#22, 0b21a53)
- 2026-11-06 looper issue Stop rendering kmaker: nothing calls it; drop its objects and the extra signing CA private key (#16)
- 2026-11-07 looper issue tacplus floor is 2.5; the latest released line is 2.6: raise the floor and commit tacplus.lock (#23)
- 2026-11-09 looper issue just gates fails on main: stratum v0.1.0 moved Config.seed_members into StratumTenantConfig (pkg/bootstrap/deploy.go:125) (#25)
- 2026-11-14 looper release v0.1.2 (https://github.com/tacplus-dev/operator/releases/tag/v0.1.2)
- 2026-11-14 looper issue Dockerfile has no USER: semgrep missing-user-entrypoint (HIGH) on Dockerfile:5 fails the build's security checks (#26, 2749787)
- 2026-11-14 looper issue STATE.md Blocked on names closed issues tacplus-dev/operator#23 and tacplus-dev/tacplus#188 (#32, 11fd449)
- 2026-11-14 looper release v0.1.3 (https://github.com/tacplus-dev/operator/releases/tag/v0.1.3)
