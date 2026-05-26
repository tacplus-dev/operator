// Package helm generates Helm chart manifests for all cavalier services.
//
// The Generator produces a complete chart — Chart.yaml, values.yaml, and all
// service templates — either entirely in memory or written to an output directory.
//
// Chart structure produced:
//
//	cavalier/
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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/tacplus/types/services/operator"
)

// Charts is the in-memory representation of a generated Helm chart.
// Keys are relative paths within the chart directory (e.g. "templates/kernel/deployment.yaml").
type Charts struct {
	Files map[string][]byte
}

// TagResolver resolves the image tag for a service registry image.
// The proj is the GitLab registry project name (e.g. "kernel", "anthropic-proxy"),
// and svc is the image name within that project (e.g. "kernel", "anthropic").
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

// Generate produces the full cavalier Helm chart for the given config.
// nodeCount is the number of schedulable Kubernetes nodes; it is used to
// set gateway_nodes when topology.gateway_nodes == 0.
func (g *Generator) Generate(cfg *protoOperator.OperatorConfig, nodeCount int) (*Charts, error) {
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

// WriteToDisk writes charts to outputDir/cavalier/.
func (g *Generator) WriteToDisk(charts *Charts, outputDir string) error {
	base := filepath.Join(outputDir, "cavalier")
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

// --- internal types ---

type serviceSpec struct {
	Name           string
	Replicas       int32
	Image          string
	HasGRPC        bool
	HasPVC         bool
	Storage        string
	Port           int32
	WebAppHostPath string
	HasHazelcast   bool
	ListenPort     int32  // gRPC listen port: 5000 for Java (kernel), 8443 for Rust/Tacplus
	// NodeModes, if non-nil, sets NODE_MODE env var per replica.
	// Index i maps to replica i; if shorter than Replicas, remaining default to "alpha".
	NodeModes      []string
	// ExtraEnvVars, if non-empty, is appended to the env block in the Deployment.
	// Each entry should be a fully-formed YAML env var block (name + value lines).
	ExtraEnvVars   string
	// AcmePort, if non-zero, adds a containerPort and a LoadBalancer Service for
	// ACME HTTP-01 challenge traffic. The service maps external port 80 to AcmePort
	// inside the container (allows non-root binding on port >1024).
	AcmePort       int32
	// ExtraPort, if non-zero, adds an additional named port to the ClusterIP Service.
	// ExtraPortName is the port name used in the Service spec (required when ExtraPort != 0).
	ExtraPort      int32
	ExtraPortName  string
	// SkipProbes, if true, omits the readiness and liveness probes from the Deployment.
	// Use for services that do not expose a health-check endpoint.
	SkipProbes     bool
	// IsStatefulSet, if true, generates a StatefulSet with headless Service and
	// volumeClaimTemplates instead of per-replica Deployments with separate PVCs.
	// Used for kernel where stable pod identity and PVC lifecycle are required.
	IsStatefulSet  bool
	// Resource limits/requests. Every service should have these set —
	// without them, runtimes (JVM, Go, Rust async runtimes) make assumptions
	// based on host RAM that don't hold inside a container, and you end up
	// over-committing the node. Sizing each service properly is the
	// developer/operator's responsibility regardless of language.
	MemRequest     string
	MemLimit       string
	CpuRequest     string
	CpuLimit       string
	// JavaHeap, if non-empty, emits `JDK_JAVA_OPTIONS=-Xmx<heap> -Xms<heap>`.
	// Tells the JVM what max heap to use. Independent of MemLimit; should be
	// set lower than MemLimit to leave room for non-heap memory (metaspace,
	// direct buffers, code cache, threads).
	JavaHeap       string
	// GoMemLimit, if non-empty, sets the GOMEMLIMIT env var (Go 1.19+) — Go's
	// soft memory target. Set lower than MemLimit (commonly ~80%) so Go's GC
	// triggers before the kernel OOM-killer fires. Same role as JavaHeap for
	// the Go runtime.
	GoMemLimit     string
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

func effectiveTopology(cfg *protoOperator.OperatorConfig, nodeCount int) *protoOperator.Topology {
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
		OauthNodes:  t.OauthNodes,
		MsggatewayNodes: t.MsggatewayNodes,
		OpaNodes:      t.OpaNodes,
		TimedbNodes:     t.TimedbNodes,
		GraphdbNodes:    t.GraphdbNodes,
		ColddbNodes:     t.ColddbNodes,
		ArgusNodes:      t.ArgusNodes,
		StratumNodes:    t.StratumNodes,
		SamlNodes:       t.SamlNodes,
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

// eventbusEnvVars returns extra env vars for EventBus pods.
// Sets PROTO_CONFIG_NODE_ROLE so the embedded Kafka broker initializes.
func eventbusEnvVars(n int32) string {
	// All nodes are combined (broker + controller) for simplicity.
	// Production would split broker/controller roles.
	return `        - name: PROTO_CONFIG_NODE_ROLE
          value: KAFKA_ROLE_COMBINED
`
}

func (g *Generator) imageRef(svc string) string {
	return g.imageRefProj(svc, svc)
}

// imageRefProj builds an image reference where the GitLab registry project name
// differs from the service name (e.g. "anthropic-proxy" project, "anthropic" image).
func (g *Generator) imageRefProj(proj, svc string) string {
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

func (g *Generator) enabledServices(cfg *protoOperator.OperatorConfig, topo *protoOperator.Topology) []serviceSpec {
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
		{Name:"kernel", Replicas:topo.KernelNodes, Image:g.imageRef("kernel"), HasGRPC:true, HasPVC:true, Storage:"2Gi", HasHazelcast:true, ListenPort:5000, IsStatefulSet:true,
			MemRequest:"1Gi", MemLimit:"2Gi", CpuRequest:"250m", JavaHeap:"1g"},
		{Name:"sentinel", Replicas:topo.SentinelNodes, Image:g.imageRef("sentinel"), HasGRPC:true, ListenPort:8443,
			MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"50m"},
		{Name:"eventbus", Replicas:topo.EventbusNodes, Image:g.imageRef("eventbus"), HasGRPC:true, HasPVC:true, Storage:"2Gi", ListenPort:8443, ExtraEnvVars: eventbusEnvVars(topo.EventbusNodes),
			MemRequest:"1Gi", MemLimit:"2Gi", CpuRequest:"250m", JavaHeap:"1g"},
		{Name:"gateway", Replicas:topo.GatewayNodes, Image:g.imageRef("gateway"), HasGRPC:true, ListenPort:8443,
			MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"100m"},
		{Name:"opa", Replicas:topo.OpaNodes, Image:g.imageRef("opa"), HasGRPC:true, ListenPort:8443,
			MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"50m"},
		{Name:"argus", Replicas:topo.ArgusNodes, Image:g.imageRef("argus"), HasGRPC:true, ListenPort:8443,
			MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"50m"},
		{Name:"timedb", Replicas:topo.TimedbNodes, Image:g.imageRefProj("timedb", "timedb"), HasGRPC:true, HasPVC:true, Storage:"2Gi", ListenPort:8443, NodeModes:timedbNodeModes(topo.TimedbNodes),
			MemRequest:"256Mi", MemLimit:"1Gi", CpuRequest:"100m"},
		{Name:"graphdb", Replicas:topo.GraphdbNodes, Image:g.imageRef("graphdb"), HasGRPC:true, HasPVC:true, Storage:"5Gi", ListenPort:8443, NodeModes: graphdbNodeModes(topo.GraphdbNodes),
			MemRequest:"256Mi", MemLimit:"1Gi", CpuRequest:"100m", GoMemLimit:"800MiB"},
		{Name:"stratum", Replicas:topo.StratumNodes, Image:g.imageRef("stratum"), HasGRPC:true, HasPVC:true, Storage:"5Gi", ListenPort:8443,
			MemRequest:"1Gi", MemLimit:"2Gi", CpuRequest:"250m", JavaHeap:"1g"},
	}

	// OAuth (OAuth2/OIDC broker) — always enabled.
	svcs = append(svcs, serviceSpec{Name:"oauth", Replicas:topo.OauthNodes, Image:g.imageRef("oauth"), HasGRPC:true, ListenPort:8443,
		MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"50m"})

	// SAML bridge — disabled by default.
	if opt.GetSaml() {
		svcs = append(svcs, serviceSpec{Name:"saml", Replicas:topo.SamlNodes, Image:g.imageRef("saml"), HasGRPC:true, ListenPort:8443,
			MemRequest:"128Mi", MemLimit:"512Mi", CpuRequest:"50m"})
	}

	// Let's Encrypt ACME agent — one replica, only when tls.letsencrypt = true.
	if cfg.GetTls().GetLetsencrypt() {
		svcs = append(svcs, serviceSpec{
			Name:       "letsencrypt",
			Replicas:   1,
			Image:      g.imageRef("letsencrypt"),
			HasGRPC:    true,
			ListenPort: 8443,
			AcmePort:   8080,
			SkipProbes: true,
			ExtraEnvVars: "        - name: SERVICE_CONFIG\n          value: /data/bootstrap.yaml\n",
		})
	}

	// Proxy services — one replica per gateway node when enabled.
	if opt.GetAnthropic() {
		svcs = append(svcs, serviceSpec{Name: "anthropic", Replicas: topo.GatewayNodes, Image: g.imageRefProj("anthropic-proxy", "anthropic"), HasGRPC: true, ListenPort: 8443})
	}
	if opt.GetOpenai() {
		svcs = append(svcs, serviceSpec{Name:"openai", Replicas:topo.GatewayNodes, Image:g.imageRefProj("openai-proxy", "openai"), HasGRPC:true, ListenPort:8443})
	}

	// MCP server — one replica when enabled.
	if opt.GetMcp() {
		svcs = append(svcs, serviceSpec{Name:"mcp", Replicas:1, Image:g.imageRef("mcp"), HasGRPC:true, ListenPort:8443})
	}

	// On hold — only deployed when explicitly set above zero.
	if topo.ColddbNodes > 0 {
		svcs = append(svcs, serviceSpec{Name:"colddb", Replicas:topo.ColddbNodes, Image:g.imageRef("colddb"), HasGRPC:true, HasPVC:true, Storage:"10Gi", ListenPort:8443})
	}
	if topo.MsggatewayNodes > 0 {
		svcs = append(svcs, serviceSpec{Name:"msggateway", Replicas:topo.MsggatewayNodes, Image:g.imageRef("msggateway"), HasGRPC:true, ListenPort:8443})
	}

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
func (g *Generator) ExpectedDeploymentNames(cfg *protoOperator.OperatorConfig, nodeCount int) map[string]bool {
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

func (g *Generator) addChartYAML(charts *Charts, cfg *protoOperator.OperatorConfig) error {
	tag := g.series
	content := fmt.Sprintf(`apiVersion: v2
name: cavalier
description: Cavalier control plane — managed by cavalier-operator
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
`))

func (g *Generator) addValuesYAML(charts *Charts, cfg *protoOperator.OperatorConfig, topo *protoOperator.Topology) error {
	svcs := g.enabledServices(cfg, topo)
	port := cfg.Port
	if port == 0 {
		port = 443
	}
	ns := cfg.Namespace
	if ns == "" {
		ns = "cavalier"
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

	var buf bytes.Buffer
	if err := valuesTmpl.Execute(&buf, map[string]any{
		"Namespace": ns,
		"Services":  svcs,
		"Dev":       cfg.Dev,
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
{{- define "cavalier.labels" -}}
app.kubernetes.io/managed-by: cavalier-operator
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Init container that waits for kernel0:5000.
*/}}
{{- define "cavalier.waitForKernel" -}}
- name: wait-for-kernel
  image: busybox:latest
  command: ["sh", "-c", "until nc -z kernel0 5000; do sleep 2; done"]
{{- end }}

{{/*
Init container that waits for sentinel0:8443.
*/}}
{{- define "cavalier.waitForSentinel" -}}
- name: wait-for-sentinel
  image: busybox:latest
  command: ["sh", "-c", "until nc -z sentinel0 8443; do sleep 2; done"]
{{- end }}

{{/*
Standard gRPC readiness/liveness probe on port 5000 (kernel/Java).
*/}}
{{- define "cavalier.grpcProbe5000" -}}
tcpSocket:
  port: 5000
initialDelaySeconds: 10
periodSeconds: 10
failureThreshold: 3
{{- end }}

{{- define "cavalier.startupProbe5000" -}}
tcpSocket:
  port: 5000
periodSeconds: 5
failureThreshold: 60
{{- end }}

{{/*
Standard gRPC readiness/liveness probe on port 8443 (Rust/Tacplus services).
*/}}
{{- define "cavalier.grpcProbe8443" -}}
tcpSocket:
  port: 8443
initialDelaySeconds: 10
periodSeconds: 10
failureThreshold: 3
{{- end }}

{{/*
Startup probe for services that depend on kernel. Gives up to 5 minutes
for kernel to come up and the service to complete init before k8s kills it.
Once the startup probe passes, the liveness probe takes over.
*/}}
{{- define "cavalier.startupProbe8443" -}}
tcpSocket:
  port: 8443
periodSeconds: 5
failureThreshold: 60
{{- end }}

{{/*
imagePullSecrets block — empty when no pull secret is configured.
*/}}
{{- define "cavalier.imagePullSecrets" -}}
{{- if .Values.global.imagePullSecret -}}
imagePullSecrets:
- name: {{ .Values.global.imagePullSecret }}
{{- end }}
{{- end }}
`)
	return nil
}

// addRegistrySecret emits templates/registry-secret.yaml when gitlab_registry_token is set.
// The secret provides pull credentials for registry.gitlab.com in the cavalier namespace.
func (g *Generator) addRegistrySecret(charts *Charts, cfg *protoOperator.OperatorConfig) error {
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

func (g *Generator) addNamespace(charts *Charts, cfg *protoOperator.OperatorConfig) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = "cavalier"
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
    {{ "{{- include \"cavalier.labels\" . | nindent 4 }}" }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ $.Name }}{{ $i }}
  template:
    metadata:
      labels:
        app: {{ $.Name }}{{ $i }}
        {{ "{{- include \"cavalier.labels\" . | nindent 8 }}" }}
    spec:
      {{ "{{- include \"cavalier.imagePullSecrets\" . | nindent 6 }}" }}
      hostname: {{ $.Name }}{{ $i }}
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
          {{ "{{- include \"cavalier.startupProbe5000\" . | nindent 10 }}" }}
        readinessProbe:
          {{ "{{- include \"cavalier.grpcProbe5000\" . | nindent 10 }}" }}
        livenessProbe:
          {{ "{{- include \"cavalier.grpcProbe5000\" . | nindent 10 }}" }}
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

func (g *Generator) addService(charts *Charts, cfg *protoOperator.OperatorConfig, svc serviceSpec) error {
	ns := cfg.Namespace
	if ns == "" {
		ns = "cavalier"
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
		// Kernel uses per-replica value keys (kernel0, kernel1, …) because each
		// replica has a distinct TLS certificate. All other services share one
		// config across replicas and use the service name as the value key.
		configValueKey := svc.Name
		if svc.IsStatefulSet {
			configValueKey = fmt.Sprintf("%s%d", svc.Name, i)
		}
		cmData := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s-config
  namespace: {{ .Values.global.namespace }}
data:
  config.yaml: {{ .Values.%s.config | quote }}
`, name, configValueKey)
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
    port: 5001
    targetPort: 5001
`)
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
		charts.Files[fmt.Sprintf("templates/%s/service-headless.yaml", svc.Name)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
spec:
  clusterIP: None
  selector:
    app: %s
  ports:
  - name: grpc
    port: %d
    targetPort: %d
  - name: hazelcast
    port: 5001
    targetPort: 5001
`, svc.Name, svc.Name, listenPort, listenPort))
	}

	listenPort := svc.ListenPort
	if listenPort == 0 {
		listenPort = 5000
	}
	probeHelper := "cavalier.grpcProbe5000"
	startupProbeHelper := "cavalier.startupProbe5000"
	if listenPort == 8443 {
		probeHelper = "cavalier.grpcProbe8443"
		startupProbeHelper = "cavalier.startupProbe8443"
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
    {{- include "cavalier.labels" . | nindent 4 }}
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
        {{- include "cavalier.labels" . | nindent 8 }}
    spec:
      {{- include "cavalier.imagePullSecrets" . | nindent 6 }}
      initContainers:
      - name: config-select
        image: busybox:1.36
        command: ["sh", "-c", "ORDINAL=${HOSTNAME##*-}; cp /configs/config-${ORDINAL}/config.yaml /bootstrap/bootstrap.yaml"]
        volumeMounts:
        - name: bootstrap
          mountPath: /bootstrap
`, svc.Name, svc.Name, svc.Replicas, svc.Name, svc.Name))
		// Mount each per-ordinal ConfigMap in the init container.
		for i := range int(svc.Replicas) {
			name := fmt.Sprintf("%s-%d", svc.Name, i)
			ssBuf.WriteString(fmt.Sprintf(`        - name: config-%d
          mountPath: /configs/config-%d
`, i, i))
			_ = name
		}
		ssBuf.WriteString(fmt.Sprintf(`      containers:
      - name: %s
        image: {{ .Values.%s.image }}
        imagePullPolicy: {{ .Values.global.imagePullPolicy }}
        env:
        - name: BOOTSTRAP_CONFIG
          value: /bootstrap/bootstrap.yaml
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
`, svc.Name, svc.Name))
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
        - containerPort: 5001
          name: hazelcast
        volumeMounts:
        - name: bootstrap
          mountPath: /bootstrap
`, listenPort))
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
			deployBuf.WriteString(fmt.Sprintf(`---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
  labels:
    {{- include "cavalier.labels" . | nindent 4 }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: %s
  template:
    metadata:
      labels:
        app: %s
        {{- include "cavalier.labels" . | nindent 8 }}
    spec:
      {{- include "cavalier.imagePullSecrets" . | nindent 6 }}
      hostname: %s
      containers:
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
`, name, name, name, name, name, svc.Name))
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
			if svc.HasPVC {
				deployBuf.WriteString(`        - name: data
          mountPath: /app/data
`)
			}
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
