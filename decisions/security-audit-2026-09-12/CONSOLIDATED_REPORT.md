# operator Security & Quality Audit — Consolidated Report
2026-09-12 — multimodal review: Fable 5.1 (Claude), Codex CLI (GPT), Grok 4.6 (via GitHub Copilot CLI)
Scope: `pkg/` + `cmd/` (40 Go files). `model/` (tacplus-generated) and `target/` excluded;
`charts/`/`helm/` (Helm templates) checked opportunistically, not the main focus.

The operator manages Helm releases and fleet config for every other service — bootstrap config,
service delegates, OPA policy, TLS cert rotation, self-upgrade. High blast radius by nature.

---

## Correction — a Critical claim that does not hold up

**Codex reported a Critical finding: "private keys are still published through ConfigMaps in the
committed Cavalier chart" citing `charts/cavalier/templates/{kernel,kmaker,gateway}/configmap-0.yaml`.
I checked this directly and it's [OVERSTATED/WRONG] as stated.** Those template files contain no
literal key material — they're thin Helm templates (`config.yaml: {{ .Values.kernel0.config | quote }}`)
whose default `values.yaml` placeholder is an explicit empty string (`config: ""`), documented as
existing only to prevent a nil-pointer during dry-run. Fable independently checked the same
question from the runtime-data-flow side and concluded the opposite: "the gateway external key...
and the kmaker signing key... are both documented and packaged as Secrets, so they are working as
intended." I confirmed via `pkg/helm/generator.go`: a `ConfigHasKeyMaterial` flag explicitly routes
kernel/kmaker/gateway configs into Kubernetes **Secret** objects, not ConfigMaps, specifically
because their configs embed private keys. Don't act on Codex's version of this claim.

**The real version of this issue is different and just as serious — see Critical #1 below.**

---

## Critical — real, personally verified

### 1. Any app service granted `llm_credential_services` gets its secret credential deployed into a plain ConfigMap [VERIFIED]
**Files:** `pkg/bootstrap/deploy.go:248-260`, `pkg/helm/generator.go:288-294` (the `ConfigHasKeyMaterial`
field and its doc comment), `pkg/helm/generator.go:1238` (`configVolumeSource` — the actual
Secret-vs-ConfigMap routing)
Found by Grok, and I traced the whole mechanism myself. `ConfigHasKeyMaterial` is a per-service
flag that, when true, packages that service's config as a Kubernetes Secret instead of a ConfigMap
— its own doc comment explains this is specifically because a ConfigMap "is not secret storage."
It's hardcoded `true` only on kernel, kmaker, and gateway's `serviceSpec` entries. But
`deploy.go:248-260` shows that **any** service named in `cfg.GetLlmCredentialServices()` gets the
real `llm_credential` value (the Anthropic API key, per today's earlier incident) injected into
its config regardless — and that service's `serviceSpec` never sets `ConfigHasKeyMaterial`, so its
config still routes into a plain ConfigMap. **This is the structural, generalized version of the
exact plaintext-API-key exposure already found live today** (`schematic-operator-config`) — it
isn't a one-off misconfiguration, it's how the generator is built: opting a service into
`llm_credential_services` is currently guaranteed to expose that credential in a ConfigMap.

---

## High

### 2. `gen-backend-tokens` is completely non-functional [cross-confirmed by all three tools]
`pkg/bootstrap/backend_tokens.go:33-37` declares an empty provider list, so the command always
fails with "no tokens found" regardless of what environment variables are actually set — all three
tools independently found this exact bug (Codex #21, Fable #11, Grok #5, Grok rating it 10/10
confidence). `BackendTokenPolicyDir` is also referenced nowhere else in the codebase per Fable's
check — the feature appears entirely disconnected/dead.

### 3. Stale pending-release recovery has two gaps beyond today's already-fixed one [VERIFIED]
**File:** `pkg/reconciler/reconciler.go:493-516` (`rollbackTargetRevision`)
I read this function directly — it's the exact code fixed earlier today
(`operator@0148e11`/`recoverStalePendingRelease`), and the fix only handles the case where a
`superseded` revision exists in history. Two siblings remain, found independently by Fable and
Grok (both flagged the same two gaps):
- **A completely fresh install with zero prior history** (`pending-install`, nothing else) has no
  `superseded` revision to fall back to — `target` stays nil, the release is stuck permanently.
- **A predecessor marked `failed` rather than `superseded`** (a plausible sequence: initial install
  fails, an upgrade attempt starts and also dies) is filtered out by the same
  `rel.Info.Status != StatusSuperseded` check — also stuck permanently.

Both are real production-availability bugs in the operator's own self-recovery logic, not
security vulnerabilities, but directly relevant given today's fix to the adjacent case.

### 4. Path traversal in multiple config/bundle-loading paths [Codex + Fable cross-confirmed]
- `pkg/bootstrap/bundle.go` (`ExpandBundle`) — a bundle service key like `../../authorized_keys`
  is joined directly under the expansion directory with no cleaning.
- `pkg/bootstrap/init.go` (remote OPA config templates) — a template map key like `../kernel.yaml`
  is joined directly onto `services/opa/`, potentially overwriting kernel delegates or crypto
  config on the machine running `init config`.
- `pkg/bootstrap/config.go` / `pkg/bootstrap/configs.go` — app-service names containing path
  separators become filesystem paths without validation.
All three require a malicious/compromised bundle or config-template source to exploit — same
general shape as the tacplus `output-dir` deletion bug fixed earlier today. Worth the same kind of
containment-check fix.

### 5. Certificates are never checked for expiry, only presence [Codex + Fable cross-confirmed]
Kernel, gateway, and kmaker certificate regeneration logic (`pkg/bootstrap/deploycrypto.go`) checks
only whether a cert exists and has the right SPIFFE shape — never `NotAfter`/remaining lifetime.
Fable notes the certs are minted with a 1-year TTL and there is no `NotAfter` comparison anywhere
in `pkg/`. Twelve months after cluster init, expired certs would keep being redistributed with no
error, and mesh mTLS would start failing with no actionable signal.

### 6. Self-upgrade image-reference construction has real bugs [Fable + Grok, different angles]
- Fable: taking container index zero's image and swapping only the tag breaks if a sidecar
  precedes the operator container, or if a mirror-prefixed image doesn't carry the resolved tag.
- Codex: for a digest-pinned image (`repo/operator@sha256:<digest>`), the tag-swap logic's
  last-colon split produces an invalid reference.
- Grok, separately: the 30-second reconcile ticker doesn't actually wait for the operator's own
  restart after a self-patch, contradicting the code's own comment about sequencing.

### 7. Stale-workload cleanup can delete resources it doesn't own, and failed partial cleanup is never retried [Codex]
Ownership for pruning is determined solely by the non-unique label
`app.kubernetes.io/managed-by=schematic-operator` — any Deployment carrying that label, even one
from an unrelated release, gets deleted if its name isn't in the current expected set. If deleting
the Deployment succeeds but deleting its associated Secret/Service fails, that failure is never
retried and the orphaned resource (potentially credential-bearing) is never seen again.

### 8. Operator RBAC objects have fixed, cluster-global names [Codex, not independently re-verified]
A second operator installation in a different namespace would collide with and overwrite the
first's ClusterRoleBinding subject. If accurate, this is a real availability/security concern for
any multi-tenant-operator deployment model — worth confirming against actual deployment plans
before treating as urgent.

### 9. ClusterRole permits deleting arbitrary ClusterRoles, not just the ones it's meant to manage [Grok, not independently re-verified]
`helm/templates/clusterrole.yaml` grants `*` verbs (including delete) on `clusterroles`/
`clusterrolebindings` generally, per Grok's reading, rather than scoped to the specific resources
the operator actually needs to manage.

---

## Medium (not individually re-verified, listed for completeness — see raw files for full detail)
- `EnsureServiceDelegates` allegedly never called on the in-cluster reconcile path, only on the
  CLI path — new services would deploy without kernel service-account matchers (Grok).
- `deploy.list` filtering doesn't propagate to app services or to the actual chart generator in
  all code paths, so excluded services can still get deployed with no config (Fable + Grok both
  flag versions of this).
- Registry-tag resolution can silently downgrade the whole fleet if the newest tag disappears
  (Fable + Grok cross-confirmed), and pagination assumes an absolute `Link` URL where the real
  registry API returns relative paths.
- Several config-file permission issues (0644 instead of 0600) on files containing key material,
  paralleling the ConfigMap issue above.
- Config hot-reload caches the first-loaded PKI/crypto permanently in memory, ignoring later
  on-disk rotation until process restart (Fable + Grok cross-confirmed) — same shape as findings
  in kernel/oauth/saml today about stale cached crypto material.
- Kernel node-scale-up regenerates every node's TLS cert (not just the new one) while leaving the
  new node without a signing CA, and the fix doesn't persist to disk in-cluster (Fable).
- Auto-upgrade service list omits several real services (`messaging`, `mailbox`, `personalauth`),
  which stay pinned to a fallback tag indefinitely (Grok).
- Unpinned `busybox:latest` base image in an init container (Grok).
- A dead, unreferenced Deployment-rendering code path that would mislead a future reader
  (Fable) — same "confusing dead code" pattern as tacplus's unused auth interceptors today.

## Recommended next step
Fix #1 (app-service credential-in-ConfigMap) first — it's the direct structural cause of an issue
already known and being worked on today. Then the pending-release recovery gaps (#3), given
there's already a fresh, well-understood fix pattern to extend. Path traversal (#4) and cert-expiry
(#5) next given severity; the rest can wait.

## Raw per-tool output
- Codex: `codex.out.md` (21 findings, includes the one corrected Critical claim above)
- Fable 5.1: `fable.out.md` (16 findings, explicitly notes what it checked and ruled out as
  "working as intended")
- Grok 4.6: `grok.out.md` (first pass, cut short by Copilot CLI's internal timeout despite very
  extensive file-reading) + `grok2.out.md` (completed continuation, 22 findings)
