# Kernel re-seeds every config root from the bundle on every boot

2026-09-23. Kernel-side change (`platform/services/kernel`); recorded here because it changes
what an operator can expect a live config push to do. No operator code change.

Kernel installs `Config.configs` into every service's ConfigState root on **every boot**,
overwriting whatever the root already held. Previously that seeding was create-only, so a
populated root was never re-seeded — which is why today's bundle change to gateway
(`external_tls_mode: letsencrypt`, port 80) never reached the running cluster: kernels rolled
into existing peers and kept the stale ConfigState.

The git-committed bundle is the single source of truth (config-as-code): a changed bundle value
always becomes live on the next kernel restart or roll, for every service. A config written into
a ConfigState root outside the bundle is transient — it applies immediately and is overwritten
from the bundle at the next kernel restart.

That includes `kernel.UpdateOpaConfig`, which `pkg/reconciler`'s `pushOpaConfig` calls. The RPC
is kept: it is the fast path that gets a changed `services/opa.yaml` live without a kernel roll,
and operator re-pushes on each reconcile, so a kernel restart that reverts opa's root to
kernel's own bundle is followed by operator pushing its config back. To change a config durably
in kernel's own view, change kernel's bundle.
