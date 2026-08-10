package helm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	protoKmaker "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/secondstarcavalier/schematic/platform/services/operator/model/types/services/operator"
)

func TestGenerateKernelProjectedTokensAndTokenReviewRBAC(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "cavalier",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
		Kmaker: &protoKmaker.Config{TokenAudience: "custom-kmaker"},
	})

	statefulSet := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	for _, want := range []string{
		"serviceAccountName: kernel",
		"automountServiceAccountToken: false",
		"- name: KMAKER_K8S_TOKEN_FILE\n          value: /var/run/secrets/cavalier/kmaker/token",
		"- name: KERNEL_K8S_TOKEN_AUDIENCE\n          value: cavalier-kernel",
		"- name: kmaker-token\n          mountPath: /var/run/secrets/cavalier/kmaker\n          readOnly: true",
		"- name: kmaker-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"audience: custom-kmaker",
		"expirationSeconds: 600",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(statefulSet, want) {
			t.Fatalf("kernel StatefulSet missing %q\n--- statefulset ---\n%s", want, statefulSet)
		}
	}

	tokenReviewRBAC := chartFile(t, charts, "templates/kernel/tokenreview-rbac.yaml")
	for _, want := range []string{
		"resources: [\"tokenreviews\"]",
		"verbs: [\"create\"]",
		"name: kernel",
	} {
		if !strings.Contains(tokenReviewRBAC, want) {
			t.Fatalf("kernel TokenReview RBAC missing %q\n--- rbac ---\n%s", want, tokenReviewRBAC)
		}
	}
}

func TestGenerateKmakerProjectedKubernetesAPITokenAndRBAC(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "cavalier",
		Topology: &protoOperator.Topology{
			KmakerNodes: 1,
		},
	})

	deployment := chartFile(t, charts, "templates/kmaker/deployment.yaml")
	for _, want := range []string{
		"serviceAccountName: kmaker",
		"automountServiceAccountToken: false",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(deployment, want) {
			t.Fatalf("kmaker Deployment missing %q\n--- deployment ---\n%s", want, deployment)
		}
	}

	tokenReviewRBAC := chartFile(t, charts, "templates/kmaker/tokenreview-rbac.yaml")
	for _, want := range []string{
		"resources: [\"tokenreviews\"]",
		"verbs: [\"create\"]",
		"name: kmaker",
	} {
		if !strings.Contains(tokenReviewRBAC, want) {
			t.Fatalf("kmaker TokenReview RBAC missing %q\n--- rbac ---\n%s", want, tokenReviewRBAC)
		}
	}

	podRBAC := chartFile(t, charts, "templates/kmaker/pod-rbac.yaml")
	for _, want := range []string{
		"resources: [\"pods\"]",
		"verbs: [\"get\"]",
		"name: kmaker",
	} {
		if !strings.Contains(podRBAC, want) {
			t.Fatalf("kmaker pod RBAC missing %q\n--- rbac ---\n%s", want, podRBAC)
		}
	}
}

func TestGeneratedServiceWorkloadsUseProjectedKernelToken(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "cavalier",
		Tls:       &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			SentinelNodes:   1,
			EventbusNodes:   1,
			GatewayNodes:    1,
			OauthNodes:      1,
			EchoNodes:       1,
			OpaNodes:        1,
			TimedbNodes:     1,
			GraphdbNodes:    1,
			ArgusNodes:      1,
			StratumNodes:    1,
			SamlNodes:       1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Saml:  true,
			Oauth: true,
			Echo:  true,
		},
	})

	services := []string{
		"sentinel",
		"eventbus",
		"gateway",
		"opa",
		"argus",
		"timedb",
		"graphdb",
		"stratum",
		"oauth",
		"saml",
		"letsencrypt",
		"echo",
	}
	for _, service := range services {
		t.Run(service, func(t *testing.T) {
			deployment := chartFile(t, charts, "templates/"+service+"/deployment.yaml")
			for _, want := range []string{
				"serviceAccountName: " + service,
				"automountServiceAccountToken: false",
				"- name: KERNEL_K8S_TOKEN_FILE\n          value: /var/run/secrets/cavalier/kernel/token",
				"- name: kernel-token\n          mountPath: /var/run/secrets/cavalier/kernel\n          readOnly: true",
				"- name: kernel-token\n        projected:",
				"defaultMode: 0444",
				"path: token",
				"audience: cavalier-kernel",
				"expirationSeconds: 3600",
			} {
				if !strings.Contains(deployment, want) {
					t.Fatalf("%s Deployment missing %q\n--- deployment ---\n%s", service, want, deployment)
				}
			}
		})
	}
	for _, service := range services {
		rbac := chartFile(t, charts, "templates/"+service+"/serviceaccount.yaml")
		if !strings.Contains(rbac, "name: "+service) {
			t.Fatalf("%s ServiceAccount missing service name\n--- serviceaccount ---\n%s", service, rbac)
		}
	}
}

func TestOperatorDeploymentUsesExplicitKubernetesAPITokenProjection(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read operator Deployment template: %v", err)
	}
	deployment := string(data)
	for _, want := range []string{
		"serviceAccountName: cavalier-operator",
		"automountServiceAccountToken: false",
		"- name: kubernetes-api-token\n          mountPath: /var/run/secrets/kubernetes.io/serviceaccount\n          readOnly: true",
		"- name: kubernetes-api-token\n        projected:",
		"defaultMode: 0444",
		"path: token",
		"expirationSeconds: 3600",
		"name: kube-root-ca.crt",
		"path: ca.crt",
		"- downwardAPI:",
		"path: namespace",
		"fieldPath: metadata.namespace",
	} {
		if !strings.Contains(deployment, want) {
			t.Fatalf("operator Deployment missing %q\n--- deployment ---\n%s", want, deployment)
		}
	}
}

func TestOperatorChartRBACCanManageGeneratedServiceRBAC(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", "clusterrole.yaml"))
	if err != nil {
		t.Fatalf("read operator ClusterRole template: %v", err)
	}
	clusterRole := string(data)
	for _, want := range []string{
		"- roles",
		"- rolebindings",
		"- clusterroles",
		"- clusterrolebindings",
		"- pods",
		"verbs: [\"get\"]",
	} {
		if !strings.Contains(clusterRole, want) {
			t.Fatalf("operator ClusterRole missing %q\n--- clusterrole ---\n%s", want, clusterRole)
		}
	}
}

// TestSingleNodeKernelValuesAndTemplates verifies that a 1-node kernel topology
// produces a values.yaml with a kernel0 placeholder and templates that reference
// .Values.kernel0.config — the combination that previously caused a nil-pointer
// error during helm install dry-run.
func TestSingleNodeKernelValuesAndTemplates(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "cavalier",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
	})

	// values.yaml must contain the per-replica key kernel0 (and no kernel1).
	values := chartFile(t, charts, "values.yaml")
	if !strings.Contains(values, "kernel0:") {
		t.Fatalf("values.yaml missing kernel0 placeholder\n--- values.yaml ---\n%s", values)
	}
	if strings.Contains(values, "kernel1:") {
		t.Fatalf("values.yaml unexpectedly contains kernel1 for a 1-node topology\n--- values.yaml ---\n%s", values)
	}

	// configmap-0.yaml must reference .Values.kernel0.config (not .Values.kernel.config).
	cm := chartFile(t, charts, "templates/kernel/configmap-0.yaml")
	if !strings.Contains(cm, ".Values.kernel0.config") {
		t.Fatalf("kernel configmap-0 must reference .Values.kernel0.config\n--- configmap ---\n%s", cm)
	}

	// statefulset.yaml must checksum .Values.kernel0.config in its annotations.
	ss := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if !strings.Contains(ss, ".Values.kernel0.config") {
		t.Fatalf("kernel statefulset must reference .Values.kernel0.config\n--- statefulset ---\n%s", ss)
	}
}

func generateTestChart(t *testing.T, cfg *protoOperator.Config) *Charts {
	t.Helper()
	charts, err := New("test", "1.2", nil).Generate(cfg, 1)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return charts
}

func chartFile(t *testing.T, charts *Charts, path string) string {
	t.Helper()
	data, ok := charts.Files[path]
	if !ok {
		t.Fatalf("generated chart missing %s", path)
	}
	return string(data)
}

// TestPVCDeploymentsUseRecreateStrategy guards a deadlock: a Deployment that owns a
// ReadWriteOnce PVC must use strategy Recreate. With the default RollingUpdate, k8s
// starts the replacement pod before terminating the old one; on a single node both
// mount the same volume and the old pod still holds the data-dir lock, so the new pod
// never becomes Ready and the old one is never torn down. Observed live on eventbus,
// whose replacement pod CrashLooped on Kafka's data-dir lock for hours while the old
// pod kept serving.
func TestPVCDeploymentsUseRecreateStrategy(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, `strategy = "  strategy:\n    type: Recreate\n"`) {
		t.Error("PVC-owning Deployments must set strategy: Recreate — the RollingUpdate default deadlocks on a ReadWriteOnce volume")
	}
	if !strings.Contains(body, "if svc.HasPVC {") {
		t.Error("the Recreate strategy must be gated on svc.HasPVC")
	}
}

// TestProbesQueryGRPCHealthOnPlaintextPort guards two defects that each shipped once.
//
// (1) tcpSocket probes stay true while a service's guts are dead — a wedged eventbus
// sat 1/1 Ready through drill 7 because port 8443 still accepted connections. Only a
// grpc.health.v1 query reflects handler health.
//
// (2) The probe must target the PLAINTEXT health port 8444, never the TLS service
// ports. The kubelet's gRPC probe cannot speak TLS, so probing 5000/8443 times out on
// every request; doing that restarted all 12 services twice before it was reverted.
//
// timeoutSeconds must stay above the k8s default of 1: a health RPC the application
// must answer needs more budget than a kernel-level TCP accept, and at 1s the JVM
// services were killed 3 times in 28 minutes by spurious liveness failures.
func TestProbesQueryGRPCHealthOnPlaintextPort(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)

	for _, probe := range []string{
		"cavalier.grpcProbe5000", "cavalier.startupProbe5000",
		"cavalier.grpcProbe8443", "cavalier.startupProbe8443",
	} {
		if !strings.Contains(body, probe) {
			t.Errorf("probe helper %q is missing", probe)
		}
	}
	// No probe helper may fall back to a TCP check.
	probeRegion := body
	if i := strings.Index(probeRegion, `"cavalier.grpcProbe5000"`); i >= 0 {
		if j := strings.Index(probeRegion[i:], `"cavalier.imagePullSecrets"`); j > 0 {
			probeRegion = probeRegion[i : i+j]
		}
	}
	if strings.Contains(probeRegion, "tcpSocket") {
		t.Error("probe helpers must not use tcpSocket — a TCP accept stays true while the service is dead")
	}
	if strings.Contains(probeRegion, "grpc:\n  port: 8443") || strings.Contains(probeRegion, "grpc:\n  port: 5000") {
		t.Error("grpc probes must target the plaintext health port 8444 — the kubelet cannot probe a TLS port")
	}
	if !strings.Contains(probeRegion, "timeoutSeconds: 5") {
		t.Error("grpc probes need timeoutSeconds above the 1s default, or JVM services flap")
	}
}
