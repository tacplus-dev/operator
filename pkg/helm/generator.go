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

	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/operator/tacplus/types/utils/operator"
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
	svcs := []serviceSpec{
		{Name:"kernel", Replicas:topo.KernelNodes, Image:g.imageRef("kernel"), HasGRPC:true, HasPVC:true, Storage:"2Gi", HasHazelcast:true, ListenPort:5000},
		{Name:"sentinel", Replicas:topo.SentinelNodes, Image:g.imageRef("sentinel"), HasGRPC:true, ListenPort:8443},
		{Name:"eventbus", Replicas:topo.EventbusNodes, Image:g.imageRef("eventbus"), HasGRPC:true, HasPVC:true, Storage:"2Gi", ListenPort:8443},
		{Name:"gateway", Replicas:topo.GatewayNodes, Image:g.imageRef("gateway"), HasGRPC:true, ListenPort:8443},
		{Name:"opa", Replicas:topo.OpaNodes, Image:g.imageRef("opa"), HasGRPC:true, ListenPort:8443},
		{Name:"argus", Replicas:topo.ArgusNodes, Image:g.imageRef("argus"), HasGRPC:true, ListenPort:8443},
		{Name:"timedb", Replicas:topo.TimedbNodes, Image:g.imageRefProj("timedb", "timedb"), HasGRPC:true, HasPVC:true, Storage:"2Gi", ListenPort:8443, NodeModes:timedbNodeModes(topo.TimedbNodes)},
		{Name:"graphdb", Replicas:topo.GraphdbNodes, Image:g.imageRef("graphdb"), HasGRPC:true, HasPVC:true, Storage:"5Gi", ListenPort:8443, NodeModes: graphdbNodeModes(topo.GraphdbNodes)},
		{Name:"stratum", Replicas:topo.StratumNodes, Image:g.imageRef("stratum"), HasGRPC:true, HasPVC:true, Storage:"5Gi", ListenPort:8443},
	}

	// OAuth (OAuth2/OIDC broker) — enabled by default.
	if opt.GetOauth() {
		svcs = append(svcs, serviceSpec{Name:"oauth", Replicas:topo.OauthNodes, Image:g.imageRef("oauth"), HasGRPC:true, ListenPort:8443})
	}

	// SAML bridge — disabled by default.
	if opt.GetSaml() {
		svcs = append(svcs, serviceSpec{Name:"saml", Replicas:topo.SamlNodes, Image:g.imageRef("saml"), HasGRPC:true, ListenPort:8443})
	}

	// Keycloak IDP — enabled when dev=true or optional_services.keycloak=true.
	// ExtraPort 9443 exposes Keycloak's native HTTPS port alongside the gRPC wrapper (8443).
	// KC_HOSTNAME is set to the full HTTPS URL so Keycloak builds correct issuer URLs in
	// OIDC discovery responses. KC_PROXY_HEADERS=xforwarded trusts X-Forwarded-* from gateway.
	if opt.GetKeycloak() || cfg.Dev {
		kcFqdn := cfg.Fqdn
		kcEnv := fmt.Sprintf(`        - name: KC_PROXY_HEADERS
          value: xforwarded
        - name: KC_HOSTNAME
          value: https://%s
        - name: KC_HOSTNAME_BACKCHANNEL_DYNAMIC
          value: "false"
`, kcFqdn)
		svcs = append(svcs, serviceSpec{Name:"keycloak", Replicas:1, Image:g.imageRef("keycloak"), HasGRPC:true, HasPVC:true, Storage:"1Gi", ListenPort:8443, ExtraPort:9443, ExtraPortName:"https", ExtraEnvVars:kcEnv})
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
initialDelaySeconds: 30
periodSeconds: 15
failureThreshold: 5
{{- end }}

{{/*
Standard gRPC readiness/liveness probe on port 8443 (Rust/Tacplus services).
*/}}
{{- define "cavalier.grpcProbe8443" -}}
tcpSocket:
  port: 8443
initialDelaySeconds: 30
periodSeconds: 15
failureThreshold: 5
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
		name := fmt.Sprintf("%s%d", svc.Name, i)
		// Kernel uses per-replica value keys (kernel0, kernel1, …) because each
		// replica has a distinct TLS certificate. All other services share one
		// config across replicas and use the service name as the value key.
		configValueKey := svc.Name
		if svc.Name == "kernel" {
			configValueKey = name
		}
		charts.Files[fmt.Sprintf("templates/%s/configmap-%d.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s-config
  namespace: {{ .Values.global.namespace }}
data:
  config.yaml: {{ .Values.%s.config | quote }}
`, name, configValueKey))

		// PVC if needed
		if svc.HasPVC {
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
		// Internal ClusterIP service for gRPC service-to-service communication.
		charts.Files[fmt.Sprintf("templates/%s/service-%d.yaml", svc.Name, i)] = []byte(fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: {{ .Values.global.namespace }}
spec:
  selector:
    app: %s
  ports:
  - name: grpc
    port: %d
    targetPort: %d
%s%s`, name, name, listenPort, listenPort, hazelcastPort, extraPort))

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

	// Deployment template (helm-templated for node iteration)
	var buf bytes.Buffer
	type tmplData struct {
		Name     string
		Replicas int32
		HasPVC   bool
	}
	_ = deploymentTmpl // used below via direct string generation for clarity
	listenPort := svc.ListenPort
	if listenPort == 0 {
		listenPort = 5000
	}
	probeHelper := "cavalier.grpcProbe5000"
	if listenPort == 8443 {
		probeHelper = "cavalier.grpcProbe8443"
	}
	// Generate individual deployments — one per replica, named <service><index>
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
`, name, name, name, name, name, svc.Name))
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
		if !svc.SkipProbes {
			deployBuf.WriteString(fmt.Sprintf(`        readinessProbe:
          {{- include "%s" . | nindent 10 }}
        livenessProbe:
          {{- include "%s" . | nindent 10 }}
`, probeHelper, probeHelper))
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
	}
	_ = buf
	charts.Files[fmt.Sprintf("templates/%s/deployment.yaml", svc.Name)] = deployBuf.Bytes()

	return nil
}
