# extra_hostnames: extra public names, fqdn stays primary (2026-09-30)

## Decision

`operator.Config.extra_hostnames` (field 24) lists public names served in
addition to `fqdn`. The operator renders `[fqdn, extra_hostnames...]`, fqdn
first, into:

- the gateway's `external_hostnames` (kernel bootstrap, GatewayConfig), and
- letsencrypt's `hostnames`.

Nothing else reads it. `fqdn` remains the single primary name.

## The OIDC issuer does not move

The kernel seed's `issuer` is `Issuer(cfg)` (pkg/bootstrap/settings.go), built
from `cfg.Fqdn` and `cfg.Port` only; the seed's `fqdn` is `cfg.Fqdn`
(pkg/bootstrap/example.go). Neither reads `extra_hostnames`, so on pilot the
issuer stays `https://console.seabeam.net` whatever extra names are listed.
`TestExtraHostnamesReachGatewayAndLetsencrypt` asserts the issuer alongside the
rendered lists. Tokens therefore always carry the fqdn issuer, even when a
client reached the cluster through an extra name.

fqdn is also first in letsencrypt's list because letsencrypt defaults its ACME
email to `admin@<hostnames[0]>`.

## Not covered (open)

- **The certificate.** certmagic v0.25.2 does not issue multi-SAN certificates:
  `ManageAsync(list)` obtains one single-name certificate per entry, and
  letsencrypt stores only `hostnames[0]`'s for the gateway. Until that is
  resolved (see looper/schematic/workers/cutover-questions.md Q1), an extra
  name is served with the fqdn's certificate and fails TLS name checks. Do
  not set `extra_hostnames` on pilot yet.
- letsencrypt keys its stratum record by a digest of the sorted hostname list
  (`certSetID`), so adding a name makes the next start miss the stored record
  and open a new ACME account and order.
- The cluster-CA fallback edge certificate (`EnsureGatewayExternalTLS`) still
  names fqdn only.
