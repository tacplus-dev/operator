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
//	    sentinel/
//	    gateway/       (one Deployment per gateway_nodes)
//	    eventbus/
//	    oauth/
//	    ... (optional services when enabled)
package helm

import (
	"sort"
	"strings"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
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
}

// New returns a new Generator.
// series is the image series (e.g. "1-2") and versionLock is the MAJOR.MINOR
// string (e.g. "1.2"), both derived from the operator binary version at startup.
// resolver provides per-service image tag resolution; pass nil to always use "latest".
func New(series, versionLock string, resolver TagResolver) *Generator {
	return &Generator{series: series, versionLock: versionLock, resolver: resolver}
}

// Generate produces the full schematic Helm chart for the given config.
// nodeCount is the number of schedulable Kubernetes nodes; it is used to
// set gateway_nodes when topology.gateway_nodes == 0.
func (g *Generator) Generate(cfg *protoOperator.Config, nodeCount int) (*Charts, error) {
	topo := effectiveTopology(cfg, nodeCount)

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
		if err := g.addService(charts, cfg, svc); err != nil {
			return nil, fmt.Errorf("generating %s templates: %w", svc.Name, err)
		}
	}

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
		if err := os.WriteFile(full, content, 0644); err != nil {
			return fmt.Errorf("writing %s: %w", relPath, err)
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
// Peer wiring by env follows graphdb's GRAPHDB_ZERO_ADDR rather than inventing a
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

func stratumSeedMembersEnv(replicas int32) string {
	return stratumSeedEnv("STRATUM_SEED_MEMBERS", replicas, stratumHazelcastPort) +
		stratumSeedEnv("STRATUM_WS_SEED_MEMBERS", replicas, stratumWorkingSetPort)
}

func stratumSeedEnv(name string, replicas int32, port int) string {
	if replicas <= 0 {
		return ""
	}
	var members []string
	for i := int32(0); i < replicas; i++ {
		// <pod>.<headless service>:<hazelcast port>. A StatefulSet-shaped
		// service is named for the service itself — pods are stratum-0,
		// stratum-1, ... and the headless Service is "stratum" — NOT the
		// stratum0 form a Deployment uses. Getting this wrong produces seed
		// addresses that never resolve, and Hazelcast simply never forms a
		// cluster: no error, just members that each sit alone.
		members = append(members, fmt.Sprintf("stratum-%d.stratum:%d", i, port))
	}
	return fmt.Sprintf("        - name: %s\n          value: %q\n", name, strings.Join(members, ","))
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
	ListenPort       int32 // gRPC listen port: 5000 for Java (kernel), 8443 for Rust/Tacplus
	// NodeModes, if non-nil, sets NODE_MODE env var per replica.
	// Index i maps to replica i; if shorter than Replicas, remaining default to "alpha".
	NodeModes []string
	// ExtraEnvVars, if non-empty, is appended to the env block in the Deployment.
	// Each entry should be a fully-formed YAML env var block (name + value lines).
	ExtraEnvVars string
	// AcmePort, if non-zero, adds a containerPort and a LoadBalancer Service for
	// ACME HTTP-01 challenge traffic. The service maps external port 80 to AcmePort
	// inside the container (allows non-root binding on port >1024).
	AcmePort int32
	// ExtraPort, if non-zero, adds an additional named port to the ClusterIP Service.
	// ExtraPortName is the port name used in the Service spec (required when ExtraPort != 0).
	ExtraPort     int32
	ExtraPortName string
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
func archTolerationLines() string {
	return `      tolerations:
      - key: kubernetes.io/arch
        operator: Equal
        value: arm64
        effect: NoSchedule
`
}

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
	return fmt.Sprintf("        - name: JDK_JAVA_OPTIONS\n          value: \"-Xmx%s -Xms%s -XX:+UseContainerSupport\"\n", svc.JavaHeap, svc.JavaHeap)
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

func withKernelServiceAuth(svc serviceSpec) serviceSpec {
	if svc.ServiceAccountName == "" {
		svc.ServiceAccountName = svc.Name
	}
	if svc.AutomountServiceAccountToken == nil {
		svc.AutomountServiceAccountToken = falsePtr()
	}
	svc.ServiceAccountTokenProjections = append(svc.ServiceAccountTokenProjections, &serviceAccountTokenProjection{
		VolumeName:        "kernel-token",
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
		KernelNodes:     t.KernelNodes,
		SentinelNodes:   t.SentinelNodes,
		EventbusNodes:   t.EventbusNodes,
		GatewayNodes:    t.GatewayNodes,
		OauthNodes:      t.OauthNodes,
		OpaNodes:        t.OpaNodes,
		TimedbNodes:     t.TimedbNodes,
		GraphdbNodes:    t.GraphdbNodes,
		ArgusNodes:      t.ArgusNodes,
		StratumNodes:    t.StratumNodes,
		SamlNodes:       t.SamlNodes,
		KmakerNodes:     t.KmakerNodes,
		EchoNodes:       t.EchoNodes,
		// Carry the app's services. A field-by-field clone silently drops
		// anything added later — which is the failure mode a fixed-field message
		// invites, and it dropped app_services on the first attempt.
		AppServices: t.AppServices,
	}
}

// timedbNodeModes returns per-replica NODE_MODE values: all nodes run as "combined"
// (ClickHouse Keeper + Data) which is appropriate for a dev cluster.
func timedbNodeModes(n int32) []string {
	modes := make([]string, n)
	for i := range modes {
		modes[i] = "combined"
	}
	return modes
}

// graphdbNodeModes returns per-replica NODE_MODE values: first node is "zero",
// the rest are "alpha".
func graphdbNodeModes(n int32) []string {
	modes := make([]string, n)
	for i := range modes {
		if i == 0 {
			modes[i] = "zero"
		} else {
			modes[i] = "alpha"
		}
	}
	return modes
}

func (g *Generator) imageRef(svc string) string {
	return g.imageRefProj(svc, svc)
}

// imageRefProj builds an image reference where the GitLab registry project name
// differs from the service name.
func (g *Generator) imageRefProj(proj, svc string) string {
	// Local-images mode (SCHEMATIC_LOCAL_IMAGES): reference locally-built images
	// (schematic/<proj>:local) instead of the GitLab registry. Used for local
	// docker-desktop clusters where k8s shares the docker daemon, so
	// IfNotPresent resolves the local tag without a registry.
	if v := os.Getenv("SCHEMATIC_LOCAL_IMAGES"); v != "" {
		// A value containing a host (slash/colon) is a registry prefix, e.g.
		// SCHEMATIC_LOCAL_IMAGES=localhost:5001 -> localhost:5001/schematic/<proj>:local.
		// Any other non-empty value uses the bare local tag (shared image store).
		if strings.ContainsAny(v, "/:") {
			return fmt.Sprintf("%s/schematic/%s:local", v, proj)
		}
		return fmt.Sprintf("schematic/%s:local", proj)
	}
	tag := g.resolveTag(proj, svc)
	return fmt.Sprintf(
		"registry.gitlab.com/secondstarcavalier/schematic/platform/services/%s/%s-%s:%s",
		proj, svc, g.series, tag,
	)
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
	//   - graphdb: wraps Dgraph; memory grows with index + cache size.
	//   - eventbus: embedded Kafka broker — heap usage scales with topic
	//     count + replication settings.
	svcs := []serviceSpec{
		{Name: "kernel", Replicas: topo.KernelNodes, Image: g.imageRef("kernel"), HasGRPC: true, HasPVC: true, Storage: "2Gi", HasHazelcast: true, ListenPort: 5000, IsStatefulSet: true, PerReplicaConfig: true,
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
			MemRequest: "1Gi", MemLimit: "2Gi", CpuRequest: "250m", JavaHeap: "1g"},
		{Name: "kmaker", Replicas: topo.KmakerNodes, Image: g.imageRef("kmaker"), HasGRPC: true, ListenPort: 8443,
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
		withKernelServiceAuth(serviceSpec{Name: "sentinel", Replicas: topo.SentinelNodes, Image: g.imageRef("sentinel"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}),
		withKernelServiceAuth(serviceSpec{Name: "eventbus", Replicas: topo.EventbusNodes, Image: g.imageRef("eventbus"), HasGRPC: true, HasPVC: true, Storage: "2Gi", ListenPort: 8443,
			MemRequest: "1Gi", MemLimit: "2Gi", CpuRequest: "250m", JavaHeap: "1g"}),
		withKernelServiceAuth(serviceSpec{Name: "gateway", Replicas: topo.GatewayNodes, Image: g.imageRef("gateway"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "100m"}),
		withKernelServiceAuth(serviceSpec{Name: "opa", Replicas: topo.OpaNodes, Image: g.imageRef("opa"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}),
		withKernelServiceAuth(serviceSpec{Name: "argus", Replicas: topo.ArgusNodes, Image: g.imageRef("argus"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}),
		withKernelServiceAuth(serviceSpec{Name: "timedb", Replicas: topo.TimedbNodes, Image: g.imageRefProj("timedb", "timedb"), HasGRPC: true, HasPVC: true, Storage: "2Gi", ListenPort: 8443, NodeModes: timedbNodeModes(topo.TimedbNodes),
			MemRequest: "256Mi", MemLimit: "1Gi", CpuRequest: "100m"}),
		withKernelServiceAuth(serviceSpec{Name: "graphdb", Replicas: topo.GraphdbNodes, Image: g.imageRef("graphdb"), HasGRPC: true, HasPVC: true, Storage: "5Gi", ListenPort: 8443, NodeModes: graphdbNodeModes(topo.GraphdbNodes),
			MemRequest: "256Mi", MemLimit: "1Gi", CpuRequest: "100m", GoMemLimit: "800MiB"}),
		// stratum is a StatefulSet with a headless Service, like kernel, because
		// its event store is a Hazelcast cluster: the members need stable
		// per-pod DNS to find each other. As a Deployment behind a plain
		// ClusterIP they could not — the pods had no stable identity and no
		// discoverable peer addresses — so several replicas would each form
		// their OWN cluster of size one. No partition sharing, and every pod
		// believing it was a cold boot, replaying the whole event log.
		withKernelServiceAuth(serviceSpec{Name: "stratum", Replicas: topo.StratumNodes, Image: g.imageRef("stratum"), HasGRPC: true, HasPVC: true, Storage: "5Gi", HasHazelcast: true, HasWorkingSetHazelcast: true, ListenPort: 8443, IsStatefulSet: true,
			ExtraEnvVars: stratumSeedMembersEnv(topo.StratumNodes),
			MemRequest:   "1Gi", MemLimit: "2Gi", CpuRequest: "250m", JavaHeap: "1g"}),
	}

	// OAuth (OAuth2/OIDC broker) — always enabled.
	svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "oauth", Replicas: topo.OauthNodes, Image: g.imageRef("oauth"), HasGRPC: true, ListenPort: 8443,
		MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}))

	// Echo sample service — disabled by default. Stateless, no S2S deps, so it
	// needs nothing beyond the standard kernel service auth every service gets.
	if opt.GetEcho() && topo.GetEchoNodes() > 0 {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "echo", Replicas: topo.EchoNodes, Image: g.imageRef("echo"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "64Mi", MemLimit: "256Mi", CpuRequest: "25m"}))
	}

	// The application's own services.
	//
	// Modeled on echo, which was the platform's only example of "a service that
	// just needs to run and authenticate to kernel": stateless, gRPC on 8443, no
	// S2S dependencies beyond the standard kernel service auth every service
	// gets. An app service that needs more than this (a PVC, extra ports, a
	// Hazelcast member) is not expressible yet and should be added deliberately
	// rather than guessed at.
	for _, name := range appServiceNames(topo) {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{
			Name: name, Replicas: topo.GetAppServices()[name], Image: g.imageRef(name),
			HasGRPC: true, ListenPort: 8443,
			MemRequest: "64Mi", MemLimit: "256Mi", CpuRequest: "25m"}))
	}

	// SAML bridge — disabled by default.
	if opt.GetSaml() {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{Name: "saml", Replicas: topo.SamlNodes, Image: g.imageRef("saml"), HasGRPC: true, ListenPort: 8443,
			MemRequest: "128Mi", MemLimit: "512Mi", CpuRequest: "50m"}))
	}

	// Let's Encrypt ACME agent — one replica, only when tls.letsencrypt = true.
	if cfg.GetTls().GetLetsencrypt() {
		svcs = append(svcs, withKernelServiceAuth(serviceSpec{
			Name:             "letsencrypt",
			Replicas:         1,
			Image:            g.imageRef("letsencrypt"),
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
  config: ""
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
		"Namespace":        ns,
		"Services":         svcs,
		"Dev":              cfg.Dev || os.Getenv("SCHEMATIC_LOCAL_IMAGES") != "",
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
app.kubernetes.io/managed-by: schematic-operator
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
Init container that waits for sentinel0:8443.
*/}}
{{- define "schematic.waitForSentinel" -}}
- name: wait-for-sentinel
  image: busybox:latest
  command: ["sh", "-c", "until nc -z sentinel0 8443; do sleep 2; done"]
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

func (g *Generator) addService(charts *Charts, cfg *protoOperator.Config, svc serviceSpec) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = "schematic"
	}
	if svc.AutomountServiceAccountToken == nil {
		svc.AutomountServiceAccountToken = falsePtr()
	}

	if svc.ServiceAccountName != "" {
		charts.Files[fmt.Sprintf("templates/%s/serviceaccount.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
`, svc.ServiceAccountName))
	}
	if svc.TokenReviewRBAC && svc.ServiceAccountName != "" {
		charts.Files[fmt.Sprintf("templates/%s/tokenreview-rbac.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %s-tokenreview
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
  name: %s-tokenreview
  labels:
    {{- include "schematic.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %s-tokenreview
subjects:
- kind: ServiceAccount
  name: %s
  namespace: {{ .Values.global.namespace }}
`, svc.Name, svc.Name, svc.Name, svc.ServiceAccountName))
	}
	if svc.PodReadRBAC && svc.ServiceAccountName != "" {
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

	// ConfigMap
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
kind: ConfigMap
metadata:
  name: %s-config
  namespace: {{ .Values.global.namespace }}
data:
  config.yaml: {{ .Values.%s.config | quote }}
`, name, configValueKey)
		if svc.HasServiceConfig {
			cmData += fmt.Sprintf("  service.yaml: {{ .Values.%s.serviceConfig | quote }}\n", configValueKey)
		}
		charts.Files[fmt.Sprintf("templates/%s/configmap-%d.yaml", svc.Name, i)] = []byte(cmData)

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
      storage: {{ .Values.%s.storage }}
`, name, svc.Name))
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
			charts.Files[fmt.Sprintf("templates/%s/service-%d-lb.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s-lb
  namespace: {{ .Values.global.namespace }}
spec:
  type: LoadBalancer
  selector:
    app: %s
  ports:
  - name: https
    port: %d
    targetPort: %d
`, name, name, svc.Port, listenPort))
		}

		// LoadBalancer for ACME HTTP-01 challenge. External port 80, targetPort = AcmePort.
		// The container binds AcmePort (e.g. 8080) to avoid requiring NET_BIND_SERVICE.
		if svc.AcmePort != 0 {
			charts.Files[fmt.Sprintf("templates/%s/service-%d-acme.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s-acme
  namespace: {{ .Values.global.namespace }}
spec:
  type: LoadBalancer
  selector:
    app: %s
  ports:
  - name: http
    port: 80
    targetPort: %d
`, name, name, svc.AcmePort))
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

	if svc.IsStatefulSet {
		// Generate a StatefulSet with an init container that selects the
		// per-ordinal ConfigMap based on the pod's hostname (kernel-0 → 0).
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
  podManagementPolicy: Parallel
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
        {{- include "schematic.labels" . | nindent 8 }}
`, svc.Name, svc.Name, svc.Replicas, svc.Name, svc.Name))
		ssBuf.WriteString(`      annotations:
`)
		for i := range int(svc.Replicas) {
			ssBuf.WriteString(fmt.Sprintf(`        checksum/config-%d: {{ .Values.%s.config | sha256sum | quote }}
`, i, configValueKey(svc, i)))
		}
		ssBuf.WriteString(`    spec:
      {{- include "schematic.imagePullSecrets" . | nindent 6 }}
`)
		ssBuf.WriteString(podServiceAccountLines(svc))
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
		ssBuf.WriteString(fmt.Sprintf(`      containers:
      - name: %s
        image: {{ .Values.%s.image }}
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
`, svc.Name, svc.Name, bootstrapEnv))
		if jo := javaOptsEnv(svc); jo != "" {
			ssBuf.WriteString(jo)
		}
		if gm := goMemLimitEnv(svc); gm != "" {
			ssBuf.WriteString(gm)
		}
		if svc.ExtraEnvVars != "" {
			ssBuf.WriteString(svc.ExtraEnvVars)
		}
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
        configMap:
          name: %s-config
`, i, name))
		}
		if svc.HasPVC {
			ssBuf.WriteString(fmt.Sprintf(`  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      resources:
        requests:
          storage: {{ .Values.%s.storage }}
`, svc.Name))
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
        {{- include "schematic.labels" . | nindent 8 }}
      annotations:
        checksum/config: {{ .Values.%s.config | sha256sum | quote }}
`, name, strategy, name, name, svc.Name))
			if svc.HasServiceConfig {
				deployBuf.WriteString(fmt.Sprintf(`        checksum/service-config: {{ .Values.%s.serviceConfig | sha256sum | quote }}
`, svc.Name))
			}
			deployBuf.WriteString(fmt.Sprintf(`    spec:
      {{- include "schematic.imagePullSecrets" . | nindent 6 }}
%s
      hostname: %s
%s%s      containers:
      - name: %s
        image: {{ .Values.%s.image }}
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
`, podServiceAccountLines(svc), name, archTolerationLines(), pvcFsGroupLines(svc), name, svc.Name))
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
			if len(svc.NodeModes) > 0 {
				nodeMode := "alpha"
				if i < len(svc.NodeModes) {
					nodeMode = svc.NodeModes[i]
				}
				deployBuf.WriteString(fmt.Sprintf(`        - name: NODE_MODE
          value: %s
`, nodeMode))
				// GraphDB Alpha nodes need the Zero address for Raft cluster formation.
				// Zero is always the first replica (graphdb0).
				if svc.Name == "graphdb" && nodeMode == "alpha" {
					deployBuf.WriteString(fmt.Sprintf(`        - name: GRAPHDB_ZERO_ADDR
          value: %s0:5080
`, svc.Name))
				}
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
        configMap:
`)
			deployBuf.WriteString(fmt.Sprintf(`          name: %s-config
`, name))
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
