# Proposals: operator

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **<title>** (<date>, <issue/run link>): <what and why>
- **Refuse an operator.yaml `kmaker.spiffe_prefix` that differs from the fleet trust domain** (2026-10-29, #14):
  #14 gives kernel, every service and kmaker one trust domain (operator.yaml `spiffe_prefix`, else kernel.yaml
  `spiffe_trust_domain`). But `BuildKmakerConfig` keeps an explicit `kmaker.spiffe_prefix` in operator.yaml when
  one is set, so kmaker could still end up on a different domain from kernel. `generate` should refuse that
  mismatch as well.

## Promoted / declined
