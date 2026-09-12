## Findings

### Critical

1. **`charts/cavalier/templates/{kernel,kmaker,gateway}/configmap-0.yaml:1` — Private keys are still published through ConfigMaps in the committed Cavalier chart.**  
   Deploying `charts/cavalier` places kernel node/signing keys, kmaker’s signing CA key, and gateway TLS key in ConfigMaps, allowing any principal with ordinary ConfigMap-read access to recover credentials that the current generator correctly stores as Secrets.

### High

2. **`pkg/bootstrap/deploy.go:75-96`, `pkg/bootstrap/deploy.go:706-737` — The in-cluster provider permanently overrides newly loaded PKI with its first cached copy.**  
   After the first successful reconciliation, replacing `crypto.yaml` to rotate a compromised or expiring CA/certificate has no effect: every later call reloads the file and then replaces `cfg.Pki` with `pkiCache`, so old keys and trust roots continue to be deployed until the operator restarts.

3. **`pkg/bootstrap/deploycrypto.go:389-429`, `pkg/bootstrap/deploycrypto.go:617-620`, `pkg/bootstrap/deploycrypto.go:666-669` — Certificate regeneration checks only presence/SPIFFE shape, never expiration or remaining lifetime.**  
   Kernel, gateway, and kmaker certificates that are expired or close to expiry are accepted indefinitely; reconciliation keeps distributing them and services eventually lose TLS connectivity instead of rotating ahead of expiry.

4. **`pkg/reconciler/reconciler.go:329-340` — Removing the OPA configuration does not revoke the policy previously pushed to kernel.**  
   If `services/opa.yaml` or its policy bundle is removed, the provider returns `nil` and `pushOpaConfig` exits without updating kernel, leaving the old—potentially permissive—authorization policy active indefinitely.

5. **`pkg/reconciler/reconciler.go:651-680` — Stale-workload ownership is determined solely by a non-unique label.**  
   Any Deployment in the managed namespace carrying `app.kubernetes.io/managed-by=schematic-operator`, including one owned by another release or administrative workflow, is deleted whenever its name is absent from the generated expected-name set.

6. **`pkg/reconciler/reconciler.go:683-697` — Failed cleanup of a stale Deployment’s Service, ConfigMap, or Secret is ignored and never retried.**  
   If Deployment deletion succeeds but deletion of its associated Secret or Service transiently fails, the next pass no longer sees the Deployment and therefore leaves the credential-bearing Secret or externally reachable Service orphaned forever.

7. **`helm/templates/clusterrole.yaml:5-38`, `helm/templates/clusterrolebinding.yaml:1-14` — Operator RBAC objects have cluster-global fixed names despite supporting multiple managed namespaces/installations.**  
   Installing another operator release in a second namespace collides with and rewrites the same `schematic-operator` ClusterRoleBinding subject, stripping the first operator of cluster-scoped permissions or transferring those permissions to the second installation.

8. **`pkg/bootstrap/generate.go:144-153` — Bundle-based apply bypasses all configuration validation.**  
   A malformed or malicious bundle reaches the cluster generator without `ValidateConfig`; negative replica counts can panic allocation paths, while unconstrained namespaces, tenant identifiers, and application-service names reach YAML, object names, RBAC names, and chart paths.

9. **`pkg/bootstrap/bundle.go:197-223` — Bundle map keys allow path traversal during expansion.**  
   A bundle containing a service key such as `../../authorized_keys` makes `ExpandBundle` join that key directly beneath `configs/`, permitting overwrite of files outside the requested expansion directory with attacker-controlled content.

10. **`pkg/bootstrap/config.go:195-203` — Remote config-template OPA paths allow path traversal.**  
    A template entry such as `../../operator.yaml` is joined directly to `services/opa`; applying a compromised template release can overwrite arbitrary files writable by the invoking user outside the OPA directory.

11. **`pkg/bootstrap/configs.go:79-87` — Application service names become filesystem paths without validation.**  
    An `app_services` key containing path separators causes `BuildConfigOutput` to write generated configuration outside `<cfgDir>/configs`, potentially overwriting other configuration or credential files.

12. **`pkg/helm/generator.go:88-94`, `pkg/helm/generator.go:819-832`, `pkg/bootstrap/services.go:105-121` — Tenant/service concatenation is not injective, allowing Kubernetes identity collisions.**  
    Configurations such as service `a-b`/tenant `c` and service `a`/tenant `b-c`, or a tenant-owned instance colliding with a shared service name, produce the same ServiceAccount and workload name while kernel delegates retain different authorization namespaces, causing one tenant/service identity to overwrite or impersonate another.

### Medium

13. **`pkg/reconciler/reconciler.go:519-527` — Pending releases with a zero `LastDeployed` timestamp can never become stale.**  
    A crashed initial Helm install commonly has no deployment timestamp; `isStalePendingRelease` consequently returns false forever, so every reconciliation refuses recovery and the fleet remains permanently stuck in `pending-install`.

14. **`pkg/upgrade/checker.go:279-290` — Upgrade resolution accepts version regressions.**  
    If the highest registry tag is deleted or hidden, `updateCache` replaces the cached version with the newly selected lower version and notifies reconciliation, automatically rolling services and the operator back to older images.

15. **`pkg/reconciler/reconciler.go:232-258` — Self-upgrade derives the target repository from container index zero but patches the container named `schematic-operator`.**  
    If a sidecar is inserted before the operator container, the code constructs the desired image from the sidecar repository and patches that image onto the operator container, potentially replacing the cluster-powerful operator with an unrelated image.

16. **`pkg/reconciler/reconciler.go:236-243` — Digest-pinned operator images are parsed as tagged images incorrectly.**  
    For `repo/operator@sha256:<digest>`, the last-colon split produces `repo/operator@sha256:<tag>`, an invalid or unintended image reference, making self-upgrade break digest-based deployments.

17. **`pkg/reconciler/reconciler.go:357-378` — “Schedulable” node counting ignores taints.**  
    Ready nodes carrying untolerated `NoSchedule` or `NoExecute` taints are counted when deriving gateway replicas; clusters with control-plane, GPU, or dedicated nodes therefore generate gateway pods that cannot schedule.

18. **`pkg/upgrade/checker.go:332-393` — Registry requests use `http.DefaultClient` without any client timeout.**  
    A registry or proxy that accepts a connection but never completes the response can block the sole polling goroutine indefinitely, silently stopping all future upgrade detection until process shutdown.

19. **`pkg/upgrade/checker.go:227-257`, `pkg/upgrade/checker.go:207-224` — Partial registry resolution silently substitutes the operator’s tag for unresolved service images.**  
    On startup, a transient failure for one repository still counts as a successful overall resolve; that service is rendered with the operator image’s tag, which may not exist in its repository, putting only the affected workload into persistent `ImagePullBackOff`.

20. **`pkg/bootstrap/init.go:27-32`, `pkg/bootstrap/deploy.go:75-106`, `pkg/reconciler/reconciler.go:272-287` — Runtime configuration is split between a startup-only config and independently reloaded service config.**  
    If mounted configuration changes without restarting the process, service configs and certificates are rebuilt from the new topology while chart generation continues using the original `r.cfg`, producing mismatched replica counts, certificate arrays, delegates, and deployed workloads.

21. **`pkg/bootstrap/backend_tokens.go:36-49` — `gen-backend-tokens` is unconditionally nonfunctional.**  
    Its provider list is empty, so no environment variable is ever inspected and every invocation fails with “no tokens found,” despite the command advertising four supported token variables.
