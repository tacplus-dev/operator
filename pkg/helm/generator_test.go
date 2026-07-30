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
			MsggatewayNodes: 1,
			OpaNodes:        1,
			TimedbNodes:     1,
			GraphdbNodes:    1,
			ArgusNodes:      1,
			StratumNodes:    1,
			SamlNodes:       1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Anthropic: true,
			Openai:    true,
			Mcp:       true,
			Saml:      true,
			Oauth:     true,
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
		"anthropic",
		"openai",
		"mcp",
		"msggateway",
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
