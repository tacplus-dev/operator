# FIX: High-tier findings 2 & 3 — `gen-backend-tokens` and stale pending-release recovery

Date: 2026-09-12
Source: `decisions/security-audit-2026-09-12/CONSOLIDATED_REPORT.md`, High findings #2 and #3.
Scope: `platform/services/operator` only. Tree left edited and uncommitted for looper review.

---

## Finding 2 — `gen-backend-tokens` was completely non-functional

### What was wrong

`pkg/bootstrap/backend_tokens.go` declared `providers := []struct{ name, env string }{}` — an
empty literal. Every environment variable documented in the command's own `Long:` help text
(`ANTHROPIC_TOKEN`, `GITLAB_TOKEN`, `OPENAI_TOKEN`, `GITHUB_TOKEN`) was therefore never read, the
`tokens` slice was always empty, and the command unconditionally returned
`"no tokens found in environment — set at least one of: ..."`. Cross-confirmed by all three audit
tools (Codex #21, Fable #11, Grok #5 at 10/10 confidence).

### What changed

1. Populated `providers` with the four `{name, env}` pairs the help text promises:

   | provider key (Rego map key) | environment variable |
   |---|---|
   | `anthropic` | `ANTHROPIC_TOKEN` |
   | `gitlab` | `GITLAB_TOKEN` |
   | `openai` | `OPENAI_TOKEN` |
   | `github` | `GITHUB_TOKEN` |

   The provider keys are the lowercase proxy-service names already used elsewhere in the
   operator (`pkg/helm/generator.go:866` names the app proxy services as "anthropic, openai,
   gitlab, github"). They become the keys of the generated `tokens := {...}` Rego map, which
   `eval` looks up by `input.provider`.

2. Split the env-read + policy-write half of `runGenBackendTokens` into
   `writeBackendTokenPolicy(dir string) (string, []string, error)`, returning the written policy
   path and the provider names. `runGenBackendTokens` now calls it, prints the same two lines as
   before, and still calls `BuildConfigOutput(dir)`.

   **Why the extraction:** it is the minimum seam that makes the fix testable. The original
   function ends in `BuildConfigOutput(dir)`, which requires a fully populated, valid config
   directory — a unit test for "does an env var reach the policy file" would otherwise have had to
   stand up the entire config tree. No behaviour changed: same output, same messages, same order,
   same `0600` file mode.

### Known remaining gap (NOT fixed — out of the dispatched scope)

Fable's finding #11 also noted that the generated policy is never consumed: `BackendTokenPolicyDir`
is referenced only from `backend_tokens.go`, and `pkg/bootstrap/example.go:407` actively
`delete(m, "backend_token_policy")`s that key while loading the entitlements and proxy policy
directories — so nothing loads `services/opa-backend-token/backend_token.rego` into the OPA config.
This fix makes the command do what its help text says; wiring the policy into the OPA config load
path is a separate change and was not in the dispatched scope. **Flagging it for the looper** — the
command is now functional but its output is still dead weight until that load path exists.

---

## Finding 3 — stale pending-Helm-release recovery: two remaining dead ends

### What was wrong

`rollbackTargetRevision` (added by `operator@0148e11`, which closed the
"upgrade died after the predecessor was marked superseded but before the new revision reached
deployed" case found by scotty's `operator-rebuild-deploy4`) picked a target by: the currently
`Deployed()` revision, else the newest `StatusSuperseded` history entry with
`Version < latest.Version`. Neither existing meant `(nil, err)` and a permanent failure from
`recoverStalePendingRelease`. Two real cases hit that:

1. **Fresh install crashed mid-flight** — a `pending-install` with no prior revision of any status.
   There is nothing to roll back to at all.
2. **The only predecessor is `Failed`, not `Superseded`** — e.g. the first install failed, then an
   upgrade attempt also died. The status filter rejected it even though a real prior revision exists.

### What changed (the approach decided in the dispatching looper session)

- `rollbackTargetRevision` now accepts `StatusFailed` as well as `StatusSuperseded` candidates,
  still newest-first among entries with `Version < latest.Version`. A failed predecessor's chart
  and values are a real, previously-attempted revision; Helm's own `rollback` action accepts any
  prior revision regardless of its terminal status, so rolling back to it is strictly better than
  staying stuck.
- A new sentinel `errNoRollbackTarget` distinguishes "no prior revision exists at all" from a real
  history-query failure. **Sentinel chosen over a boolean return** because the function already has
  an `(*Release, error)` signature with a genuine error path (the `History()` failure) that must
  stay distinguishable; a third boolean return would have made every call site read worse.
- `recoverStalePendingRelease` checks `errors.Is(err, errNoRollbackTarget)` and, in that case,
  delegates to a new `uninstallStalePendingRelease`, which logs at `Warn` (matching the existing
  rollback path), runs `action.NewUninstall(cfg)` against `releaseName` with the same 5-minute
  timeout the rollback path uses, and returns `recovered = true`. The next reconcile then sees
  `helmdriver.ErrReleaseNotFound` from `Releases.Last` and performs a clean `helmInstall`. A real
  history-query error still returns the same permanent-failure error as before.
- `isStalePendingRelease` is untouched: both new paths still only fire once a pending release has
  been stuck for `StalePendingReleaseAge`.

### Reviewing the approach

Implementing it surfaced no reason the decided approach is wrong. One note worth recording:
uninstalling is not a no-op on cluster state — it deletes whatever resources the crashed first
install did manage to create. That is the correct outcome here (there is no earlier desired state to
return to, and a half-created release is not a state worth preserving), and it only happens for a
release that has been pending past `StalePendingReleaseAge` *and* has no prior revision of any
status, i.e. unambiguously an interrupted first install. `KeepHistory` is left at its default
(`false`) so the stuck revision record is removed along with the resources.

---

## Tests

### `pkg/bootstrap/backend_tokens_test.go` (new)

Matches the package's existing plain-subtest style (`settings_test.go:76` is the only
table-driven case in the package; the table here is a small `for _, tc := range []struct{...}`
in the same shape).

- `TestWriteBackendTokenPolicyReadsDocumentedEnvVars` — one subtest per documented variable: clear
  all four, set one, assert the returned provider list and that the generated `.rego` contains
  `"<provider>": "<token>",`. This is the dispatched success criterion
  (`ANTHROPIC_TOKEN=sk-ant-test` → a policy file containing that token).
- `TestWriteBackendTokenPolicyIncludesEveryProviderSet` — all four set, asserts all four providers
  in declaration order.
- `TestWriteBackendTokenPolicyWithoutAnyToken` — none set still errors.

### `pkg/reconciler/reconciler_test.go`

Reuses the existing `stalePendingReleaseStorage` / `releaseRevision` fixtures (in-memory Helm
storage driver + `kubefake.PrintingKubeClient`) added by `0148e11` — no new fixture invented.

- `TestRecoverStalePendingReleaseWithoutAnyPriorRevision` — **rewritten**. It previously asserted
  that a lone stuck revision produces an error; that is exactly the dead end being fixed, so it now
  asserts recovery succeeds and the stuck release is gone or `uninstalled`. Changed status to
  `StatusPendingInstall` to match the real first-install case.
- `TestRecoverStalePendingReleaseWithFailedPredecessor` (new) — `Failed` rev 1 + `PendingUpgrade`
  rev 2: `rollbackTargetRevision` picks rev 1, and recovery produces a deployed rev 3.
- `TestRollbackTargetRevisionPrefersNewestCandidate` (new) — `Superseded` 1 + `Failed` 2 +
  `PendingUpgrade` 3 picks 2, pinning newest-first ordering now that `Failed` is eligible.
- `TestRollbackTargetRevisionWithoutAnyPriorRevision` (new) — asserts the sentinel specifically,
  so "no candidate" stays distinguishable from a history-query error.

---

## Verification (actual output)

```
$ go build ./...
BUILD_OK
$ go vet ./...
VET_OK
```

`gofmt -l pkg/` lists only pre-existing unformatted files (`pkg/bootstrap/deploy.go`,
`deployconfig.go`, `deploycrypto.go`, `generate.go`, `upgrade.go`, `validate_output.go`,
`validate_output_test.go`, `pkg/helm/generator.go`, `generator_test.go`, `pkg/upgrade/services.go`).
None of the three files touched here appear; they were not reformatted and the pre-existing files
were left alone.

### `go test ./...`

Two failures, both pre-existing:

```
--- FAIL: TestEnsureServiceDelegatesAppServiceGetsBroker (0.26s)
    delegates_test.go:425: tasks delegate authorized_services [tasks kernel] missing eventbus
--- FAIL: TestDefaultOPAPolicyUsesGeneratedGrantNames (0.00s)
    opa_test.go:85: defaultMainRego grants "ECHO_ANY", which is not in the generated catalog
    opa_test.go:85: defaultMainRego grants "TASKS_ANY", which is not in the generated catalog
    opa_test.go:85: defaultMainRego grants "QUERY_ANY", which is not in the generated catalog
FAIL	.../pkg/bootstrap	7.389s
```

Confirmed pre-existing by running the full suite **before** making any change: the tree had no
modifications to tracked files at that point (`git status --porcelain` showed only the untracked
`decisions/` directory), so no stash was needed and the stash stack was never touched. The identical
two failures appeared then. `pkg/helm`, `pkg/reconciler`, `pkg/upgrade`, `model/*` all pass.

### New tests fail against the pre-fix code

Finding 2 — restored HEAD's empty provider list (keeping only the extracted function so the package
still compiles) and ran the new tests:

```
--- FAIL: TestWriteBackendTokenPolicyReadsDocumentedEnvVars/ANTHROPIC_TOKEN
    backend_tokens_test.go:35: writing backend token policy: no tokens found in environment — set at least one of: ANTHROPIC_TOKEN, GITLAB_TOKEN, OPENAI_TOKEN, GITHUB_TOKEN
    ... (same for GITLAB_TOKEN, OPENAI_TOKEN, GITHUB_TOKEN)
--- FAIL: TestWriteBackendTokenPolicyIncludesEveryProviderSet
    backend_tokens_test.go:61: writing backend token policy: no tokens found in environment — ...
```

Finding 3 — restored `git show HEAD:pkg/reconciler/reconciler.go` and ran the new tests (the
sentinel test was temporarily removed, since it cannot compile without `errNoRollbackTarget`):

```
--- FAIL: TestRecoverStalePendingReleaseWithoutAnyPriorRevision
    recovering stale pending release: helm release schematic revision 1 is stale pending-install but no rollback revision is available: "schematic" has no deployed releases
--- FAIL: TestRecoverStalePendingReleaseWithFailedPredecessor
    picking rollback target with a failed predecessor: "schematic" has no deployed releases
--- FAIL: TestRollbackTargetRevisionPrefersNewestCandidate
    expected revision 2 to be the rollback target, got 1
```

Both files were restored immediately afterwards; `go build ./...` clean after restore.

### Post-fix

```
$ go test ./pkg/reconciler/ -run 'StalePending|RollbackTarget' -v
PASS: TestIsStalePendingRelease
PASS: TestIsStalePendingReleaseSkipsFreshPendingRelease
PASS: TestIsStalePendingReleaseSkipsDeployedRelease
PASS: TestRecoverStalePendingReleaseWithoutDeployedRevision
PASS: TestRecoverStalePendingReleaseWithoutAnyPriorRevision
PASS: TestRecoverStalePendingReleaseWithFailedPredecessor
PASS: TestRollbackTargetRevisionPrefersNewestCandidate
PASS: TestRollbackTargetRevisionWithoutAnyPriorRevision
PASS: TestRollbackTargetRevisionPrefersDeployedRevision
ok  	.../pkg/reconciler	0.799s

$ go test ./pkg/bootstrap/ -run 'WriteBackendTokenPolicy' -v
PASS: TestWriteBackendTokenPolicyReadsDocumentedEnvVars (all 4 subtests)
PASS: TestWriteBackendTokenPolicyIncludesEveryProviderSet
PASS: TestWriteBackendTokenPolicyWithoutAnyToken
ok  	.../pkg/bootstrap	0.930s
```

The `gen-backend-tokens` CLI path was not exercised end-to-end (its tail calls
`BuildConfigOutput`, which needs a complete valid config directory); the unit test covers the
documented success criterion directly, which the dispatch allowed as the equivalent.

---

## Open questions / defaults taken

1. **Provider key names.** The help text names only the environment variables, not the Rego map
   keys. I used `anthropic`/`gitlab`/`openai`/`github` — the lowercase proxy-service names used
   throughout the operator. Nothing in-tree consumes this policy yet (see the remaining gap above),
   so there is no existing consumer to contradict; if a consumer later expects different keys, this
   table is the one place to change.
2. **The unconsumed policy output.** Left as-is and flagged above rather than wired in, since that
   is beyond the two dispatched findings.

## Files changed

- `pkg/bootstrap/backend_tokens.go`
- `pkg/bootstrap/backend_tokens_test.go` (new)
- `pkg/reconciler/reconciler.go`
- `pkg/reconciler/reconciler_test.go`
