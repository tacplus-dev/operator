# Proposals: operator

Written by loopers. Each proposal: what, why, links. Jody promotes accepted ones into ROADMAP.md (via an agent);
the looper then marks it `promoted` or `declined` here with the date.

## Open
- **Drop the retired-key scrub once every crypto.yaml is rewritten** (2026-10-30, #16): `cryptoHoldsRetiredKeys`
  (pkg/bootstrap/settings.go) makes generate rewrite a crypto.yaml that still holds `pki.kmaker_signing_ca`, so the
  kmaker CA private key leaves existing config dirs. Once every deployment has run a generate with it (after the first
  release that carries #16), the check is dead weight and can go; the proto's `reserved 7` stays.

## Promoted / declined
