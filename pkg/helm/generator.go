// Package helm generates Helm chart manifests for all schematic services.
//
// The Generator produces a complete chart — Chart.yaml, values.yaml, and all
// service templates — either entirely in memory or written to an output directory.
//
// Chart structure produced:
//
//	schematic/
//	  Chart.yaml
//	  values.yaml
//	  templates/
//	    namespace.yaml
//	    _helpers.tpl
//	    kernel/
//	    gateway/       (one Deployment per gateway_nodes)
//	    eventbus/
//	    oauth/
//	    ... (optional services when enabled)
package helm

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	protoHarness "gitlab.com/seabeam/schematic/platform/services/operator/model/types/clients/harness"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// ManagedByLabel is the label key every generated chart resource carries, and
// ManagedByValue is the value the chart's "schematic.labels" helper applies.
//
// The value is NOT "Helm": Helm 3 sets app.kubernetes.io/managed-by only when a
// chart template asks for it, and this chart's helper hard-codes the operator's
// own name instead. Anything selecting on these resources (e.g. the reconciler's
// stale-deployment pruning) must match this value.
const (
	ManagedByLabel = "app.kubernetes.io/managed-by"
	ManagedByValue = "schematic-operator"
)

// NodePoolLabel is the node label that splits the cluster into a fixed core
// pool, which runs the platform, and an elastic tenant pool, which runs the
// tenant-owned dogs and grows by adding nodes. The tenant pool also carries the
// taint NodePoolLabel=TenantPool:NoSchedule so nothing but a tenant dog lands
// there and the autoscaler can empty its nodes again.
//
// Placement is rendered only when the cluster has a Ready node labelled
// CorePool (see Generate's nodePools): a cluster without labelled pools — every
// local cluster, and any cluster before its pools exist — renders exactly what
// it rendered before pools existed, rather than a fleet that cannot schedule.
//
// ServicePodLabel names the workload on every pod when pools are on. The
// per-replica Deployments' pods share no other label (each is app: <svc><i>),
// and a PodDisruptionBudget has to select all of a service's replicas.
const (
	NodePoolLabel   = "schematic.seabeam.io/pool"
	CorePool        = "core"
	TenantPool      = "tenant"
	ServicePodLabel = "schematic.seabeam.io/service"
)

// Charts is the in-memory representation of a generated Helm chart.
// Keys are relative paths within the chart directory (e.g. "templates/kernel/deployment.yaml").
type Charts struct {
	Files map[string][]byte
}

// TagResolver resolves the image tag for a service registry image.
// The proj is the GitLab registry project name and svc is the image name within
// that project. They are equal for every platform service; the split exists for
// repos that publish an image under a different name than the repo.
// Returns a full image tag string, e.g. "v1.2.12-main.3" or "latest".
type TagResolver interface {
	ServiceTag(proj, svc string) string
}

// Generator builds Helm charts from an OperatorConfig.
type Generator struct {
	// series is the image series derived from the operator binary, e.g. "1-2".
	series string
	// versionLock is the MAJOR.MINOR from the operator binary, e.g. "1.2".
	versionLock string
	// resolver resolves per-service image tags. May be nil (falls back to "latest").
	resolver TagResolver
	// tenantOwned is the set of service names registry.json marks
	// (spec.service).tenant_owned. Those services are deployed once per entry
	// in Config.tenants instead of once for the whole cluster. May be nil,
	// which means every service is shared — the fleet's state today.
	tenantOwned map[string]bool
	// pinnedTenant maps a tenant-owned service name to the single tenant
	// registry.json's (spec.service).pinned_tenant pins it to. Such a service
	// gets exactly one instance, in that tenant, instead of one per entry in
	// Config.tenants. May be nil, which means no service is pinned — the
	// fleet's state today.
	pinnedTenant map[string]string
}

// New returns a new Generator.
// series is the image series (e.g. "1-2") and versionLock is the MAJOR.MINOR
// string (e.g. "1.2"), both derived from the operator binary version at startup.
// resolver provides per-service image tag resolution; pass nil to always use "latest".
// tenantOwned names the services registry.json marks tenant_owned; pass nil for
// a fleet with no tenant-owned services.
// pinnedTenant names, per tenant-owned service, the single tenant
// registry.json's pinned_tenant pins it to; pass nil for a fleet with no
// pinned services.
func New(series, versionLock string, resolver TagResolver, tenantOwned map[string]bool, pinnedTenant map[string]string) *Generator {
	return &Generator{series: series, versionLock: versionLock, resolver: resolver, tenantOwned: tenantOwned, pinnedTenant: pinnedTenant}
}

// TenantInstanceName returns the Kubernetes object name for one tenant's
// instance of a tenant-owned service. Both the Deployment and the
// ServiceAccount carry it, and the kernel delegate for that instance points its
// kubernetes_service_accounts matcher at it.
func TenantInstanceName(svc, tenant string) string {
	return svc + "-" + tenant
}

// checkPinnedTenants fails generation when a deployed service is pinned to a
// tenant Config.tenants does not list. A pinned service has exactly one
// instance and the pin names where it lives, so an unconfigured tenant is a
// config error: rendering nothing would take the whole service out of the
// fleet silently, and rendering it into a tenant that does not exist would give
// it an identity kernel mints for nobody. An unpinned tenant-owned service
// keeps the existing behaviour — no tenants configured means no instances.
func (g *Generator) checkPinnedTenants(cfg *protoOperator.Config, topo *protoOperator.Topology) error {
	if len(g.pinnedTenant) == 0 {
		return nil
	}
	configured := make(map[string]bool, len(cfg.GetTenants()))
	for _, t := range cfg.GetTenants() {
		configured[t] = true
	}
	for _, name := range appServiceNames(topo) {
		tenant, pinned := g.pinnedTenant[name]
		if !pinned || !g.tenantOwned[name] {
			continue
		}
		if !configured[tenant] {
			return fmt.Errorf("service %q is pinned to tenant %q ((spec.service).pinned_tenant) but config.tenants does not list it: %v", name, tenant, cfg.GetTenants())
		}
	}
	return nil
}

// Generate produces the full schematic Helm chart for the given config.
// nodeCount is the number of schedulable Kubernetes nodes; it is used to
// set gateway_nodes when topology.gateway_nodes == 0.
// nodePools reports that the cluster has a core pool (see NodePoolLabel): when
// true every workload is placed on its pool and multi-replica workloads get a
// PodDisruptionBudget; when false neither is rendered.
func (g *Generator) Generate(cfg *protoOperator.Config, nodeCount int, nodePools bool) (*Charts, error) {
	topo := effectiveTopology(cfg, nodeCount)

	if err := g.checkPinnedTenants(cfg, topo); err != nil {
		return nil, err
	}

	charts := &Charts{Files: make(map[string][]byte)}

	if err := g.addChartYAML(charts, cfg); err != nil {
		return nil, err
	}
	if err := g.addValuesYAML(charts, cfg, topo); err != nil {
		return nil, err
	}
	if err := g.addHelpers(charts); err != nil {
		return nil, err
	}
	if err := g.addRegistrySecret(charts, cfg); err != nil {
		return nil, err
	}

	services := g.enabledServices(cfg, topo)
	// Annotate gateway with external port so the service template creates a LoadBalancer.
	port := cfg.Port
	if port == 0 {
		port = 443
	}
	for i, s := range services {
		if s.Name == "gateway" {
			services[i].Port = port
			services[i].WebAppHostPath = cfg.WebRoot
			break
		}
	}
	for _, svc := range services {
		if nodePools {
			svc.NodePool = CorePool
			if svc.TenantInstance {
				svc.NodePool = TenantPool
			}
		}
		if err := g.addService(charts, cfg, svc); err != nil {
			return nil, fmt.Errorf("generating %s templates: %w", svc.Name, err)
		}
	}
	if err := g.addHarness(charts, cfg); err != nil {
		return nil, fmt.Errorf("generating harness templates: %w", err)
	}
	manifest, err := pipelineSecretsConfigMap(services)
	if err != nil {
		return nil, fmt.Errorf("generating the pipeline secrets manifest: %w", err)
	}
	charts.Files["templates/pipeline-secrets.yaml"] = manifest

	return charts, nil
}

// WriteToDisk writes charts to outputDir/schematic/.
func (g *Generator) WriteToDisk(charts *Charts, outputDir string) error {
	base := filepath.Join(outputDir, "schematic")
	for relPath, content := range charts.Files {
		full := filepath.Join(base, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0600); err != nil {
			return fmt.Errorf("writing %s: %w", relPath, err)
		}
		// 0600, like the kernel bootstrap configs: this tree holds the GitLab
		// registry token (templates/registry-secret.yaml) and the service configs
		// carrying kernel's and kmaker's private keys (templates/<svc>/secret-N.yaml),
		// and it is routinely regenerated into an existing directory — WriteFile
		// leaves an already-present file's mode alone, so a chart rendered by an
		// older binary stays world-readable without this chmod.
		if err := os.Chmod(full, 0600); err != nil {
			return fmt.Errorf("securing %s: %w", relPath, err)
		}
		// A config that moved from a ConfigMap to a Secret leaves the old
		// template behind in an output dir generated before the move, and helm
		// would apply both — re-publishing the key material the Secret exists to
		// protect. Nothing else prunes this directory.
		if stale, ok := strings.CutSuffix(filepath.Base(relPath), ".yaml"); ok && strings.HasPrefix(stale, "secret-") {
			old := filepath.Join(filepath.Dir(full), "configmap-"+strings.TrimPrefix(stale, "secret-")+".yaml")
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing superseded %s: %w", old, err)
			}
		}
	}
	return nil
}

// stratumSeedMembersEnv renders STRATUM_SEED_MEMBERS: the "host:port" address of
// every stratum replica, from the StatefulSet's stable per-pod DNS.
//
// stratum's event store is a Hazelcast cluster and its members have to find each
// other. The alternative it falls back to is multicast, which does not work in
// Kubernetes: several replicas would each form their OWN cluster of size one, so
// no partitions would be shared and every pod would believe it was a cold boot
// with an empty store — replaying the entire event log, each of them.
//
// Peer wiring by env follows the former graphdb GRAPHDB_ZERO_ADDR shape rather than inventing a
// second typed-config path for what is deployment topology the operator already
// knows.
// The chart exposes Hazelcast on 5001 for every HasHazelcast service (kernel's
// convention), so stratum must bind that rather than Hazelcast's 5701 default.
const stratumHazelcastPort = 5001

// stratumWorkingSetPort is the second Hazelcast cluster in stratum's JVM: the
// working-set maps ("stratum-ws"), kept separate from the event store because
// they have their own lifecycle — idle-TTL eviction, caller-chosen map ids.
//
// Two clusters in one JVM cannot share a port, and the event store pins 5001
// with auto-increment off (a predictable peer address is the whole point of the
// seed list). So the working set gets its own pinned port, and it must be a
// DIFFERENT constant rather than "5001 + 1": auto-increment is exactly the
// mechanism that made the second instance land somewhere nobody could name.
const stratumWorkingSetPort = 5701

// grpcHealthProbePort is the plaintext grpc.health.v1 listener the kubelet's
// gRPC probe speaks to (see the grpcProbe* helpers). It is NOT a Service port,
// so nothing else in the chart names it — but a NetworkPolicy that selects a
// pod must, because the probe arrives from the NODE address like any other
// ingress and a policy that forgets it takes the pod NotReady.
const grpcHealthProbePort = 8444

// gatewayHttpPort is the container-side port the gateway binds for the edge's
// PLAINTEXT port 80 — ACME HTTP-01 challenges and the redirect to https. The
// LoadBalancer maps external 80 to it, so the container never needs
// NET_BIND_SERVICE. Must match the http_listener the operator writes into the
// gateway's config (pkg/bootstrap).
const gatewayHttpPort = 8080

// stratumSearchName is the volume-less second workload: search members, joining
// the same Hazelcast cluster as the writers with the member attribute
// stratum.role=search. Pods are stratum-search-0..M and the headless Service is
// "stratum-search", which is what puts them in the seed list below.
const stratumSearchName = "stratum-search"

// stratumMemberRoleEnv is the env var one stratum pod declares its role in. The
// writers' StatefulSet says writer explicitly rather than leaning on the binary's
// default: the two workloads run the same image, and a chart that names the role on
// one of them and not the other is a chart where the durability-bearing half is the
// one left to a default.
func stratumMemberRoleEnv(role string) string {
	return fmt.Sprintf("        - name: STRATUM_MEMBER_ROLE\n          value: %q\n", role)
}

// stratumSeedMembersEnv renders the seed lists for both of stratum's Hazelcast
// clusters, spanning BOTH workloads.
//
// Every member has to be able to find every other one, so the list is the writers
// plus the search members. Leaving the search members out would work by accident —
// they would find the writers and join through them — right up to a cold start in
// which no writer is up yet, where a search member would resolve nothing, decide it
// was alone and form its own cluster of one. That is the same failure the seed list
// was introduced for.
func stratumSeedMembersEnv(writers, searchers int32) string {
	return stratumSeedEnv("STRATUM_SEED_MEMBERS", writers, searchers, stratumHazelcastPort) +
		stratumSeedEnv("STRATUM_WS_SEED_MEMBERS", writers, searchers, stratumWorkingSetPort)
}

func stratumSeedEnv(name string, writers, searchers int32, port int) string {
	if writers <= 0 {
		return ""
	}
	var members []string
	// <pod>.<headless service>:<hazelcast port>. A StatefulSet-shaped
	// service is named for the service itself — pods are stratum-0,
	// stratum-1, ... and the headless Service is "stratum" — NOT the
	// stratum0 form a Deployment uses. Getting this wrong produces seed
	// addresses that never resolve, and Hazelcast simply never forms a
	// cluster: no error, just members that each sit alone.
	for i := int32(0); i < writers; i++ {
		members = append(members, fmt.Sprintf("stratum-%d.stratum:%d", i, port))
	}
	for i := int32(0); i < searchers; i++ {
		members = append(members, fmt.Sprintf("%s-%d.%s:%d", stratumSearchName, i, stratumSearchName, port))
	}
	return fmt.Sprintf("        - name: %s\n          value: %q\n", name, strings.Join(members, ","))
}

// blobstore's two in-cluster ports, which are `Config.http_listener`'s and
// `Config.peer_listener`'s defaults in blobstore.proto. They are stated here, not
// derived: the chart has to publish them before any config is delivered, and a
// port the chart and the service disagree about is a listener nothing can reach.
//
// NEITHER IS EXPOSED OUTSIDE THE CLUSTER. The gateway holds the edge and proxies
// `/upload/` and `/b/` to the bytes port; the peer port is dialled only by the
// other blobstore pod, at its stable per-pod DNS name.
const (
	blobstoreBytesPort = 8080
	blobstorePeerPort  = 8081
)

// blobstorePeersEnv renders BLOBSTORE_PEERS: the peer-listener address of EVERY
// replica, this pod's own included, from the StatefulSet's stable per-pod DNS.
//
// The service picks itself out of the list by hostname and treats the remainder as
// its peer, so the list is deliberately complete rather than per-pod — which is
// what lets one env value serve both pods and one ConfigMap serve the StatefulSet.
//
// Delivered by env for the same reason stratum's STRATUM_SEED_MEMBERS is: peer
// wiring is deployment topology the operator already knows, and a second typed
// config path for it would only let the two disagree.
func blobstorePeersEnv(replicas int32) string {
	if replicas <= 0 {
		return ""
	}
	var peers []string
	for i := int32(0); i < replicas; i++ {
		// <pod>.<headless service>:<peer port>. A StatefulSet's pods are
		// blobstore-0, blobstore-1 and its headless Service is "blobstore" — NOT
		// the blobstore0 form a Deployment uses.
		peers = append(peers, fmt.Sprintf("blobstore-%d.blobstore:%d", i, blobstorePeerPort))
	}
	return fmt.Sprintf("        - name: BLOBSTORE_PEERS\n          value: %q\n", strings.Join(peers, ","))
}

// appServiceNames returns the application's service names in sorted order, so
// chart output is deterministic. Declared here rather than imported from
// bootstrap: bootstrap depends on helm, and the reverse would be a cycle.
func appServiceNames(topo *protoOperator.Topology) []string {
	var out []string
	for name, n := range topo.GetAppServices() {
		if n > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// --- internal types ---

// namedPort is one additional in-cluster port: the container port and the name it
// is published under.
type namedPort struct {
	Name string
	Port int32
}

// secretEnvVar is one field delivered to a pod as an environment variable read
// from a Kubernetes Secret, instead of through kernel's combined config seed.
// See serviceSpec.SecretEnvVars.
type secretEnvVar struct {
	// EnvName is the variable the service reads. It is the name tacplus's
	// generated <Msg>FromEnvVar looks for — PROTO_<PROTO PATH>_<MESSAGE>_<FIELD>
	// upper-snaked — so it must be taken from the generated helper, not spelled
	// by hand here.
	EnvName string `json:"env"`
	// Key is the entry inside the Secret. One Secret per service holds every one
	// of its secret fields.
	Key string `json:"key"`
	// GitLabVariable is the GitLab CI/CD variable the deployment pipeline writes
	// into Key (D25: GitLab variable -> pipeline -> k8s Secret). The operator
	// never holds the value: it is not in crypto.yaml, helm values or the chart,
	// and the chart only references the Secret. The pipeline learns this mapping
	// from the manifest pipelineSecretsConfigMap publishes.
	GitLabVariable string `json:"gitlabVariable"`
}

type serviceSpec struct {
	Name             string
	Replicas         int32
	Image            string
	HasGRPC          bool
	HasPVC           bool
	Storage          string
	Port             int32
	WebAppHostPath   string
	HasServiceConfig bool
	HasHazelcast     bool
	// HasWorkingSetHazelcast declares a SECOND Hazelcast cluster in the same JVM,
	// on stratumWorkingSetPort. Only stratum has one: its working-set maps are a
	// separate cluster from the event store. Kernel has a single cluster and must
	// keep rendering exactly one port block.
	HasWorkingSetHazelcast bool
	// HazelcastNetworkPolicy, if true, renders a NetworkPolicy that confines the
	// Hazelcast member ports to this service's own pods. The member protocol has
	// no TLS and no member authentication and carries Java serialization, so any
	// pod that can open a TCP connection to it is effectively inside the cluster;
	// the network is the only control there is.
	//
	// Set on stratum only, for now. Kernel has the same open member port on
	// stratumHazelcastPort and wants the same treatment, but its policy has not
	// been verified against a live kernel cluster yet — see
	// decisions/stratum-hazelcast-networkpolicy-2026-09-12.md.
	HazelcastNetworkPolicy bool
	// HazelcastPeerApps names the OTHER workloads whose pods are members of this
	// service's Hazelcast cluster, as `app` label values, so HazelcastNetworkPolicy
	// admits them to the member ports.
	//
	// It exists because stratum is two workloads in one cluster: the writers and the
	// volume-less search members. The policy's peer selector is the service's own
	// `app` label, which is exactly right for a one-workload cluster and silently
	// wrong for this one — the search members' join would be dropped, and Hazelcast
	// answers a blocked member port by forming a separate cluster rather than by
	// failing.
	HazelcastPeerApps []string
	ListenPort        int32 // gRPC listen port: 5000 for Java (kernel), 8443 for Rust/Tacplus
	// NodeModes, if non-nil, sets NODE_MODE env var per replica.
	// Index i maps to replica i; if shorter than Replicas, remaining default to "alpha".
	NodeModes []string
	// ExtraEnvVars, if non-empty, is appended to the env block in the Deployment.
	// Each entry should be a fully-formed YAML env var block (name + value lines).
	ExtraEnvVars string
	// AcmePort, if non-zero, adds a containerPort and an extra port on the
	// per-replica ClusterIP Service for the ACME HTTP-01 responder. IN-CLUSTER
	// ONLY: the gateway owns the edge's port 80 and proxies challenge requests
	// here, so this responder is not exposed outside the cluster and there is
	// no LoadBalancer for it.
	AcmePort int32
	// HttpPort, if non-zero, adds a containerPort and a port-80 entry on this
	// service's external LoadBalancer (external 80 -> HttpPort in the
	// container). Set on gateway only, and only in LETSENCRYPT mode: one
	// LoadBalancer carries both 443 and 80, so a single reserved address serves
	// the public HTTPS endpoint and ACME HTTP-01 validation.
	HttpPort int32
	// ExtraPort, if non-zero, adds an additional named port to the ClusterIP Service.
	// ExtraPortName is the port name used in the Service spec (required when ExtraPort != 0).
	ExtraPort     int32
	ExtraPortName string
	// NamedPorts are additional container ports, each also published on this
	// service's ClusterIP Service and — for a StatefulSet — on its headless
	// Service, which is what makes a port reachable at a pod's stable DNS name.
	//
	// IN-CLUSTER ONLY, every one of them: nothing here reaches a LoadBalancer.
	// AcmePort above is the single-port special case of the same idea, kept as it
	// is because it is letsencrypt's and is referenced by name; this is the general
	// form, added for blobstore, which needs two (its byte listener, which the
	// gateway proxies, and its peer listener, which only the other pod dials).
	NamedPorts []namedPort
	// SkipProbes, if true, omits the readiness and liveness probes from the Deployment.
	// Use for services that do not expose a health-check endpoint.
	SkipProbes bool
	// ServiceAccountName, if non-empty, creates and uses a dedicated service account.
	ServiceAccountName string
	// AutomountServiceAccountToken, when set, controls ambient Kubernetes token automounting.
	AutomountServiceAccountToken *bool
	// ServiceAccountTokenProjection, if non-nil, mounts an explicit projected Kubernetes token.
	ServiceAccountTokenProjection *serviceAccountTokenProjection
	// ServiceAccountTokenProjections mounts additional explicit projected Kubernetes tokens.
	ServiceAccountTokenProjections []*serviceAccountTokenProjection
	// TokenReviewRBAC grants the service account permission to create Kubernetes TokenReviews.
	TokenReviewRBAC bool
	// PodReadRBAC grants the service account namespace-scoped permission to read Pods.
	PodReadRBAC bool
	// IsStatefulSet, if true, generates a StatefulSet with headless Service and
	// volumeClaimTemplates instead of per-replica Deployments with separate PVCs.
	// Used for kernel where stable pod identity and PVC lifecycle are required.
	IsStatefulSet bool
	// OrderedReady, if true, renders the StatefulSet with podManagementPolicy
	// OrderedReady instead of Parallel, so pod N+1 is not created until pod N is
	// Running and Ready. Set on kernel: its state lives only in memory,
	// replicated between members, and Parallel brought every missing kernel pod
	// up at once with no member holding state (the 0→3 scale, 2026-09-23).
	// Rolling updates are one pod at a time under either policy.
	OrderedReady bool
	// ConfigHasKeyMaterial, if true, packages the service's generated config as
	// a Secret instead of a ConfigMap. Three configs embed private keys: kernel's
	// per-node config (node TLS key, node signing CA key, and the gateway
	// external TLS key it serves), kmaker's (the cluster CA signing key), and
	// gateway's (the external TLS key). A ConfigMap is not secret storage —
	// no encryption at rest, and readable by anything holding configmap get.
	ConfigHasKeyMaterial bool
	// SecretEnvVars are configuration fields that MUST NOT travel in kernel's
	// combined seed, delivered to the pod as environment variables sourced from
	// a per-service Kubernetes Secret this generator renders.
	//
	// WHY THIS EXISTS. Everything on a service's Config message reaches the pod
	// through kernel: the operator fills kernelConfig.Configs.<svc>_config and
	// kernel republishes it to that service's ConfigState. That seed is ONE blob
	// carrying every service's Config, so a signing secret on a Config message is
	// a signing secret every config-holder in the fleet can read — which is how
	// blobstore's upload-token key leaked
	// (looper/schematic/decisions/blobstore-rev22-defects-2026-09-27.md).
	//
	// ConfigHasKeyMaterial is NOT the same thing and does not solve this: it only
	// decides whether the pod's own bootstrap config is packaged as a Secret
	// rather than a ConfigMap. The seed copy exists either way.
	//
	// A field declared here lives on its own message (oauth.Secrets, not
	// oauth.Config), so the seed has nowhere to put it, and the value moves
	// GitLab CI/CD variable -> pipeline-written k8s Secret -> pod env and nowhere
	// else (D25). The chart references the Secret and never renders it. EnvName is
	// the name tacplus's generated <Msg>FromEnvVar reads, so it is derived from
	// the proto package and message, never invented here.
	SecretEnvVars []secretEnvVar
	// PerReplicaConfig, if true, gives each replica its own values.yaml config key
	// (kernel0, kernel1, …) instead of sharing one key named for the service.
	//
	// This is a property of the CONFIG, not of the workload kind. Kernel needs it
	// because every replica carries a distinct TLS certificate, and the bundle
	// supplies those as bootstrap0..N. Every other service — including stateful
	// ones — has a single config that all replicas read, and the bundle supplies
	// exactly one entry under the service name. Keying this off IsStatefulSet
	// made stratum's configmaps reference .Values.stratum0.config, which nothing
	// populates, so all three pods started with an empty config and died with
	// "data is null or empty".
	PerReplicaConfig bool
	// SharesServiceIdentityWith names the service this workload is a SECOND WORKLOAD
	// OF, rather than a service of its own. Set on stratum-search, and on nothing
	// else.
	//
	// A search member is stratum: same image, same config, same identity to kernel.
	// So it reuses that service's ServiceAccount (and therefore the identity kernel
	// resolves by TokenReview) and that service's values.yaml config key, and this
	// workload emits no ServiceAccount or RBAC objects of its own — a second object
	// with the same name is a duplicate resource helm refuses, and a service account
	// of its own would be a principal kernel has never heard of.
	SharesServiceIdentityWith string
	// TenantInstance marks one tenant's instance of a tenant-owned service
	// ("<svc>-<tenant>"), pinned or not. It decides the instance's pool.
	TenantInstance bool
	// NodePool, if non-empty, is the pool (CorePool or TenantPool) the workload
	// is placed on. Generate sets it, and only when the cluster has pools.
	NodePool string
	// Resource limits/requests. Every service should have these set —
	// without them, runtimes (JVM, Go, Rust async runtimes) make assumptions
	// based on host RAM that don't hold inside a container, and you end up
	// over-committing the node. Sizing each service properly is the
	// developer/operator's responsibility regardless of language.
	MemRequest string
	MemLimit   string
	CpuRequest string
	CpuLimit   string
	// JavaHeap, if non-empty, emits `JDK_JAVA_OPTIONS=-Xmx<heap> -Xms<heap>`.
	// Tells the JVM what max heap to use. Independent of MemLimit; should be
	// set lower than MemLimit to leave room for non-heap memory (metaspace,
	// direct buffers, code cache, threads).
	JavaHeap string
	// JavaHeapMin, if non-empty, sets -Xms independently of -Xmx.
	//
	// Xms == Xmx is a latency choice for a service with a large steady-state
	// heap: it pre-commits and avoids growth pauses. For a service that is
	// mostly idle it is pure waste — the JVM reserves memory the node could give
	// to another pod, and RSS then reports the reservation rather than the live
	// set, which makes the heap look "used" when measuring. Kernel showed
	// 1175Mi RSS against -Xms1g while doing almost nothing.
	JavaHeapMin string
	// GoMemLimit, if non-empty, sets the GOMEMLIMIT env var (Go 1.19+) — Go's
	// soft memory target. Set lower than MemLimit (commonly ~80%) so Go's GC
	// triggers before the kernel OOM-killer fires. Same role as JavaHeap for
	// the Go runtime.
	GoMemLimit string
}

type serviceAccountTokenProjection struct {
	VolumeName        string
	MountPath         string
	TokenPath         string
	Audience          string
	ExpirationSeconds int32
	IncludeKubeRootCA bool
	IncludeNamespace  bool
}

func serviceAccountLine(svc serviceSpec) string {
	if svc.ServiceAccountName == "" {
		return ""
	}
	return fmt.Sprintf("      serviceAccountName: %s", svc.ServiceAccountName)
}

// pvcFsGroupNonRoot is the uid/gid the distroless `nonroot` user runs as. A
// freshly provisioned PersistentVolume is owned by root:root with mode 0755, so
// a container that does not run as root cannot create anything inside it. The
// kubelet applies fsGroup by recursively chgrp'ing the volume to this gid and
// adding g+w, which fixes the non-root case without taking anything away from a
// container that DOES run as root.
const pvcFsGroupNonRoot = 65532

// archTolerationLines renders the toleration every workload needs to land on an
// Arm node.
//
// The whole fleet is arm64 by design (control/GKE.md: t2a Spot nodes), and GKE
// taints Arm nodes `kubernetes.io/arch=arm64:NoSchedule`. Without a matching
// toleration nothing schedules — which is invisible until a node is REPLACED,
// because an existing pod keeps running on a node that gains a NoSchedule taint.
//
// That is exactly how it surfaced: a Spot preemption replaced the node, the new
// one carried the taint, and 14 of 15 pods went Pending with "1 node(s) had
// untolerated taint(s)" while the node sat at 25% CPU. The one pod that kept
// running was cert-manager's acme-solver, which ships this toleration itself.
// spreadAcrossNodesLines asks the scheduler to place replicas of a service on
// DIFFERENT nodes.
//
// Preferred, not required: with more replicas than nodes — or on a single-node
// dev cluster — a required rule leaves pods Pending forever, which is a worse
// failure than co-location. Redundancy that cannot schedule is not redundancy.
//
// This matters for the services we run one-per-node for availability: three
// kernels stacked on one node survive nothing, and the Hazelcast members would
// share a fate they are supposed to be independent of.
func spreadAcrossNodesLines(svc serviceSpec) string {
	if svc.Replicas < 2 {
		return ""
	}
	return fmt.Sprintf(`      affinity:
        podAntiAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            podAffinityTerm:
              topologyKey: kubernetes.io/hostname
              labelSelector:
                matchLabels:
                  app: %s
`, svc.Name)
}

func archTolerationLines() string {
	return `      tolerations:
      - key: kubernetes.io/arch
        operator: Equal
        value: arm64
        effect: NoSchedule
`
}

// nodePoolLines places the pod on its pool (see NodePoolLabel). It must follow
// archTolerationLines directly: a tenant instance's toleration of the tenant
// pool's taint is one more item in the tolerations list that function opens.
func nodePoolLines(svc serviceSpec) string {
	if svc.NodePool == "" {
		return ""
	}
	var b strings.Builder
	if svc.NodePool == TenantPool {
		fmt.Fprintf(&b, `      - key: %s
        operator: Equal
        value: %s
        effect: NoSchedule
`, NodePoolLabel, TenantPool)
	}
	fmt.Fprintf(&b, `      nodeSelector:
        %s: %s
`, NodePoolLabel, svc.NodePool)
	return b.String()
}

// servicePodLabelLine renders the ServicePodLabel pod-template label that the
// workload's PodDisruptionBudget selects on.
func servicePodLabelLine(svc serviceSpec) string {
	if svc.NodePool == "" {
		return ""
	}
	return fmt.Sprintf("        %s: %s\n", ServicePodLabel, svc.Name)
}

// podDisruptionBudget lets a voluntary disruption — a node drain for a pool
// upgrade, or the autoscaler emptying a tenant node — take at most one of the
// workload's pods at a time. A single-replica workload gets none: a budget of
// one unavailable out of one is no budget, and anything stricter would make its
// node undrainable.
func podDisruptionBudget(svc serviceSpec) []byte {
	if svc.NodePool == "" || svc.Replicas < 2 {
		return nil
	}
	return []byte(fmt.Sprintf(`apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      %s: %s
`, svc.Name, ServicePodLabel, svc.Name))
}

// pvcFsGroupLines renders the pod securityContext for a service with a PVC.
//
// Without it, eventbus -- distroless java, USER nonroot -- could not create
// /app/data/__cluster_metadata-0 and its embedded Kafka broker failed to format
// KRaft storage on every attempt, forever. It was on retry 131 when this was
// found. Nothing surfaced: the pod's probes only check that the gRPC port
// answers, so it reported Ready the whole time while every consumer in the
// fleet sat on "Kafka not yet initialized".
//
// The image's own Dockerfile pre-chowns /var/lib/eventbus/data for 65532, which
// looks like the fix is already in place -- but this template mounts the claim
// at /app/data, a path the image never prepared. Doing it here rather than in
// each Dockerfile keeps the guarantee attached to the thing that decides the
// mount path.
func pvcFsGroupLines(svc serviceSpec) string {
	if !svc.HasPVC {
		return ""
	}
	return fmt.Sprintf("      securityContext:\n        fsGroup: %d\n", pvcFsGroupNonRoot)
}

func podServiceAccountLines(svc serviceSpec) string {
	var b bytes.Buffer
	if line := serviceAccountLine(svc); line != "" {
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString(automountServiceAccountTokenLine(svc))
	return b.String()
}

func automountServiceAccountTokenLine(svc serviceSpec) string {
	if svc.AutomountServiceAccountToken == nil {
		return ""
	}
	return fmt.Sprintf("      automountServiceAccountToken: %t\n", *svc.AutomountServiceAccountToken)
}

func projectedServiceAccountTokenMount(svc serviceSpec) string {
	var b bytes.Buffer
	for _, proj := range serviceAccountTokenProjections(svc) {
		if proj == nil {
			continue
		}
		b.WriteString(fmt.Sprintf(`        - name: %s
          mountPath: %s
          readOnly: true
`, proj.VolumeName, proj.MountPath))
	}
	return b.String()
}

func projectedServiceAccountTokenVolume(svc serviceSpec) string {
	var b bytes.Buffer
	for _, proj := range serviceAccountTokenProjections(svc) {
		if proj == nil {
			continue
		}
		b.WriteString(projectedServiceAccountTokenProjectionVolume(proj))
	}
	return b.String()
}

func serviceAccountTokenProjections(svc serviceSpec) []*serviceAccountTokenProjection {
	projections := make([]*serviceAccountTokenProjection, 0, len(svc.ServiceAccountTokenProjections)+1)
	if svc.ServiceAccountTokenProjection != nil {
		projections = append(projections, svc.ServiceAccountTokenProjection)
	}
	projections = append(projections, svc.ServiceAccountTokenProjections...)
	return projections
}

func projectedServiceAccountTokenProjectionVolume(proj *serviceAccountTokenProjection) string {
	if proj == nil {
		return ""
	}
	tokenPath := proj.TokenPath
	if tokenPath == "" {
		tokenPath = "token"
	}
	expirationSeconds := proj.ExpirationSeconds
	if expirationSeconds <= 0 {
		expirationSeconds = 3600
	}
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf(`      - name: %s
        projected:
          defaultMode: 0444
          sources:
          - serviceAccountToken:
              path: %s
`, proj.VolumeName, tokenPath))
	if proj.Audience != "" {
		b.WriteString(fmt.Sprintf("              audience: %s\n", proj.Audience))
	}
	b.WriteString(fmt.Sprintf("              expirationSeconds: %d\n", expirationSeconds))
	if proj.IncludeKubeRootCA {
		b.WriteString(`          - configMap:
              name: kube-root-ca.crt
              items:
              - key: ca.crt
                path: ca.crt
`)
	}
	if proj.IncludeNamespace {
		b.WriteString(`          - downwardAPI:
              items:
              - path: namespace
                fieldRef:
                  fieldPath: metadata.namespace
`)
	}
	return b.String()
}

// resourcesBlock returns the YAML `resources:` block for a container, indented
// to match position inside a containers[].* entry. Empty string if no fields set.
func resourcesBlock(svc serviceSpec) string {
	if svc.MemRequest == "" && svc.MemLimit == "" && svc.CpuRequest == "" && svc.CpuLimit == "" {
		return ""
	}
	var b bytes.Buffer
	b.WriteString("        resources:\n")
	if svc.MemRequest != "" || svc.CpuRequest != "" {
		b.WriteString("          requests:\n")
		if svc.MemRequest != "" {
			b.WriteString(fmt.Sprintf("            memory: %s\n", svc.MemRequest))
		}
		if svc.CpuRequest != "" {
			b.WriteString(fmt.Sprintf("            cpu: %s\n", svc.CpuRequest))
		}
	}
	if svc.MemLimit != "" || svc.CpuLimit != "" {
		b.WriteString("          limits:\n")
		if svc.MemLimit != "" {
			b.WriteString(fmt.Sprintf("            memory: %s\n", svc.MemLimit))
		}
		if svc.CpuLimit != "" {
			b.WriteString(fmt.Sprintf("            cpu: %s\n", svc.CpuLimit))
		}
	}
	return b.String()
}

// javaOptsEnv returns a YAML env-var snippet that sets JDK_JAVA_OPTIONS, which
// the `java` binary reads directly (JEP 245, Java 9+) — works regardless of
// how the container's entrypoint invokes the JVM. Empty string if JavaHeap
// is not set. UseContainerSupport is the default on modern JDKs but set
// explicitly for safety.
func javaOptsEnv(svc serviceSpec) string {
	if svc.JavaHeap == "" {
		return ""
	}
	min := svc.JavaHeapMin
	if min == "" {
		min = svc.JavaHeap
	}
	return fmt.Sprintf("        - name: JDK_JAVA_OPTIONS\n          value: \"-Xmx%s -Xms%s -XX:+UseContainerSupport\"\n", svc.JavaHeap, min)
}

// goMemLimitEnv returns a YAML env-var snippet that sets GOMEMLIMIT for Go
// services. Empty string if GoMemLimit is not set.
func goMemLimitEnv(svc serviceSpec) string {
	if svc.GoMemLimit == "" {
		return ""
	}
	return fmt.Sprintf("        - name: GOMEMLIMIT\n          value: \"%s\"\n", svc.GoMemLimit)
}

func falsePtr() *bool {
	v := false
	return &v
}

func kmakerTokenAudience(cfg *protoOperator.Config) string {
	if cfg.GetKmaker().GetTokenAudience() != "" {
		return cfg.GetKmaker().GetTokenAudience()
	}
	return "kmaker"
}

const (
	kernelServiceTokenMountPath = "/var/run/secrets/schematic/kernel"
	kernelServiceTokenFile      = kernelServiceTokenMountPath + "/token"
)

func kernelTokenAudience() string {
	return "schematic-kernel"
}

// kernelTokenVolumeName is the projected-token volume withKernelServiceAuth
// adds. Carrying it is what makes a serviceSpec kernel-authenticated, so it is
// also how KernelAuthServiceNames recognises one.
const kernelTokenVolumeName = "kernel-token"

func withKernelServiceAuth(svc serviceSpec) serviceSpec {
	if svc.ServiceAccountName == "" {
		if svc.SharesServiceIdentityWith != "" {
			svc.ServiceAccountName = svc.SharesServiceIdentityWith
		} else {
			svc.ServiceAccountName = svc.Name
		}
	}
	if svc.AutomountServiceAccountToken == nil {
		svc.AutomountServiceAccountToken = falsePtr()
	}
	svc.ServiceAccountTokenProjections = append(svc.ServiceAccountTokenProjections, &serviceAccountTokenProjection{
		VolumeName:        kernelTokenVolumeName,
		MountPath:         kernelServiceTokenMountPath,
		TokenPath:         "token",
		Audience:          kernelTokenAudience(),
		ExpirationSeconds: 3600,
	})
	svc.ExtraEnvVars += fmt.Sprintf(`        - name: KERNEL_K8S_TOKEN_FILE
          value: %s
`, kernelServiceTokenFile)
	return svc
}

func effectiveTopology(cfg *protoOperator.Config, nodeCount int) *protoOperator.Topology {
	t := cfg.GetTopology()
	if t == nil {
		t = &protoOperator.Topology{}
	}
	if t.GatewayNodes == 0 && nodeCount > 0 {
		t = cloneTopology(t)
		t.GatewayNodes = int32(nodeCount)
	}
	return t
}

func cloneTopology(t *protoOperator.Topology) *protoOperator.Topology {
	return &protoOperator.Topology{
		KernelNodes:        t.KernelNodes,
		EventbusNodes:      t.EventbusNodes,
		GatewayNodes:       t.GatewayNodes,
		OauthNodes:         t.OauthNodes,
		OpaNodes:           t.OpaNodes,
		StratumNodes:       t.StratumNodes,
		StratumSearchNodes: t.StratumSearchNodes,
		KmakerNodes:        t.KmakerNodes,
		EchoNodes:          t.EchoNodes,
		MessagingNodes:     t.MessagingNodes,
		MailboxNodes:       t.MailboxNodes,
		PersonalauthNodes:  t.PersonalauthNodes,
		BlobstoreNodes:     t.BlobstoreNodes,
		// Carry the app's services. A field-by-field clone silently drops
		// anything added later — which is the failure mode a fixed-field message
		// invites, and it dropped app_services on the first attempt. It dropped
		// blobstore_nodes on this one too, so the warning is doing its job as a
		// warning and not as a fix: a `proto.Clone` here would end the class, and
		// changing it is outside what this change is for.
		AppServices: t.AppServices,
	}
}

// timedbNodeModes / graphdbNodeModes lived here. Both went with the
// timedb/graphdb boneyard on 2026-08-25 (BIG GOAL 3, first move #1). The generic
// serviceSpec.NodeModes plumbing they fed is left in place but now has no caller
// — noted rather than removed, since it is not this change's business.

func (g *Generator) imageRef(cfg *protoOperator.Config, svc string) string {
	return g.imageRefProj(cfg, svc, svc)
}

// imageRefProj builds an image reference where the GitLab registry project name
// differs from the service name. A digest in cfg.image_digests for proj
// replaces the tag: <repository>@<digest>.
func (g *Generator) imageRefProj(cfg *protoOperator.Config, proj, svc string) string {
	var repo, tag string
	// Local-images mode (SCHEMATIC_LOCAL_IMAGES): reference locally-built images
	// (schematic/<proj>:local) instead of the GitLab registry. Used for local
	// docker-desktop clusters where k8s shares the docker daemon, so
	// IfNotPresent resolves the local tag without a registry.
	if v := os.Getenv("SCHEMATIC_LOCAL_IMAGES"); v != "" {
		// A value containing a host (slash/colon) is a registry prefix, e.g.
		// SCHEMATIC_LOCAL_IMAGES=localhost:5001 -> localhost:5001/schematic/<proj>:local.
		// Any other non-empty value uses the bare local tag (shared image store).
		if strings.ContainsAny(v, "/:") {
			repo = fmt.Sprintf("%s/schematic/%s", v, proj)
		} else {
			repo = fmt.Sprintf("schematic/%s", proj)
		}
		tag = "local"
	} else {
		repo = fmt.Sprintf("registry.gitlab.com/seabeam/schematic/platform/services/%s/%s-%s", proj, svc, g.series)
		tag = g.resolveTag(proj, svc)
	}
	if digest := cfg.GetImageDigests()[proj]; digest != "" {
		return repo + "@" + digest
	}
	return repo + ":" + tag
}

// resolveTag returns the image tag for a service via the resolver,
// falling back to "latest" if the resolver is nil or returns empty.
func (g *Generator) resolveTag(proj, svc string) string {
	if g.resolver != nil {
		if tag := g.resolver.ServiceTag(proj, svc); tag != "" {
			return tag
		}
	}
	return "latest"
}

func (g *Generator) enabledServices(cfg *protoOperator.Config, topo *protoOperator.Topology) []serviceSpec {
	opt := cfg.GetOptionalServices()

	// Core always-on services.
	// Resource defaults below are starting points. Memory sizing is a
	// per-service responsibility — Java services get JavaHeap, Go services
	// get GoMemLimit, all set lower than MemLimit to leave room for non-GC-
	// managed memory. Tune as each service's working set grows.
	// Notable workloads:
	//   - stratum: in-memory DB with in-memory result sets — needs to grow
	//     with event volume; today's 2Gi limit is the starting point.
	//   - eventbus: embedded Kafka broker — heap usage scales with topic
	//     count + replication settings.
	svcs := []serviceSpec{
		{Name: "kernel", Replicas: topo.KernelNodes, Image: g.imageRef(cfg, "kernel"), HasGRPC: true, HasPVC: true, Storage: "2Gi", HasHazelcast: true, ListenPort: 5000, IsStatefulSet: true, OrderedReady: true, PerReplicaConfig: true, ConfigHasKeyMaterial: true,
			ServiceAccountName:           "kernel",
			AutomountServiceAccountToken: falsePtr(),
			ServiceAccountTokenProjections: []*serviceAccountTokenProjection{
				{
					VolumeName:        "kmaker-token",
					MountPath:         "/var/run/secrets/schematic/kmaker",
					TokenPath:         "token",
					Audience:          kmakerTokenAudience(cfg),
					ExpirationSeconds: 600,
				},
				{
					VolumeName:        "kubernetes-api-token",
					MountPath:         "/var/run/secrets/kubernetes.io/serviceaccount",
					TokenPath:         "token",
					ExpirationSeconds: 3600,
					IncludeKubeRootCA: true,
					IncludeNamespace:  true,
				},
			},
			TokenReviewRBAC: true,
			ExtraEnvVars: fmt.Sprintf(`        - name: KMAKER_K8S_TOKEN_FILE
          value: /var/run/secrets/schematic/kmaker/token
        - name: KERNEL_K8S_TOKEN_AUDIENCE
          value: %s
`, kernelTokenAudience()),
			MemRequest: "384Mi", MemLimit: "1Gi", CpuRequest: "250m", JavaHeap: "512m", JavaHeapMin: "128m"},
		{Name: "kmaker", Replicas: topo.KmakerNodes, Image: g.imageRef(cfg, "kmaker"), HasGRPC: true, ListenPort: 8443, ConfigHasKeyMaterial: true,
			ServiceAccountName:           "kmaker",
			AutomountServiceAccountToken: falsePtr(),
			ServiceAccountTokenProjection: &serviceAccountTokenProjection{
				VolumeName:        "kubernetes-api-token",
				MountPath:         "/var/run/secrets/kubernetes.io/serviceaccount",
				TokenPath:         "token",
				ExpirationSeconds: 3600,
				IncludeKubeRootCA: true,
				IncludeNamespace:  true,
			},
			TokenReviewRBAC: true, PodReadRBAC: true,
			ExtraEnvVars: "        - name: SERVICE_CONFIG\n          value: /data/bootstrap.yaml\n",
			MemRequest:   "128Mi", MemLimit: "512Mi", CpuRequest: "50m", GoMemLimit: "400MiB"},
		withKernelServiceAuth(serviceSpec{Name: "eventbus", Replicas: topo.EventbusNodes, Image: g.imageRef(cfg, "eventbus"), HasGRPC: true, HasPVC: true, Storage: "2Gi", ListenPort: 8443,
			// Measured 2026-09-19 on the live fleet: ~30m steady. 250m reserved
			// 8x what it used and, with three fleets' worth of 250m Java pods,
			// left the 2-vCPU nodes at 99% requested while 6% busy.
			MemRequest: "1Gi", MemLimit: "2Gi", CpuRequest: "100m", JavaHeap: "1g", JavaHeapMin: "128m"}),
		withKernelServiceAuth(serviceSpec{Name: "gateway", Replicas: topo.GatewayNodes, Image: g.imageRef(cfg, "gateway"), HasGRPC: true, ListenPort: 8443, ConfigHasKeyMaterial: true,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "100m"}),
		withKernelServiceAuth(serviceSpec{Name: "opa", Replicas: topo.OpaNodes, Image: g.imageRef(cfg, "opa"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}),
		// stratum is a StatefulSet with a headless Service, like kernel, because
		// its event store is a Hazelcast cluster: the members need stable
		// per-pod DNS to find each other. As a Deployment behind a plain
		// ClusterIP they could not — the pods had no stable identity and no
		// discoverable peer addresses — so several replicas would each form
		// their OWN cluster of size one. No partition sharing, and every pod
		// believing it was a cold boot, replaying the whole event log.
		withKernelServiceAuth(serviceSpec{Name: "stratum", Replicas: topo.StratumNodes, Image: g.imageRef(cfg, "stratum"), HasGRPC: true, HasPVC: true, Storage: "5Gi", HasHazelcast: true, HasWorkingSetHazelcast: true, HazelcastNetworkPolicy: true, HazelcastPeerApps: []string{stratumSearchName}, ListenPort: 8443, IsStatefulSet: true,
			ExtraEnvVars: stratumSeedMembersEnv(topo.StratumNodes, topo.GetStratumSearchNodes()) +
				stratumMemberRoleEnv("writer"),
			// Measured 2026-09-19 on the live fleet: ~10m steady per replica; the
			// record store is I/O-bound, not CPU-bound. Same reasoning as eventbus.
			MemRequest: "1Gi", MemLimit: "2Gi", CpuRequest: "50m", JavaHeap: "1g", JavaHeapMin: "128m"}),
	}

	// stratum's SEARCH members: the same image and the same config, joining the same
	// Hazelcast cluster with the member attribute stratum.role=search, and with NO
	// volume.
	//
	// A search member holds partitions and answers the parallel scatter-gather a
	// predicate query is, and persists nothing: no redo log, no record index, no
	// checkpoint. It is never counted for min_durable_copies and never a restore
	// contributor, so this count is free to move with query load and says nothing
	// about durability — stratum_nodes is the number that does.
	//
	// A StatefulSet rather than a Deployment, and volume-less: the members need the
	// stable per-pod DNS the seed list is written from (stratum-search-0.stratum-search),
	// which a Deployment's pods do not have. Everything a StatefulSet usually brings a
	// volume for is exactly what this workload does not do.
	if topo.StratumNodes > 0 && topo.GetStratumSearchNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: stratumSearchName,
			SharesServiceIdentityWith: "stratum",
			Replicas:                  topo.GetStratumSearchNodes(),
			Image:                     g.imageRef(cfg, "stratum"),
			HasGRPC:                   true, ListenPort: 8443, IsStatefulSet: true,
			HasHazelcast: true, HasWorkingSetHazelcast: true,
			HazelcastNetworkPolicy: true, HazelcastPeerApps: []string{"stratum"},
			ExtraEnvVars: stratumSeedMembersEnv(topo.StratumNodes, topo.GetStratumSearchNodes()) +
				stratumMemberRoleEnv("search"),
			// The same memory as a writer and none of its disk. A search member holds
			// its share of the partitions and never evicts them — the idle clock is
			// wired to the disk leg it does not have — so memory is the whole of what
			// it is sized for, and it is sized like a writer because it holds the same
			// kind of share of the same partitions.
			MemRequest: "1Gi", MemLimit: "2Gi", CpuRequest: "50m", JavaHeap: "1g", JavaHeapMin: "128m"}))
	}

	// OAuth (OAuth2/OIDC broker) — always enabled.
	svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "oauth", Replicas: topo.OauthNodes, Image: g.imageRef(cfg, "oauth"), HasGRPC: true, ListenPort: 8443,
		MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m",
		// Sign in with Apple's .p8 key: oauth.Secrets.apple_signin_key_p8, which
		// tacplus's generated SecretsFromEnvVar reads from this variable.
		SecretEnvVars: []secretEnvVar{{
			EnvName:        "PROTO_SERVICES_OAUTH_SECRETS_APPLE_SIGNIN_KEY_P8",
			Key:            "apple_signin_key_p8",
			GitLabVariable: "APPLE_SIGNIN_KEY_P8",
		}}}))

	// Echo sample service — disabled by default. Stateless, no S2S deps, so it
	// needs nothing beyond the standard kernel service auth every service gets.
	if opt.GetEcho() && topo.GetEchoNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "echo", Replicas: topo.EchoNodes, Image: g.imageRef(cfg, "echo"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "64Mi", MemLimit: "256Mi", CpuRequest: "25m"}))
	}

	// Messaging — the platform's message primitive. Disabled by default.
	// Stateless mediation over eventbus; its eventbus access is s2s and is
	// arranged by the generated ServiceManager, so like echo it needs nothing
	// beyond the standard kernel service auth.
	if opt.GetMessaging() && topo.GetMessagingNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "messaging", Replicas: topo.MessagingNodes, Image: g.imageRef(cfg, "messaging"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "64Mi", MemLimit: "256Mi", CpuRequest: "25m"}))
	}

	// Mailbox — pull delivery over messaging's deliveries. Disabled by default,
	// same shape as messaging.
	if opt.GetMailbox() && topo.GetMailboxNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "mailbox", Replicas: topo.MailboxNodes, Image: g.imageRef(cfg, "mailbox"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "64Mi", MemLimit: "256Mi", CpuRequest: "25m",
			// The crew-mail service account's JSON key:
			// mailbox.Secrets.gmail_service_account_json, which tacplus's generated
			// SecretsFromEnvVar reads from this variable.
			SecretEnvVars: []secretEnvVar{{
				EnvName:        "PROTO_SERVICES_MAILBOX_SECRETS_GMAIL_SERVICE_ACCOUNT_JSON",
				Key:            "gmail_service_account_json",
				GitLabVariable: "CREW_MAIL_SA_KEY_JSON",
			}}}))
	}

	// personalauth — the login broker for PERSONAL principals. Disabled by
	// default. Same shape as oauth, which is the other login broker: stateless,
	// gRPC on 8443, kernel access arranged by the generated ServiceManager.
	if opt.GetPersonalauth() && topo.GetPersonalauthNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "personalauth", Replicas: topo.PersonalauthNodes, Image: g.imageRef(cfg, "personalauth"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}))
	}

	// blobstore — the platform's binary blob primitive. Disabled by default.
	//
	// A STATEFULSET WITH A PVC PER POD, and the only service of that shape that is
	// not a Hazelcast cluster. The reasons are different from kernel's and
	// stratum's: it needs stable per-pod DNS because the pods MIRROR to each other
	// by name (blob-store-2026-09-25.md R2 — an upload is acked only when both pods
	// have the bytes), and it needs a volumeClaimTemplate because each pod holds its
	// own full copy of the files. As per-replica Deployments they would have no
	// stable name to dial and the peers could not find each other.
	//
	// ConfigHasKeyMaterial is deliberately NOT set. blobstore's config carries no
	// key material at all — its upload token is raw randomness verified by a
	// record lookup, not something signed — so a ConfigMap is the honest
	// packaging. It once carried token_hmac_key and was a Secret for that reason;
	// that key does not exist any more (decided 2026-09-27).
	//
	// Storage 20Gi is a starting point and is the one number here that is a guess:
	// it is the mirrored copy of every blob every tenant holds, so it is the field
	// to grow first. Growing it is supported — reconcileImmutableStatefulSetFields
	// expands the live PVCs and recreates the StatefulSet — and shrinking it is not.
	if opt.GetBlobstore() && topo.GetBlobstoreNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{
			Name: "blobstore", Replicas: topo.BlobstoreNodes, Image: g.imageRef(cfg, "blobstore"),
			HasGRPC: true, ListenPort: 8443,
			HasPVC: true, Storage: "20Gi", IsStatefulSet: true,
			NamedPorts: []namedPort{
				{Name: "bytes", Port: blobstoreBytesPort},
				{Name: "peer", Port: blobstorePeerPort},
			},
			ExtraEnvVars: blobstorePeersEnv(topo.BlobstoreNodes),
			// Measured nowhere yet: this service streams bytes through a 64 KiB
			// buffer and holds no working set, so its memory is the Go runtime plus
			// one buffer per in-flight upload. Sized like the other small Go
			// services until a live cluster says otherwise.
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m", GoMemLimit: "400MiB",
		}))
	}

	// The application's own services.
	//
	// Modeled on echo, which was the platform's only example of "a service that
	// just needs to run and authenticate to kernel": stateless, gRPC on 8443, no
	// S2S dependencies beyond the standard kernel service auth every service
	// gets. An app service that needs more than this (a PVC, extra ports, a
	// Hazelcast member) is not expressible yet and should be added deliberately
	// rather than guessed at.
	// A tenant-owned app service (registry.json's (spec.service).tenant_owned)
	// is deployed once per tenant instead of once for the cluster: object names
	// become "<svc>-<tenant>" so N tenants do not collapse into one Deployment.
	// The image is still the one service's image — the instances differ only in
	// identity, which kernel derives from the delegate, never from config here.
	// A service named in llm_credential_services has the real llm_credential
	// value injected into its bootstrap config (pkg/bootstrap/deploy.go), so
	// that config must be packaged as a Secret, not a ConfigMap — same reason
	// as kernel/kmaker/gateway above. The list is config-driven, so this has to
	// be read from it rather than hardcoded per service name.
	llmCredentialServices := map[string]bool{}
	for _, name := range cfg.GetLlmCredentialServices() {
		llmCredentialServices[name] = true
	}
	for _, name := range appServiceNames(topo) {
		base := serviceSpec{
			Name: name, Replicas: topo.GetAppServices()[name], Image: g.imageRef(cfg, name),
			HasGRPC: true, ListenPort: 8443,
			ConfigHasKeyMaterial: llmCredentialServices[name],
			MemRequest:           "64Mi", MemLimit: "256Mi", CpuRequest: "25m"}
		if !g.tenantOwned[name] {
			svcs = append(svcs, withKernelServiceAuth(base))
			continue
		}
		// A pinned service is one instance in one tenant, never replicated:
		// its rows are keyed by something other than the tenant, so a second
		// instance would split them. Generate has already failed if the pinned
		// tenant is not configured.
		if tenant, pinned := g.pinnedTenant[name]; pinned {
			inst := base
			inst.Name = TenantInstanceName(name, tenant)
			inst.TenantInstance = true
			svcs = append(svcs, withKernelServiceAuth(inst))
			continue
		}
		for _, tenant := range cfg.GetTenants() {
			inst := base
			inst.Name = TenantInstanceName(name, tenant)
			inst.TenantInstance = true
			svcs = append(svcs, withKernelServiceAuth(inst))
		}
	}

	// Let's Encrypt ACME agent — one replica, only when tls.letsencrypt = true.
	if cfg.GetTls().GetLetsencrypt() {
		// The gateway takes the edge's port 80 on its own LoadBalancer, and
		// only in this mode: it exists to carry ACME HTTP-01 challenges to the
		// letsencrypt service and to redirect everything else to https. In
		// CERT_CHAIN mode nothing binds 8080, so the chart renders no port 80
		// rather than a forwarding rule into a closed port.
		for i := range svcs {
			if svcs[i].Name == "gateway" {
				svcs[i].HttpPort = gatewayHttpPort
				break
			}
		}
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{
			Name:             "letsencrypt",
			Replicas:         1,
			Image:            g.imageRef(cfg, "letsencrypt"),
			HasGRPC:          true,
			ListenPort:       8443,
			AcmePort:         8080,
			SkipProbes:       true,
			ExtraEnvVars:     "        - name: SERVICE_CONFIG\n          value: /data/service.yaml\n",
			HasServiceConfig: true,
		}))
	}

	// The application proxy services (anthropic, openai, gitlab, github), the MCP
	// server and msggateway were rendered here. They were application concerns,
	// not platform, and have moved out of this repo.

	// Filter zero-replica services.
	out := svcs[:0]
	for _, s := range svcs {
		if s.Replicas > 0 {
			out = append(out, s)
		}
	}
	return out
}

// ExpectedDeploymentNames returns the set of deployment names that the
// generator would create for the given config. Used by the reconciler to
// prune stale deployments after a topology change.
func (g *Generator) ExpectedDeploymentNames(cfg *protoOperator.Config, nodeCount int) map[string]bool {
	topo := effectiveTopology(cfg, nodeCount)
	svcs := g.enabledServices(cfg, topo)
	names := make(map[string]bool)
	for _, s := range svcs {
		for i := range int(s.Replicas) {
			names[fmt.Sprintf("%s%d", s.Name, i)] = true
		}
	}
	return names
}

// KernelAuthServiceNames returns the sorted, de-duplicated service IDENTITIES
// this chart renders with a kernel identity for cfg's topology — the specs
// withKernelServiceAuth stamps a projected kernel token onto.
//
// A workload that borrows another service's identity
// (SharesServiceIdentityWith, which stratum's search members do) reports the
// identity it borrows, not its own workload name: the identity is what kernel
// authenticates and what the operator has to know about.
//
// It exists for the bootstrap package's coverage test. A new platform service
// needs a serviceSpec here AND an entry in the operator's kernel-authenticated
// map, and it is the second of those one-line additions that has been forgotten
// — twice, each time as a live crash-loop. bootstrap cannot read enabledServices
// itself and helm cannot import bootstrap (that is the cycle appServiceNames
// exists to avoid), so the comparison needs this one accessor.
func (g *Generator) KernelAuthServiceNames(cfg *protoOperator.Config, nodeCount int) []string {
	topo := effectiveTopology(cfg, nodeCount)
	seen := make(map[string]bool)
	var out []string
	for _, s := range g.enabledServices(cfg, topo) {
		if !hasKernelToken(s) {
			continue
		}
		name := s.Name
		if s.SharesServiceIdentityWith != "" {
			name = s.SharesServiceIdentityWith
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func hasKernelToken(svc serviceSpec) bool {
	for _, p := range svc.ServiceAccountTokenProjections {
		if p.VolumeName == kernelTokenVolumeName {
			return true
		}
	}
	return false
}

// --- chart file generators ---

func (g *Generator) addChartYAML(charts *Charts, cfg *protoOperator.Config) error {
	tag := g.series
	content := fmt.Sprintf(`apiVersion: v2
name: schematic
description: Schematic control plane — managed by schematic-operator
type: application
version: 0.1.0
appVersion: %q
`, tag)
	charts.Files["Chart.yaml"] = []byte(content)
	return nil
}

var valuesTmpl = template.Must(template.New("values").Parse(`global:
  namespace: {{ .Namespace }}
  imagePullPolicy: {{ if .Dev }}Always{{ else }}IfNotPresent{{ end }}
  imagePullSecret: "gitlab-registry"

{{- range .Services }}
{{ .Name }}:
  image: {{ .Image }}
{{- if not .SharesServiceIdentityWith }}
  config: ""
{{- end }}
{{- if .HasPVC }}
  storage: {{ .Storage }}
{{- end }}
{{- if .Port }}
  port: {{ .Port }}
{{- end }}
{{- if .WebAppHostPath }}
  webappHostPath: {{ .WebAppHostPath }}
{{- end }}
{{- end }}
{{- range .PerReplicaConfigs }}
{{ . }}:
  config: ""
{{- end }}
`))

// perReplicaConfigKey holds the values.yaml key for one StatefulSet replica's config.
// StatefulSet services need per-replica keys (e.g. kernel0, kernel1) in values.yaml
// so that templates referencing .Values.kernel0.config don't fail with a nil-pointer
// error when no runtime override is provided (e.g. during a dry-run).
type perReplicaConfigKey = string

// valuesRef renders a hyphen-safe values.yaml lookup, e.g.
//
//	{{ index .Values "docmanager-acme" "config" }}
//
// A tenant-owned service's Kubernetes objects are named "<svc>-<tenant>", and
// the matching values key carries that dash. `.Values.docmanager-acme.config`
// does not parse — a Go template reads the dash as an operator, not as part of
// the key — so every per-service values lookup goes through index. Dashless
// names render identically either way.
func valuesRef(key, field string) string {
	return fmt.Sprintf("index .Values %q %q", key, field)
}

// configValueKey returns the values.yaml key holding replica i's config.
//
// Kernel uses per-replica keys (kernel0, kernel1, …) because each replica has a
// distinct TLS certificate and the bundle supplies them as bootstrap0..N. Every
// other service shares one config under the service name. The configmap and the
// StatefulSet's checksum annotation must agree, so both go through here.
func configValueKey(svc serviceSpec, i int) string {
	if svc.PerReplicaConfig {
		return fmt.Sprintf("%s%d", svc.Name, i)
	}
	// A second workload of another service reads that service's config: the bundle
	// supplies one entry per SERVICE, and stratum's search members run the same
	// binary against the same config as its writers.
	if svc.SharesServiceIdentityWith != "" {
		return svc.SharesServiceIdentityWith
	}
	return svc.Name
}

func (g *Generator) addValuesYAML(charts *Charts, cfg *protoOperator.Config, topo *protoOperator.Topology) error {
	svcs := g.enabledServices(cfg, topo)
	port := cfg.Port
	if port == 0 {
		port = 443
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}

	// Annotate the gateway serviceSpec with port and webroot so it renders
	// correctly in the range loop without creating a duplicate YAML key.
	for i, s := range svcs {
		if s.Name == "gateway" {
			svcs[i].Port = port
			svcs[i].WebAppHostPath = cfg.WebRoot
			break
		}
	}

	// StatefulSet services (kernel) use per-replica value keys (kernel0, kernel1, …)
	// in their templates. Add placeholder entries so helm doesn't fail with a nil-pointer
	// on .Values.kernel0.config when no runtime override is present.
	var perReplica []perReplicaConfigKey
	for _, s := range svcs {
		if s.PerReplicaConfig {
			for i := range int(s.Replicas) {
				perReplica = append(perReplica, fmt.Sprintf("%s%d", s.Name, i))
			}
		}
	}

	var buf bytes.Buffer
	if err := valuesTmpl.Execute(&buf, map[string]any{
		"Namespace":         ns,
		"Services":          svcs,
		"Dev":               cfg.Dev || os.Getenv("SCHEMATIC_LOCAL_IMAGES") != "",
		"PerReplicaConfigs": perReplica,
	}); err != nil {
		return fmt.Errorf("rendering values.yaml: %w", err)
	}
	charts.Files["values.yaml"] = buf.Bytes()
	return nil
}

func (g *Generator) addHelpers(charts *Charts) error {
	charts.Files["templates/_helpers.tpl"] = []byte(`{{/*
Common labels applied to all resources.
*/}}
{{- define "schematic.labels" -}}
` + ManagedByLabel + `: ` + ManagedByValue + `
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Init container that waits for kernel0:5000.
*/}}
{{- define "schematic.waitForKernel" -}}
- name: wait-for-kernel
  image: busybox:latest
  command: ["sh", "-c", "until nc -z kernel0 5000; do sleep 2; done"]
{{- end }}

{{/*
Readiness/liveness probe querying grpc.health.v1 on the plaintext health port.

These MUST target 8444, not the service port. The kubelet's gRPC probe speaks
PLAINTEXT only — it cannot do TLS — so probing 5000/8443 times out on every
request (that mistake once restarted all 12 services). tacplus serves
grpc.health.v1, and nothing else, on a separate plaintext listener on 8444.

They must also be a gRPC probe rather than tcpSocket: a TCP accept stays true
while a service's guts are dead, which is precisely how a wedged eventbus sat
1/1 Ready through drill 7. Only the health RPC reflects handler health.

timeoutSeconds is 5, NOT the k8s default of 1. A 1s budget is fine for a TCP
accept (kernel-level, cheap even mid-GC) but too tight for a health RPC that
the application must actually answer: at 1s the JVM services flapped, killed
3 times in 28 minutes by spurious liveness failures.
*/}}
{{- define "schematic.grpcProbe5000" -}}
grpc:
  port: 8444
initialDelaySeconds: 10
periodSeconds: 10
timeoutSeconds: 5
failureThreshold: 3
{{- end }}

{{- define "schematic.startupProbe5000" -}}
grpc:
  port: 8444
periodSeconds: 5
timeoutSeconds: 5
failureThreshold: 60
{{- end }}

{{- define "schematic.grpcProbe8443" -}}
grpc:
  port: 8444
initialDelaySeconds: 10
periodSeconds: 10
timeoutSeconds: 5
failureThreshold: 3
{{- end }}

{{/*
Startup probe for services that depend on kernel. Gives up to 5 minutes
for kernel to come up and the service to complete init before k8s kills it.
Once the startup probe passes, the liveness probe takes over. The health
endpoint reports NOT_SERVING until bootstrap completes, so this gates on real
readiness rather than merely on the port being bound.
*/}}
{{- define "schematic.startupProbe8443" -}}
grpc:
  port: 8444
periodSeconds: 5
timeoutSeconds: 5
failureThreshold: 60
{{- end }}

{{/*
Liveness/readiness for eventbus only. eventbus embeds its Kafka broker in its
own JVM, so a restart is also a broker restart: every downstream Subscribe RPC
reconnects at once (~25 sessions within one second) and the consumer groups
rebalance. Measured partition-assignment waits in that window are 87-135s, and
eventbus answers NOT_SERVING for the duration because its health check probes
Kafka. The shared 30s budget (period 10 x threshold 3) therefore killed it
mid-rebalance on every restart, and each kill made the next rebalance dirtier —
7 restarts into CrashLoopBackOff before this was raised.

300s matches the order of magnitude startupProbe8443 already allows for a cold
start, because a post-restart rebalance is functionally a second cold start.
eventbus's own UNHEALTHY_STREAK_THRESHOLD trips first at ~240s, so the pod is
normally restarted by its own verdict and this probe is the backstop.
*/}}
{{- define "schematic.grpcProbeEventbus" -}}
grpc:
  port: 8444
initialDelaySeconds: 10
periodSeconds: 10
timeoutSeconds: 5
failureThreshold: 30
{{- end }}

{{/*
Startup probe for stratum only. Cold start now includes warm Lucene index open
plus redo-log replay, which exceeds the shared 300s budget above and got the
pod SIGTERM'd mid-replay on a live rolling restart. ~30 minutes total.
*/}}
{{- define "schematic.startupProbeStratum" -}}
grpc:
  port: 8444
periodSeconds: 10
timeoutSeconds: 5
failureThreshold: 180
{{- end }}

{{/*
imagePullSecrets block — empty when no pull secret is configured.
*/}}
{{- define "schematic.imagePullSecrets" -}}
{{- if .Values.global.imagePullSecret -}}
imagePullSecrets:
- name: {{ .Values.global.imagePullSecret }}
{{- end }}
{{- end }}
`)
	return nil
}

// addRegistrySecret emits templates/registry-secret.yaml when gitlab_registry_token is set.
// The secret provides pull credentials for registry.gitlab.com in the schematic namespace.
func (g *Generator) addRegistrySecret(charts *Charts, cfg *protoOperator.Config) error {
	token := cfg.GetGitlabRegistryToken()
	if token == "" {
		return nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	dockerConfig, err := json.Marshal(map[string]any{
		"auths": map[string]any{
			"registry.gitlab.com": map[string]any{
				"username": "oauth2",
				"password": token,
				"auth":     auth,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("building dockerconfigjson: %w", err)
	}
	configB64 := base64.StdEncoding.EncodeToString(dockerConfig)
	charts.Files["templates/registry-secret.yaml"] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: gitlab-registry
  namespace: {{ .Values.global.namespace }}
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: %s
`, configB64))
	return nil
}

func (g *Generator) addNamespace(charts *Charts, cfg *protoOperator.Config) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}
	charts.Files["templates/namespace.yaml"] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, ns))
	return nil
}

var deploymentTmpl = template.Must(template.New("deployment").Funcs(template.FuncMap{
	"iterate": func(n int32) []int32 {
		s := make([]int32, n)
		for i := range s {
			s[i] = int32(i)
		}
		return s
	},
}).Parse(
	`{{- range $i, $_ := iterate .Replicas }}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $.Name }}{{ $i }}
  namespace: {{ "{{" }} .Values.global.namespace {{ "}}" }}
  labels:
    {{ "{{- include \"schematic.labels\" . | nindent 4 }}" }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ $.Name }}{{ $i }}
  template:
    metadata:
      labels:
        app: {{ $.Name }}{{ $i }}
        {{ "{{- include \"schematic.labels\" . | nindent 8 }}" }}
    spec:
      {{ "{{- include \"schematic.imagePullSecrets\" . | nindent 6 }}" }}
      hostname: {{ $.Name }}{{ $i }}
      {{- if $.HasPVC }}
      securityContext:
        fsGroup: 65532
      {{- end }}
      containers:
      - name: {{ $.Name }}{{ $i }}
        image: {{ "{{" }} .Values.{{ $.Name }}.image {{ "}}" }}
        imagePullPolicy: {{ "{{" }} .Values.global.imagePullPolicy {{ "}}" }}
        env:
        - name: BOOTSTRAP_CONFIG
          value: /data/bootstrap.yaml
        ports:
        - containerPort: 5000
          name: grpc
        {{- if $.HasHazelcast }}
        - containerPort: 5001
          name: hazelcast
        {{- end }}
        volumeMounts:
        - name: config
          mountPath: /data/bootstrap.yaml
          subPath: config.yaml
        {{- if $.HasPVC }}
        - name: data
          mountPath: /app/data
        {{- end }}
        startupProbe:
          {{ "{{- include \"schematic.startupProbe5000\" . | nindent 10 }}" }}
        readinessProbe:
          {{ "{{- include \"schematic.grpcProbe5000\" . | nindent 10 }}" }}
        livenessProbe:
          {{ "{{- include \"schematic.grpcProbe5000\" . | nindent 10 }}" }}
      volumes:
      - name: config
        configMap:
          name: {{ $.Name }}{{ $i }}-config
      {{- if $.HasPVC }}
      - name: data
        persistentVolumeClaim:
          claimName: {{ $.Name }}{{ $i }}-data
      {{- end }}
{{- end }}
`))

// secretEnvSecretName is the Secret holding every secret-env field of one
// service. Named for the service, not the pod: a set of pods delivered a field
// this way has to agree on it, and a per-ordinal Secret could differ.
func secretEnvSecretName(svc serviceSpec) string {
	return svc.Name + "-pipeline-secrets"
}

// secretEnvLines renders the container env entries that read that Secret.
func secretEnvLines(svc serviceSpec) string {
	var buf bytes.Buffer
	for _, e := range svc.SecretEnvVars {
		buf.WriteString(fmt.Sprintf(`        - name: %s
          valueFrom:
            secretKeyRef:
              name: %s
              key: %s
`, e.EnvName, secretEnvSecretName(svc), e.Key))
	}
	return buf.String()
}

// pipelineSecretsConfigMapName is the ConfigMap the deployment pipeline reads to
// learn which Secrets to write.
const pipelineSecretsConfigMapName = "schematic-pipeline-secrets"

// pipelineSecret is one entry of that manifest: a Secret the pipeline writes,
// the GitLab variable behind each of its keys, and the workloads to restart
// when its content changes (env from a secretKeyRef is read once, at container
// start).
type pipelineSecret struct {
	Name      string         `json:"name"`
	Workloads []string       `json:"workloads"`
	Entries   []secretEnvVar `json:"entries"`
}

// pipelineSecretsConfigMap renders the manifest of every SecretEnvVars Secret
// the chart references, so the pipeline iterates over it instead of hardcoding
// a list per secret. It is always rendered, empty when no enabled service
// declares one, so a missing ConfigMap means an older operator, not "nothing
// to write".
func pipelineSecretsConfigMap(svcs []serviceSpec) ([]byte, error) {
	secrets := []pipelineSecret{}
	for _, svc := range svcs {
		if len(svc.SecretEnvVars) == 0 {
			continue
		}
		workloads := []string{}
		if svc.IsStatefulSet {
			workloads = []string{"statefulset/" + svc.Name}
		} else {
			for i := range int(svc.Replicas) {
				workloads = append(workloads, fmt.Sprintf("deployment/%s%d", svc.Name, i))
			}
		}
		secrets = append(secrets, pipelineSecret{Name: secretEnvSecretName(svc), Workloads: workloads, Entries: svc.SecretEnvVars})
	}
	manifest, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
data:
  secrets.json: |
%s
`, pipelineSecretsConfigMapName, indentLines(string(manifest), "    "))), nil
}

// configVolumeSource renders the volume source for a service's config volume:
// a secret for key-bearing configs, a configMap otherwise.
func configVolumeSource(svc serviceSpec, name string) string {
	if svc.ConfigHasKeyMaterial {
		return fmt.Sprintf("        secret:\n          secretName: %s-config\n", name)
	}
	return fmt.Sprintf("        configMap:\n          name: %s-config\n", name)
}

// hazelcastNetworkPolicy confines the Hazelcast member ports of one service to
// that service's own pods.
//
// The member protocol is the hole: no TLS, no member authentication, and Java
// serialization on the wire. Anything that completes a TCP handshake to it is a
// peer, so "who can open the socket" IS the authentication — and with no
// NetworkPolicy in the namespace that was every pod in every namespace of the
// cluster.
//
// Shape, and why it is this shape rather than a tighter one:
//
//   - Rule 1 names the member ports and admits only pods carrying this service's
//     own "app" label. A bare podSelector in a NetworkPolicy peer means "in THIS
//     namespace", which is exactly the intent — a pod called "stratum" in some
//     other namespace is not a member.
//   - Rule 2 has no "from" at all, so it admits every source, and names the gRPC
//     service port and the health port. Selecting a pod turns on default-deny for
//     everything the policy does not name, so these have to be written down or
//     locking the member port takes the service off the network. Leaving them open
//     to the whole cluster preserves today's trust boundary rather than inventing
//     a new one in the same change: the gRPC port's control is S2S auth, not the
//     network, and the health port serves grpc.health.v1 and nothing else.
//   - policyTypes is Ingress only. Egress is left alone deliberately: the exposure
//     is inbound, and an egress allowlist would have to enumerate DNS, object
//     storage, the token issuer and every S2S callee — much more to get wrong, for
//     none of this particular risk.
func hazelcastNetworkPolicy(svc serviceSpec) []byte {
	listenPort := svc.ListenPort
	if listenPort == 0 {
		listenPort = 5000
	}
	var memberPorts bytes.Buffer
	if svc.HasHazelcast {
		memberPorts.WriteString(fmt.Sprintf(`    - protocol: TCP
      port: %d
`, stratumHazelcastPort))
	}
	if svc.HasWorkingSetHazelcast {
		memberPorts.WriteString(fmt.Sprintf(`    - protocol: TCP
      port: %d
`, stratumWorkingSetPort))
	}
	// This workload's own pods, plus every other workload in the same Hazelcast
	// cluster. One selector per app label rather than one with several labels: a
	// single matchLabels is an AND, so listing two app values there would match
	// nothing at all.
	var peerSelectors bytes.Buffer
	for _, app := range append([]string{svc.Name}, svc.HazelcastPeerApps...) {
		peerSelectors.WriteString(fmt.Sprintf(`    - podSelector:
        matchLabels:
          app: %s
`, app))
	}
	return []byte(fmt.Sprintf(`apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: %s-hazelcast
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
spec:
  podSelector:
    matchLabels:
      app: %s
  policyTypes:
  - Ingress
  ingress:
  # Hazelcast member discovery and cluster join: peers only.
  - from:
%s    ports:
%s  # Everything this service actually serves stays reachable as it is today.
  - ports:
    - protocol: TCP
      port: %d
    - protocol: TCP
      port: %d
`, svc.Name, svc.Name, peerSelectors.String(), memberPorts.String(), listenPort, grpcHealthProbePort))
}

// loadBalancerIPLine pins the cluster's one external LoadBalancer Service —
// the gateway's -lb, carrying TCP/443 and TCP/80 — to its reserved static
// external address. fqdn's public A record resolves to it, and ACME HTTP-01
// validation connects to port 80 on that same name. Without this the provider
// hands the Service an ephemeral address, so a re-render silently moves the
// gateway off the address DNS points at.
//
// An empty external_ip renders nothing, leaving local and dev clusters on the
// provider-assigned address they have always had.
func loadBalancerIPLine(cfg *protoOperator.Config) string {
	if cfg.GetExternalIp() == "" {
		return ""
	}
	return fmt.Sprintf("  loadBalancerIP: %s\n", cfg.GetExternalIp())
}

func serviceAccountYAML(name string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
`, name))
}

func (g *Generator) addService(charts *Charts, cfg *protoOperator.Config, svc serviceSpec) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}
	if svc.AutomountServiceAccountToken == nil {
		svc.AutomountServiceAccountToken = falsePtr()
	}

	// A second workload of another service emits no ServiceAccount and no RBAC of its
	// own: it USES that service's, so a second object of the same name here would be
	// a duplicate resource helm refuses to apply.
	if svc.ServiceAccountName != "" && svc.SharesServiceIdentityWith == "" {
		charts.Files[fmt.Sprintf("templates/%s/serviceaccount.yaml", svc.Name)] = serviceAccountYAML(svc.ServiceAccountName)
	}
	if svc.TokenReviewRBAC && svc.ServiceAccountName != "" && svc.SharesServiceIdentityWith == "" {
		// ClusterRole/ClusterRoleBinding are cluster-scoped, so their names must be
		// namespace-qualified: two fleets in two namespaces on one cluster would
		// otherwise collide on the same fixed name, with the second deploy silently
		// rewriting the first's binding to its own subject namespace.
		tokenReviewName := fmt.Sprintf("%s-%s-tokenreview", ns, svc.Name)
		charts.Files[fmt.Sprintf("templates/%s/tokenreview-rbac.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %s
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
rules:
- apiGroups: ["authentication.k8s.io"]
  resources: ["tokenreviews"]
  verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %s
subjects:
- kind: ServiceAccount
  name: %s
  namespace: {{ .Values.global.namespace }}
`, tokenReviewName, tokenReviewName, tokenReviewName, svc.ServiceAccountName))
	}
	if svc.PodReadRBAC && svc.ServiceAccountName != "" && svc.SharesServiceIdentityWith == "" {
		charts.Files[fmt.Sprintf("templates/%s/pod-rbac.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %s-pod-reader
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s-pod-reader
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %s-pod-reader
subjects:
- kind: ServiceAccount
  name: %s
  namespace: {{ .Values.global.namespace }}
`, svc.Name, svc.Name, svc.Name, svc.ServiceAccountName))
	}

	// ConfigMap — or a Secret, for configs that embed private key material.
	kind, typeLine, dataField, fileStem := "ConfigMap", "", "data", "configmap"
	if svc.ConfigHasKeyMaterial {
		kind, typeLine, dataField, fileStem = "Secret", "type: Opaque\n", "stringData", "secret"
	}
	for i := range int(svc.Replicas) {
		// StatefulSet pods use hyphenated names (kernel-0); Deployments use kernel0.
		var name string
		if svc.IsStatefulSet {
			name = fmt.Sprintf("%s-%d", svc.Name, i)
		} else {
			name = fmt.Sprintf("%s%d", svc.Name, i)
		}
		configValueKey := configValueKey(svc, i)
		cmData := fmt.Sprintf(`apiVersion: v1
kind: %s
metadata:
  name: %s-config
  namespace: {{ .Values.global.namespace }}
%s%s:
  config.yaml: {{ %s | quote }}
`, kind, name, typeLine, dataField, valuesRef(configValueKey, "config"))
		if svc.HasServiceConfig {
			cmData += fmt.Sprintf("  service.yaml: {{ %s | quote }}\n", valuesRef(configValueKey, "serviceConfig"))
		}
		charts.Files[fmt.Sprintf("templates/%s/%s-%d.yaml", svc.Name, fileStem, i)] = []byte(cmData)

		// PVC if needed — StatefulSet manages PVCs via volumeClaimTemplates.
		if svc.HasPVC && !svc.IsStatefulSet {
			charts.Files[fmt.Sprintf("templates/%s/pvc-%d.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s-data
  namespace: {{ .Values.global.namespace }}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: {{ %s }}
`, name, valuesRef(svc.Name, "storage")))
		}

		// Service
		listenPort := svc.ListenPort
		if listenPort == 0 {
			listenPort = 5000
		}
		hazelcastPort := ""
		if svc.HasHazelcast {
			hazelcastPort = fmt.Sprintf(`  - name: hazelcast
    port: %d
    targetPort: %d
`, stratumHazelcastPort, stratumHazelcastPort)
		}
		if svc.HasWorkingSetHazelcast {
			hazelcastPort += fmt.Sprintf(`  - name: hazelcast-ws
    port: %d
    targetPort: %d
`, stratumWorkingSetPort, stratumWorkingSetPort)
		}
		extraPort := ""
		if svc.ExtraPort != 0 {
			extraPort = fmt.Sprintf(`  - name: %s
    port: %d
    targetPort: %d
`, svc.ExtraPortName, svc.ExtraPort, svc.ExtraPort)
		}
		// The ACME HTTP-01 responder, reachable in-cluster only. The gateway is
		// the one caller: it holds the edge's port 80 and proxies
		// /.well-known/acme-challenge/* here.
		if svc.AcmePort != 0 {
			extraPort += fmt.Sprintf(`  - name: acme
    port: %d
    targetPort: %d
`, svc.AcmePort, svc.AcmePort)
		}
		for _, np := range svc.NamedPorts {
			extraPort += fmt.Sprintf(`  - name: %s
    port: %d
    targetPort: %d
`, np.Name, np.Port, np.Port)
		}
		// Per-replica ClusterIP service for direct gRPC addressing.
		var svcSelector string
		if svc.IsStatefulSet {
			svcSelector = fmt.Sprintf("    statefulset.kubernetes.io/pod-name: %s", name)
		} else {
			svcSelector = fmt.Sprintf("    app: %s", name)
		}
		charts.Files[fmt.Sprintf("templates/%s/service-%d.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
spec:
  selector:
%s
  ports:
  - name: grpc
    port: %d
    targetPort: %d
%s%s`, name, svcSelector, listenPort, listenPort, hazelcastPort, extraPort))

		// External LoadBalancer service for services with a configured external port
		// (e.g. gateway exposed on port 443). This is a separate service to avoid
		// HostPort conflicts with other services using the same container port.
		if svc.Port != 0 {
			// Port 80 rides the SAME LoadBalancer as 443 when the service
			// declares an HttpPort. One Service, one forwarding rule set, one
			// reserved address: the public A record and ACME HTTP-01
			// validation both land on this pod. A second LoadBalancer for
			// port 80 would take an address of its own, which HTTP-01 never
			// reaches — it connects to port 80 on the NAME.
			httpPort := ""
			if svc.HttpPort != 0 {
				httpPort = fmt.Sprintf(`  - name: http
    port: 80
    targetPort: %d
`, svc.HttpPort)
			}
			charts.Files[fmt.Sprintf("templates/%s/service-%d-lb.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s-lb
  namespace: {{ .Values.global.namespace }}
spec:
  type: LoadBalancer
%s  selector:
    app: %s
  ports:
  - name: https
    port: %d
    targetPort: %d
%s`, name, loadBalancerIPLine(cfg), name, svc.Port, listenPort, httpPort))
		}
	}

	// Headless Service for StatefulSet DNS discovery.
	if svc.IsStatefulSet {
		listenPort := svc.ListenPort
		if listenPort == 0 {
			listenPort = 5000
		}
		hazelcastHeadless := ""
		if svc.HasHazelcast {
			hazelcastHeadless = fmt.Sprintf(`  - name: hazelcast
    port: %d
    targetPort: %d
`, stratumHazelcastPort, stratumHazelcastPort)
		}
		if svc.HasWorkingSetHazelcast {
			hazelcastHeadless += fmt.Sprintf(`  - name: hazelcast-ws
    port: %d
    targetPort: %d
`, stratumWorkingSetPort, stratumWorkingSetPort)
		}
		// A StatefulSet's headless Service is what makes `<pod>.<service>:<port>`
		// resolve, so a port a PEER dials has to be here and not only on the
		// per-replica ClusterIP Service. blobstore's mirror addresses its peer
		// exactly that way.
		for _, np := range svc.NamedPorts {
			hazelcastHeadless += fmt.Sprintf(`  - name: %s
    port: %d
    targetPort: %d
`, np.Name, np.Port, np.Port)
		}
		charts.Files[fmt.Sprintf("templates/%s/service-headless.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
spec:
  clusterIP: None
  # Peer DNS must resolve BEFORE a pod is Ready. Hazelcast joins at startup by
  # resolving the seed list (stratum-0.stratum, …), and a pod is not Ready until
  # after it has joined — so with the default (not-ready addresses withheld) every
  # pod in a cold start resolves nothing, decides it is alone, and forms its own
  # single-member cluster. Observed exactly that: 7x "Cannot resolve hostname
  # 'stratum-0.stratum'" per pod, three one-member clusters, each running the
  # cold-boot event-log replay independently, and a query answered by one pod
  # returning 0 records while another returned 57. They only converged later via
  # Hazelcast's split-brain merge.
  publishNotReadyAddresses: true
  selector:
    app: %s
  ports:
  - name: grpc
    port: %d
    targetPort: %d
%s`, svc.Name, svc.Name, listenPort, listenPort, hazelcastHeadless))
	}

	if svc.HazelcastNetworkPolicy {
		charts.Files[fmt.Sprintf("templates/%s/networkpolicy.yaml", svc.Name)] =
			hazelcastNetworkPolicy(svc)
	}
	if pdb := podDisruptionBudget(svc); pdb != nil {
		charts.Files[fmt.Sprintf("templates/%s/pdb.yaml", svc.Name)] = pdb
	}

	listenPort := svc.ListenPort
	if listenPort == 0 {
		listenPort = 5000
	}
	probeHelper := "schematic.grpcProbe5000"
	startupProbeHelper := "schematic.startupProbe5000"
	if listenPort == 8443 {
		probeHelper = "schematic.grpcProbe8443"
		startupProbeHelper = "schematic.startupProbe8443"
	}
	// stratum's cold start now includes warm Lucene index open + redo-log replay,
	// which the shared 300s startup budget is too tight for — stratum-2 was
	// SIGTERM'd twice mid-replay on a live rolling restart. Give stratum alone a
	// longer budget; the shared helper still applies to every other service.
	// Both stratum workloads. A search member opens no Lucene index and replays no
	// redo log, but it does wait at the same cluster-formation and hot-restore
	// barriers the writers do — it is handed partitions the moment it joins, so it
	// must not answer a query before the writers have recovered. The shared 300s
	// budget is too tight for that wait for the same reason it is too tight for a
	// writer's.
	if svc.Name == "stratum" || svc.Name == stratumSearchName {
		startupProbeHelper = "schematic.startupProbeStratum"
	}
	// eventbus needs a far longer liveness budget than the shared 30s: its
	// embedded broker makes every restart a rebalance storm it must be allowed
	// to finish. See schematic.grpcProbeEventbus.
	if svc.Name == "eventbus" {
		probeHelper = "schematic.grpcProbeEventbus"
	}

	if svc.IsStatefulSet {
		// Generate a StatefulSet with an init container that selects the
		// per-ordinal ConfigMap based on the pod's hostname (kernel-0 → 0).
		podManagementPolicy := "Parallel"
		if svc.OrderedReady {
			podManagementPolicy = "OrderedReady"
		}
		var ssBuf bytes.Buffer
		ssBuf.WriteString(fmt.Sprintf(`---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
spec:
  serviceName: %s
  replicas: %d
  podManagementPolicy: %s
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
%s        {{- include "schematic.labels" . | nindent 8 }}
`, svc.Name, svc.Name, svc.Replicas, podManagementPolicy, svc.Name, svc.Name, servicePodLabelLine(svc)))
		ssBuf.WriteString(`      annotations:
`)
		for i := range int(svc.Replicas) {
			ssBuf.WriteString(fmt.Sprintf(`        checksum/config-%d: {{ %s | sha256sum | quote }}
`, i, valuesRef(configValueKey(svc, i), "config")))
		}
		ssBuf.WriteString(`    spec:
      {{- include "schematic.imagePullSecrets" . | nindent 6 }}
`)
		ssBuf.WriteString(podServiceAccountLines(svc))
		// The StatefulSet path was missing this while the Deployment path had it,
		// so the two services that own a volumeClaimTemplate -- kernel and stratum
		// -- were the two that could not write to their own volume. See
		// pvcFsGroupLines: a fresh PV is root:root 0755 and these images run as
		// `nonroot`.
		ssBuf.WriteString(pvcFsGroupLines(svc))
		ssBuf.WriteString(`      initContainers:
      - name: config-select
        image: busybox:1.36
        command: ["sh", "-c", "ORDINAL=${HOSTNAME##*-}; cp /configs/config-${ORDINAL}/config.yaml /bootstrap/bootstrap.yaml"]
        volumeMounts:
        - name: bootstrap
          mountPath: /bootstrap
`)
		// Mount each per-ordinal ConfigMap in the init container.
		for i := range int(svc.Replicas) {
			ssBuf.WriteString(fmt.Sprintf(`        - name: config-%d
          mountPath: /configs/config-%d
`, i, i))
		}
		bootstrapEnv := `        - name: BOOTSTRAP_CONFIG
          value: /bootstrap/bootstrap.yaml
`
		ssBuf.WriteString(archTolerationLines())
		ssBuf.WriteString(nodePoolLines(svc))
		ssBuf.WriteString(spreadAcrossNodesLines(svc))
		ssBuf.WriteString(fmt.Sprintf(`      containers:
      - name: %s
        image: {{ %s }}
        imagePullPolicy: {{ .Values.global.imagePullPolicy }}
        env:
%s        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
`, svc.Name, valuesRef(svc.Name, "image"), bootstrapEnv))
		if jo := javaOptsEnv(svc); jo != "" {
			ssBuf.WriteString(jo)
		}
		if gm := goMemLimitEnv(svc); gm != "" {
			ssBuf.WriteString(gm)
		}
		if svc.ExtraEnvVars != "" {
			ssBuf.WriteString(svc.ExtraEnvVars)
		}
		ssBuf.WriteString(secretEnvLines(svc))
		ssBuf.WriteString(fmt.Sprintf(`        ports:
        - containerPort: %d
          name: grpc
`, listenPort))
		if svc.HasHazelcast {
			ssBuf.WriteString(fmt.Sprintf(`        - containerPort: %d
          name: hazelcast
`, stratumHazelcastPort))
		}
		if svc.HasWorkingSetHazelcast {
			ssBuf.WriteString(fmt.Sprintf(`        - containerPort: %d
          name: hazelcast-ws
`, stratumWorkingSetPort))
		}
		for _, np := range svc.NamedPorts {
			ssBuf.WriteString(fmt.Sprintf(`        - containerPort: %d
          name: %s
`, np.Port, np.Name))
		}
		ssBuf.WriteString(`        volumeMounts:
        - name: bootstrap
          mountPath: /bootstrap
`)
		ssBuf.WriteString(projectedServiceAccountTokenMount(svc))
		if svc.HasPVC {
			ssBuf.WriteString(`        - name: data
          mountPath: /app/data
`)
		}
		if !svc.SkipProbes {
			ssBuf.WriteString(fmt.Sprintf(`        startupProbe:
          {{- include "%s" . | nindent 10 }}
        readinessProbe:
          {{- include "%s" . | nindent 10 }}
        livenessProbe:
          {{- include "%s" . | nindent 10 }}
`, startupProbeHelper, probeHelper, probeHelper))
		}
		if rb := resourcesBlock(svc); rb != "" {
			ssBuf.WriteString(rb)
		}
		ssBuf.WriteString(`      volumes:
      - name: bootstrap
        emptyDir: {}
`)
		ssBuf.WriteString(projectedServiceAccountTokenVolume(svc))
		for i := range int(svc.Replicas) {
			name := fmt.Sprintf("%s-%d", svc.Name, i)
			ssBuf.WriteString(fmt.Sprintf(`      - name: config-%d
%s`, i, configVolumeSource(svc, name)))
		}
		if svc.HasPVC {
			ssBuf.WriteString(fmt.Sprintf(`  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      resources:
        requests:
          storage: {{ %s }}
`, valuesRef(svc.Name, "storage")))
		}
		charts.Files[fmt.Sprintf("templates/%s/statefulset.yaml", svc.Name)] = ssBuf.Bytes()
	} else {
		// Generate individual Deployments — one per replica, named <service><index>.
		var deployBuf bytes.Buffer
		for i := range int(svc.Replicas) {
			name := fmt.Sprintf("%s%d", svc.Name, i)
			// A Deployment that owns a ReadWriteOnce PVC must NOT surge a second
			// pod: the default RollingUpdate starts the new pod before the old one
			// terminates, both mount the same volume on this single node, and the
			// old pod still holds the data-dir lock. The new pod then fails to
			// start, never becomes Ready, so the old pod is never torn down — a
			// deadlock. Observed on eventbus: the new pod CrashLooped on Kafka's
			// data-dir lock while the old pod kept serving.
			strategy := ""
			if svc.HasPVC {
				strategy = "  strategy:\n    type: Recreate\n"
			}
			deployBuf.WriteString(fmt.Sprintf(`---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
spec:
  replicas: 1
%s  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
%s        {{- include "schematic.labels" . | nindent 8 }}
      annotations:
        checksum/config: {{ %s | sha256sum | quote }}
`, name, strategy, name, name, servicePodLabelLine(svc), valuesRef(svc.Name, "config")))
			if svc.HasServiceConfig {
				deployBuf.WriteString(fmt.Sprintf(`        checksum/service-config: {{ %s | sha256sum | quote }}
`, valuesRef(svc.Name, "serviceConfig")))
			}
			deployBuf.WriteString(fmt.Sprintf(`    spec:
      {{- include "schematic.imagePullSecrets" . | nindent 6 }}
%s
      hostname: %s
%s%s      containers:
      - name: %s
        image: {{ %s }}
        imagePullPolicy: {{ .Values.global.imagePullPolicy }}
        env:
        - name: BOOTSTRAP_CONFIG
          value: /data/bootstrap.yaml
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
`, podServiceAccountLines(svc), name, archTolerationLines()+nodePoolLines(svc)+spreadAcrossNodesLines(svc), pvcFsGroupLines(svc), name, valuesRef(svc.Name, "image")))
			if jo := javaOptsEnv(svc); jo != "" {
				deployBuf.WriteString(jo)
			}
			if gm := goMemLimitEnv(svc); gm != "" {
				deployBuf.WriteString(gm)
			}
			if svc.WebAppHostPath != "" {
				// Dev-mode webapp hot reload: gateway reads WEB_ROOT and serves
				// SPA files from disk instead of the embedded bundle. The host
				// directory is bind-mounted via hostPath below.
				deployBuf.WriteString("        - name: WEB_ROOT\n          value: /web/app\n")
			}
			if svc.ExtraEnvVars != "" {
				deployBuf.WriteString(svc.ExtraEnvVars)
			}
			deployBuf.WriteString(secretEnvLines(svc))
			if len(svc.NodeModes) > 0 {
				nodeMode := "alpha"
				if i < len(svc.NodeModes) {
					nodeMode = svc.NodeModes[i]
				}
				deployBuf.WriteString(fmt.Sprintf(`        - name: NODE_MODE
          value: %s
`, nodeMode))
			}
			deployBuf.WriteString(fmt.Sprintf(`        ports:
        - containerPort: %d
          name: grpc
`, listenPort))
			if svc.AcmePort != 0 {
				deployBuf.WriteString(fmt.Sprintf(`        - containerPort: %d
          name: acme
`, svc.AcmePort))
			}
			if svc.HttpPort != 0 {
				deployBuf.WriteString(fmt.Sprintf(`        - containerPort: %d
          name: http
`, svc.HttpPort))
			}
			if svc.HasHazelcast {
				deployBuf.WriteString(`        - containerPort: 5001
          name: hazelcast
`)
			}
			deployBuf.WriteString(`        volumeMounts:
        - name: config
          mountPath: /data/bootstrap.yaml
          subPath: config.yaml
`)
			if svc.HasServiceConfig {
				deployBuf.WriteString(`        - name: config
          mountPath: /data/service.yaml
          subPath: service.yaml
`)
			}
			if svc.HasPVC {
				deployBuf.WriteString(`        - name: data
          mountPath: /app/data
`)
			}
			deployBuf.WriteString(projectedServiceAccountTokenMount(svc))
			if svc.WebAppHostPath != "" {
				deployBuf.WriteString(`        - name: webapp
          mountPath: /web/app
          readOnly: true
`)
			}
			if !svc.SkipProbes {
				deployBuf.WriteString(fmt.Sprintf(`        startupProbe:
          {{- include "%s" . | nindent 10 }}
        readinessProbe:
          {{- include "%s" . | nindent 10 }}
        livenessProbe:
          {{- include "%s" . | nindent 10 }}
`, startupProbeHelper, probeHelper, probeHelper))
			}
			if rb := resourcesBlock(svc); rb != "" {
				deployBuf.WriteString(rb)
			}
			deployBuf.WriteString(`      volumes:
      - name: config
`)
			deployBuf.WriteString(configVolumeSource(svc, name))
			if svc.HasPVC {
				deployBuf.WriteString(fmt.Sprintf(`      - name: data
        persistentVolumeClaim:
          claimName: %s-data
`, name))
			}
			deployBuf.WriteString(projectedServiceAccountTokenVolume(svc))
			if svc.WebAppHostPath != "" {
				deployBuf.WriteString(fmt.Sprintf(`      - name: webapp
        hostPath:
          path: %s
          type: Directory
`, svc.WebAppHostPath))
			}
		}
		charts.Files[fmt.Sprintf("templates/%s/deployment.yaml", svc.Name)] = deployBuf.Bytes()
	}

	return nil
}

// --- runtime test harness (D27) ---
//
// The harness is a client, not a service: no Deployment, no Service, no config
// in kernel's seed, no kernel delegate. It is rendered as suspended CronJob
// templates the pipeline runs with `kubectl create job --from=cronjob/<name>`,
// so the image and pod spec stay under operator rendering and a run is never a
// hand-written manifest. Design: looper/schematic/decisions/
// d27-runtime-test-harness-design-2026-10-02.md §2.2.

// harnessAccountsSecret holds the harness tenants' account passwords. The
// pipeline writes it from GitLab CI variables (the D25 pipeline-provisioned
// Secret): the chart references it and never renders it, so the operator never
// holds a password. Its keys are the environment variable names the harness
// reads.
const harnessAccountsSecret = "harness-accounts"

// harnessChaosPods and harnessChaosPVCs are everything the destructive profile
// may delete, by name. An empty resourceNames would grant every object of the
// kind, which is why these are fixed lists and never derived to possibly-empty
// ones.
var (
	harnessChaosPods = []string{"blobstore-0", "blobstore-1", "kernel-0", "kernel-1", "kernel-2"}
	harnessChaosPVCs = []string{"data-blobstore-0", "data-blobstore-1"}
)

// addHarness renders the harness templates when optional_services.harness is
// set: the gate profile always, the destructive profile only with
// harness_destructive as well.
func (g *Generator) addHarness(charts *Charts, cfg *protoOperator.Config) error {
	opt := cfg.GetOptionalServices()
	if !opt.GetHarness() {
		return nil
	}
	image := g.imageRef(cfg, "harness")

	gate, err := harnessCronJob("harness", "harness", image, harnessConfig(cfg, protoHarness.Profile_PROFILE_GATE))
	if err != nil {
		return err
	}
	charts.Files["templates/harness/serviceaccount.yaml"] = serviceAccountYAML("harness")
	charts.Files["templates/harness/cronjob.yaml"] = gate

	if !opt.GetHarnessDestructive() {
		return nil
	}
	destructive, err := harnessCronJob("harness-destructive", "harness-chaos", image, harnessConfig(cfg, protoHarness.Profile_PROFILE_DESTRUCTIVE))
	if err != nil {
		return err
	}
	charts.Files["templates/harness-destructive/serviceaccount.yaml"] = serviceAccountYAML("harness-chaos")
	charts.Files["templates/harness-destructive/cronjob.yaml"] = destructive
	charts.Files["templates/harness-destructive/chaos-rbac.yaml"] = harnessChaosRBAC()
	return nil
}

// harnessConfig is the harness Job's runtime config. expected_digests are the
// image digests this fleet is pinned to (cfg.image_digests, which the pipeline
// writes), as "<image>@<digest>" sorted by image, so the report a run produces
// can be matched to the digest set it ran against.
func harnessConfig(cfg *protoOperator.Config, profile protoHarness.Profile) *protoHarness.Config {
	images := make([]string, 0, len(cfg.GetImageDigests()))
	for image := range cfg.GetImageDigests() {
		images = append(images, image)
	}
	sort.Strings(images)
	digests := make([]string, 0, len(images))
	for _, image := range images {
		digests = append(digests, image+"@"+cfg.GetImageDigests()[image])
	}
	return &protoHarness.Config{Fqdn: cfg.GetFqdn(), Profile: profile, ExpectedDigests: digests}
}

// harnessCronJob renders one profile's CronJob.
//
// The pod mounts no ambient token and its ServiceAccount has no kernel
// delegate, so it cannot mint an s2s token even by accident. The one exception
// is harness-chaos, which gets an explicit projected Kubernetes API token: the
// destructive profile deletes pods and PVCs through the API, under the Role
// harnessChaosRBAC grants and nothing more.
//
// The config travels as the env names tacplus's generated ConfigToEnvVar
// produces — the same ones the harness reads with config_from_env_var — and is
// validated here against the proto's constraints, so a config the harness
// would refuse fails generation instead of a run.
func harnessCronJob(name, serviceAccount, image string, hc *protoHarness.Config) ([]byte, error) {
	if err := protoHarness.ValidateConfig(hc); err != nil {
		return nil, fmt.Errorf("%s config: %w", name, err)
	}
	env := protoHarness.ConfigToEnvVar(hc)
	envNames := make([]string, 0, len(env))
	for k := range env {
		envNames = append(envNames, k)
	}
	sort.Strings(envNames)

	chaos := serviceAccount == "harness-chaos"
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf(`apiVersion: batch/v1
kind: CronJob
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
spec:
  # A template, not a schedule: suspend keeps it from ever firing, and a run is
  # started with kubectl create job --from=cronjob/%s.
  schedule: "0 0 1 1 *"
  suspend: true
  jobTemplate:
    spec:
      # A run is a test result. A retry would turn a red run green, and a
      # destructive one would break the fleet twice.
      backoffLimit: 0
      template:
        metadata:
          labels:
            app: %s
            {{- include "schematic.labels" . | nindent 12 }}
        spec:
          {{- include "schematic.imagePullSecrets" . | nindent 10 }}
          serviceAccountName: %s
          automountServiceAccountToken: false
          restartPolicy: Never
`, name, name, name, serviceAccount))
	b.WriteString(indentLines(archTolerationLines(), "    "))
	b.WriteString(fmt.Sprintf(`          containers:
          - name: %s
            image: %s
            imagePullPolicy: {{ .Values.global.imagePullPolicy }}
            env:
`, name, image))
	for _, k := range envNames {
		b.WriteString(fmt.Sprintf("            - name: %s\n              value: %q\n", k, env[k]))
	}
	b.WriteString(fmt.Sprintf(`            envFrom:
            - secretRef:
                name: %s
            resources:
              requests:
                memory: 64Mi
                cpu: 25m
              limits:
                memory: 256Mi
`, harnessAccountsSecret))
	if chaos {
		b.WriteString(`            volumeMounts:
            - name: kubernetes-api-token
              mountPath: /var/run/secrets/kubernetes.io/serviceaccount
              readOnly: true
          volumes:
`)
		b.WriteString(indentLines(projectedServiceAccountTokenProjectionVolume(&serviceAccountTokenProjection{
			VolumeName:        "kubernetes-api-token",
			TokenPath:         "token",
			ExpirationSeconds: 3600,
			IncludeKubeRootCA: true,
			IncludeNamespace:  true,
		}), "    "))
	}
	return b.Bytes(), nil
}

// harnessChaosRBAC is the destructive profile's only grant: delete, by name,
// on the pods and PVCs its scenarios break. No get, no list, no other verb.
func harnessChaosRBAC() []byte {
	return []byte(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: harness-chaos
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["delete"]
  resourceNames: [%s]
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["delete"]
  resourceNames: [%s]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: harness-chaos
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: harness-chaos
subjects:
- kind: ServiceAccount
  name: harness-chaos
  namespace: {{ .Values.global.namespace }}
`, quotedList(harnessChaosPods), quotedList(harnessChaosPVCs)))
}

// quotedList renders names as the body of a YAML flow sequence.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return strings.Join(quoted, ", ")
}

// indentLines prefixes every non-empty line of s. The pod-spec helpers render
// at a Deployment's depth; a CronJob's pod spec sits two levels deeper.
func indentLines(s, prefix string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, l := range lines {
		if l != "" && l != "\n" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "")
}
