## Findings (ranked)

| # | Severity | File | Lines | Issue | Confidence |
|---|----------|------|-------|-------|------------|
| 1 | 🟠 HIGH | `pkg/bootstrap/deploy.go` | 248–257 | LLM credentials are rendered into **app-service ConfigMaps** (`ConfigHasKeyMaterial` is only set for kernel/kmaker/gateway). | 9/10 |
| 2 | 🟠 HIGH | `pkg/bootstrap/settings.go` | 65–81 | `SaveSettings` strips PKI/`llm_credential` but **not** `gitlab_registry_token`, so it can land in ConfigMap `schematic-operator-config`. | 8/10 |
| 3 | 🟠 HIGH | `pkg/bootstrap/bundle.go` | 47–56 | Bundle `config:` keeps `llm_credential`; `crypto:` is PKI-only — split disagrees with `SaveSettings`/`SaveCrypto`. | 8/10 |
| 4 | 🟠 HIGH | `helm/templates/deployment.yaml` | 41 | Init container `chmod -R a+rX /config` makes extracted bundle (including `creds/`) world-readable. | 8/10 |
| 5 | 🟠 HIGH | `pkg/bootstrap/backend_tokens.go` | 33–35 | `providers` is an **empty slice**; command always errors even when documented env vars are set. | 10/10 |
| 6 | 🟠 HIGH | `pkg/bootstrap/init.go` | 51–114 vs `generate.go` | In-cluster path never calls `EnsureServiceDelegates`; new services deploy without kernel SA matchers and fail token exchange. | 9/10 |
| 7 | 🟠 HIGH | `pkg/bootstrap/generate.go` | 256–273 | CLI `apply` has **no** stale pending-release recovery (sibling of today’s reconciler fix). | 9/10 |
| 8 | 🟠 HIGH | `pkg/reconciler/reconciler.go` | 488–516 | Pending **install** with no prior revision still cannot recover; loop errors forever after 5 minutes. | 9/10 |
| 9 | 🟠 HIGH | `pkg/bootstrap/kernelidentity.go` | 31–49 | `startKernelIdentity` is one-shot; if `sm.New()` gives up, OPA is never pushed until process restart. | 8/10 |
| 10 | 🟡 MEDIUM | `pkg/helm/generator.go` | 1810–1815 | `WebRoot` is interpolated raw into `hostPath` (YAML injection / host FS read if `operator.yaml` is writable). | 8/10 |
| 11 | 🟡 MEDIUM | `pkg/bootstrap/example.go` | 189 | `WriteServices` writes `kernel.yaml` (embedded TLS key) as **0644**. | 8/10 |
| 12 | 🟡 MEDIUM | `pkg/upgrade/services.go` | 6–22 | Auto-upgrade list omits `messaging`, `mailbox`, `personalauth` (and app services); they stay on fallback tag. | 9/10 |
| 13 | 🟡 MEDIUM | `pkg/bootstrap/deploy.go` | 613–641 | `FilterTopology` never zeros `AppServices`; `deploy.list` cannot hide app workloads. | 9/10 |
| 14 | 🟡 MEDIUM | `pkg/bootstrap/upgrade.go` | 294–355 | `upgrade` replaces the operator binary with **no checksum/signature** check. | 8/10 |
| 15 | 🟡 MEDIUM | `helm/templates/deployment.yaml` | 34 | Unpinned `busybox:latest` init container. | 8/10 |
| 16 | 🟡 MEDIUM | `pkg/reconciler/reconciler.go` | 519–526 | `LastDeployed.IsZero()` ⇒ never stale ⇒ pending release never recovered. | 7/10 |
| 17 | 🟡 MEDIUM | `pkg/helm/generator.go` | 872–880 | `ExpectedDeploymentNames` always uses `<svc><i>` even for StatefulSets (`kernel`/`stratum`). Harmless for prune today; will mis-prune if those ever become Deployments. | 8/10 |
| 18 | 🟡 MEDIUM | `pkg/bootstrap/example.go` | 244–252, 418–426 | Default OPA trusted issuers read `providers[].issuer`; tenant path uses proto `issuer_url`. Empty trusted-issuer set if oauth YAML uses proto names. | 8/10 |
| 19 | 🟡 MEDIUM | `helm/templates/clusterrole.yaml` | 26–31 | ClusterRole `clusterroles`/`clusterrolebindings` `*` (incl. delete) — any ClusterRole, not just `*-tokenreview`. | 8/10 |
| 20 | ⚪ LOW | `pkg/helm/generator.go` | 1015–1021 | Dead `waitForKernel` still targets **`kernel0:5000`**; real kernel is `kernel-0.kernel`. | 9/10 |
| 21 | ⚪ LOW | `pkg/bootstrap/kernelidentity.go` | 69–77 | Temp bootstrap dir never removed; `Setenv` from a goroutine. | 8/10 |
| 22 | ⚪ LOW | `pkg/bootstrap/deploy.go` | 709–743 | First successful PKI/kmaker TLS is cached for process life; on-disk `crypto.yaml` rotation is ignored until restart. | 8/10 |

**Already-fixed (not re-reported):** pending-upgrade rollback when a superseded revision exists.

---

Follow-up (reply with a number):
1. Fix highest-severity issues
2. Fix all issues
3. Commit a summary of findings (`SECURITY-REVIEW.md`)

