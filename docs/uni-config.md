# The uni-config: one document defines a deployment

A deployment is defined by one document, the **uni-config** (#17, Jody 2026-11-01). It says which services run, at which
pins, with which settings, its identity and trust material, its sign-in providers and its secrets. The operator consumes
only that document. No ConfigMap, no other Secret and no environment variable carries a deployment's configuration or
secrets.

## The schema

`operatorv1.UniConfig`, in [proto/services/uniconfig.proto](../proto/services/uniconfig.proto). The version is in the
package name: a change that breaks a deployment's document is a new package (`operatorv2`), never a new meaning for an
existing field. The package is `operatorv1`, not `operator.v1`, because tacplus names the Go package after the proto
package and a dotted name is not a Go identifier.

| Field | What it holds | Where it came from before |
|---|---|---|
| `operator` | the operator's `Config`: topology, FQDN, mode, optional services, tenants, `image_digests`, `local_images` | `operator.yaml`, `images.yaml` |
| `services` | each service's config, keyed by service name, as YAML text | `services/<name>.yaml` |
| `tenants` | each tenant's `oauth.OAuthTenantConfig`, keyed by tenant name | `tenants/<name>/oauth.yaml` |
| `opa_policies` | the OPA entitlements policy: each file under `services/opa/`, keyed by its path | `services/opa/**` |
| `secrets.pki` | the deployment's PKI | `crypto.yaml` |
| `secrets.llm_credential`, `secrets.gitlab_registry_token` | the shared LLM credential, the registry token | `crypto.yaml` |
| `secrets.oauth`, `secrets.mailbox` | each service's own `Secrets` message | the `<svc>-pipeline-secrets` Secrets the pipeline wrote |

Everything a protected variable holds goes under `secrets`. The committed part (`operator`, `services`, `tenants`,
`opa_policies`) carries no secret of the operator's own: `operator.pki`, `operator.llm_credential` and
`operator.gitlab_registry_token` are refused there. A service's own config can still have secret fields (oauth's
providers' `client_secret`, for example). Those travel in that service's `services` entry: the pipeline fills them in
from its variables, in memory, and the whole document is a Secret.

`deploy.list`, the local development filter of `set-deploy-list`, is not part of a uni-config.

## Validation

`schematic-operator validate <file|->` is what the pipeline's `diff` stage runs before apply. It refuses:

- **an unknown field**, named by its path, wherever it is (`operator.topology.kmaker_nodes (retired)`,
  `secrets.oauth.apple_key`). The generated parser would drop it silently, so validate checks every key against the
  message first.
- **a missing required secret**, by its name: `secrets`, `secrets.pki` and `secrets.pki.root_ca` always, and a secret
  that the config uses: `secrets.oauth.apple_signin_key_p8` when an oauth provider is a Sign in with Apple provider,
  `secrets.mailbox.gmail_service_account_json` when mailbox's `gmail` is set.
- **secret material in the committed part** (above).
- **a deployment that does not build**: each service's config is checked against that service's `Config` in the
  registry the operator embeds (an unknown service key is named), and the generated configs must satisfy what their
  consumers enforce, as `generate` checks them.

Nothing of the document is printed.

## The carrier

One namespaced Secret, **`uni-config`**, in the deployment's namespace. Its single key, `uni-config`, holds the
serialized `UniConfig`: YAML, JSON or binary proto. A Secret and not a custom resource, because a CRD is cluster-scoped
to install and a deployment has no rights outside its namespace (decisions/deployments-per-cluster-2026-10-26.md in
tacplus-dev/roadmap). A Secret also matches the content, which carries secrets.

## The pipeline

The deployment's pipeline (tacplus-dev/fleet-seatrials#28) builds the uni-config in the job's memory. It combines the
committed config dir and the project's protected CI/CD variables, then validates it and applies it as the Secret. It
never saves the document as an artifact and never prints it. The operator offers the assembly:

```sh
schematic-operator uni-config assemble -c deploy/bootstrap \
    --crypto-from-env FLEET_CRYPTO_YAML \
    --secret-from-env oauth.apple_signin_key_p8=FLEET_APPLE_KEY_P8 \
    --secret-from-env mailbox.gmail_service_account_json=CREW_MAIL_SA_KEY_JSON
```

`assemble` reads the config dir: `operator.yaml`, `images.yaml`, `services/`, `tenants/`, and `crypto.yaml` if it is
there. `--crypto-from-env` reads `crypto.yaml`'s content from a variable instead of a file. Each `--secret-from-env`
sets one field of `secrets` from a variable, and an unknown field path or an unset variable is refused. The YAML goes
to stdout, to be piped into `validate -` and into the Secret, for example
`kubectl create secret generic uni-config --from-file=uni-config=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`.

A secret or setting changes by assembling and applying a new uni-config.

## The operator

The operator chart ([helm/](../helm)) mounts the `uni-config` Secret and runs the operator with
`--uni-config=/etc/schematic/uni-config/uni-config`. It renders no config of its own from helm values, so no secret is
stored in a helm release record.

With `--uni-config`, any command (the in-cluster operator, `generate`, `apply`) parses and validates the document. It
then expands the document into its config directory: `--dir`, which must be empty, or a private temporary one. Every
command works from that directory, and the operator writes its working state (`configs/`, the kernel delegates) there.
The directory is rebuilt from the uni-config at every start and is never a second source. The secret files in it
(`crypto.yaml`, `service-secrets.yaml`) are readable by their owner only. `--uni-config -` reads stdin, so the CLI can
run straight from the pipeline's in-memory document.

The in-cluster operator polls its mounted uni-config. When the pipeline applies a new one and the kubelet refreshes the
mount, the operator restarts and renders from it. Helm then changes only the objects whose content changed, and a pod
rolls only when its own config or secrets changed. Each service's `Secrets` from `secrets` is rendered into that
service's own Secret, `<service>-secrets`, which only that service's pods read. Its content is a checksum on their pod
template.

## Environment variables

The goal is no configuration or secret in any pod's environment. Under the interim allowlist (the hub's, until Jody
decides), only downward-API pod identity stays: `NODE_NAME` and `POD_NAME`. Each variable goes away when the service
that reads it reads a typed field instead:

| Env var | Status |
|---|---|
| `NODE_MODE` | gone (#17): it had no reader since timedb and graphdb went |
| `SCHEMATIC_OPERATOR_DIR`, `POD_NAMESPACE`, `SCHEMATIC_LOCAL_IMAGES` (operator pod) | gone (#17): `--dir`, the service account's namespace mount, `Config.local_images`. The CLI still honours `SCHEMATIC_OPERATOR_DIR` and `SCHEMATIC_LOCAL_IMAGES` from the shell that runs it. |
| `PROTO_SERVICES_*_SECRETS_*` | values from the uni-config's `secrets` (#17); still env until the generated `Secrets` reader takes a file (tacplus-dev/tacplus#262) |
| `KERNEL_K8S_TOKEN_FILE`, `KERNEL_K8S_TOKEN_AUDIENCE`, `BOOTSTRAP_CONFIG`, `SERVICE_CONFIG` | until the chassis reads them from config (tacplus-dev/tacplus#262) |
| `STRATUM_SEED_MEMBERS`, `STRATUM_WS_SEED_MEMBERS`, `STRATUM_MEMBER_ROLE` | until stratum's `Config` carries them (tacplus-dev/stratum#23) |
| `BLOBSTORE_PEERS` | until blobstore's `Config` carries them (tacplus-dev/blobstore#10) |
| `JAVA_OPTS`, `GOMEMLIMIT` | runtime sizing: Jody's call whether they stay |
| `WEB_ROOT` | dev-only gateway hot reload (`set-web-root`) |

The check that a fleet's pods carry no other variable is `deploy/checks.sh env` in the deployment's repository
(tacplus-dev/fleet-seatrials#28).
