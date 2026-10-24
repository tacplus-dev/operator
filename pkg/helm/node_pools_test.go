package helm

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8syaml "sigs.k8s.io/yaml"

	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

// nodePoolsTestConfig is a fleet with a multi-replica StatefulSet (kernel), a
// multi-replica per-replica-Deployment service (opa) and one tenant-owned
// dog in one tenant.
func nodePoolsTestConfig() *protoOperator.Config {
	return &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme"},
		Topology: &protoOperator.Topology{
			KernelNodes:  3,
			OpaNodes:     2,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"dog": 1},
		},
	}
}

func generateNodePoolsChart(t *testing.T, nodePools bool) *Charts {
	t.Helper()
	charts, err := New("test", "1.2", nil, map[string]bool{"dog": true}, nil).Generate(nodePoolsTestConfig(), 1, nodePools)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	return charts
}

func tenantPoolToleration() corev1.Toleration {
	return corev1.Toleration{Key: NodePoolLabel, Operator: corev1.TolerationOpEqual, Value: TenantPool, Effect: corev1.TaintEffectNoSchedule}
}

func hasToleration(spec corev1.PodSpec, want corev1.Toleration) bool {
	for _, tol := range spec.Tolerations {
		if tol == want {
			return true
		}
	}
	return false
}

// TestNodePoolsPlacePlatformOnCoreAndTenantDogsOnTenant: with pools on, every
// platform workload is pinned to the core pool, a tenant-owned instance to the
// tenant pool with a toleration of that pool's taint, and the arch toleration
// every workload needs survives alongside it.
func TestNodePoolsPlacePlatformOnCoreAndTenantDogsOnTenant(t *testing.T) {
	manifests := renderedManifests(t, generateNodePoolsChart(t, true))
	archToleration := corev1.Toleration{Key: "kubernetes.io/arch", Operator: corev1.TolerationOpEqual, Value: "arm64", Effect: corev1.TaintEffectNoSchedule}

	var kernel appsv1.StatefulSet
	decodeManifest(t, manifests, "StatefulSet/kernel", &kernel)
	var opa appsv1.Deployment
	decodeManifest(t, manifests, "Deployment/opa1", &opa)
	var dog appsv1.Deployment
	decodeManifest(t, manifests, "Deployment/dog-acme0", &dog)

	for _, tc := range []struct {
		name, pool, service string
		pod                 corev1.PodTemplateSpec
	}{
		{"kernel", CorePool, "kernel", kernel.Spec.Template},
		{"opa1", CorePool, "opa", opa.Spec.Template},
		{"dog-acme0", TenantPool, "dog-acme", dog.Spec.Template},
	} {
		if got := tc.pod.Spec.NodeSelector[NodePoolLabel]; got != tc.pool {
			t.Errorf("%s nodeSelector %s = %q, want %q", tc.name, NodePoolLabel, got, tc.pool)
		}
		if !hasToleration(tc.pod.Spec, archToleration) {
			t.Errorf("%s lost the arm64 toleration: %+v", tc.name, tc.pod.Spec.Tolerations)
		}
		if got := hasToleration(tc.pod.Spec, tenantPoolToleration()); got != (tc.pool == TenantPool) {
			t.Errorf("%s tolerates the tenant pool taint = %v, want %v", tc.name, got, tc.pool == TenantPool)
		}
		if got := tc.pod.Labels[ServicePodLabel]; got != tc.service {
			t.Errorf("%s pod label %s = %q, want %q", tc.name, ServicePodLabel, got, tc.service)
		}
	}
}

// TestNodePoolsBudgetMultiReplicaWorkloads: each workload with two or more
// replicas gets a PodDisruptionBudget of one, and its selector matches every
// replica's pods — including the per-replica Deployments, whose pods share no
// other label. A single replica gets no budget, so its node stays drainable.
func TestNodePoolsBudgetMultiReplicaWorkloads(t *testing.T) {
	manifests := renderedManifests(t, generateNodePoolsChart(t, true))

	pods := map[string][]corev1.PodTemplateSpec{}
	var kernel appsv1.StatefulSet
	decodeManifest(t, manifests, "StatefulSet/kernel", &kernel)
	pods["kernel"] = append(pods["kernel"], kernel.Spec.Template)
	for _, name := range []string{"opa0", "opa1"} {
		var d appsv1.Deployment
		decodeManifest(t, manifests, "Deployment/"+name, &d)
		pods["opa"] = append(pods["opa"], d.Spec.Template)
	}

	for svc, templates := range pods {
		var pdb policyv1.PodDisruptionBudget
		decodeManifest(t, manifests, "PodDisruptionBudget/"+svc, &pdb)
		if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
			t.Errorf("PodDisruptionBudget/%s maxUnavailable = %v, want 1", svc, pdb.Spec.MaxUnavailable)
		}
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			t.Fatalf("PodDisruptionBudget/%s selector: %v", svc, err)
		}
		for _, tmpl := range templates {
			if !sel.Matches(labels.Set(tmpl.Labels)) {
				t.Errorf("PodDisruptionBudget/%s selector %s does not match pod labels %v", svc, sel, tmpl.Labels)
			}
		}
	}

	for _, single := range []string{"PodDisruptionBudget/dog-acme", "PodDisruptionBudget/gateway"} {
		if _, ok := manifests[single]; ok {
			t.Errorf("a single-replica workload got %s; it would make its node undrainable for nothing", single)
		}
	}
}

// TestNoNodePoolsRendersNoPlacement: a cluster without a core pool must render
// no pool selector, label or budget at all. That is what lets the operator image
// roll out before the pools exist without leaving the fleet Pending.
func TestNoNodePoolsRendersNoPlacement(t *testing.T) {
	charts := generateNodePoolsChart(t, false)
	for path, data := range charts.Files {
		for _, unwanted := range []string{NodePoolLabel, ServicePodLabel, "PodDisruptionBudget", "nodeSelector"} {
			if strings.Contains(string(data), unwanted) {
				t.Errorf("%s contains %q with no node pools", path, unwanted)
			}
		}
	}
}

// decodeManifest strictly decodes one rendered manifest (keyed Kind/name) into
// the typed object, failing the test if the chart did not render it.
func decodeManifest(t *testing.T, manifests map[string]string, key string, into any) {
	t.Helper()
	doc, ok := manifests[key]
	if !ok {
		t.Fatalf("rendered chart has no %s", key)
	}
	if err := k8syaml.UnmarshalStrict([]byte(doc), into); err != nil {
		t.Fatalf("decoding %s: %v\n%s", key, err, doc)
	}
}
