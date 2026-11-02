# History: operator

Append-only: add entries at the end, never edit or remove one (CI enforces it). One line each:
`- <YYYY-MM-DD> <who: looper|agent|jody> <kind: issue|decision|release|deploy> <one line> (<links>)`

- 2026-11-01 looper issue just generate fails at floor 2.4: stratum (latest) declares tacplus 2.5 (#15, 354e840)
- 2026-11-02 looper issue operator.yaml spiffe_prefix reaches kmaker only: kernel and every service take kernel.yaml's spiffe_trust_domain (#14, f1b1fb0)
- 2026-11-02 looper issue Fallback entitlements policy drops the routed tenant for every non-admin sign-in (#5)
- 2026-11-02 looper issue Work without SeaBeam: no gitlab.com/seabeam module paths, sources or registries (#8, 060a346)
- 2026-11-02 looper issue Patch vulnerable golang.org/x/*, grpc, klauspost/compress, spdystream and otel (40 advisories) (#9)
