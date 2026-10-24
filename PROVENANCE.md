# Provenance

Copied **wholesale** on 2026-10-23 from `~/workspace/gitlab/seabeam/schematic/platform/services/operator` at
commit `a70a319` (branch `d3-stratum-c2s-split`), with `proto/services/operator.proto`, the common/vservices protos it imports,
and `proto/dependencies.yaml` from `platform/proto` at `57f45fe` on branch `big8-p1-messaging`. Build outputs not copied.

Jody, 2026-10-23: the platform's primitives move to tacplus-dev as ships in the night; the private copy runs
the launch fleet unchanged and new-design changes land here.

Divergences: `kind: service`; headers in tacplus.yaml (Jody Scott, Apache-2.0; LICENSE added); `src: proto`;
`proto/registry.yaml` is a lock of github.com/tacplus-dev/registry.

Known gap (2026-10-23): operator.proto imports other services' protos (see `depends:` in tacplus.yaml), so
`tacplus registry --proto-dir proto` cannot scan it standalone yet and `proto/registry.yaml` has no entry for
operator (registry id 47). `tacplus registry` must learn to resolve `depends` when scanning; until then the
fleet build will refuse operator ("no entry for it in registry.yaml").

2026-10-23 night (Jody): *"Operator might should be a library and not a service within tacplus-dev because it
does not know about upstream services that will exist."* So `kind: library`: operator is the machinery a
deployment repo builds its own operator from, against that fleet's merged tree. Follow-ups: split the Go
module so a deployment repo can import it and supply the fleet model; move fleet-specific rendering (the
test-harness CronJobs, hand-written service special cases) out of the library to the fleet layer. The
hand-written per-service defaults table is already gone (the generated ServicesDefaults recurses).
