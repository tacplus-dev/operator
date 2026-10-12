package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	k8syaml "sigs.k8s.io/yaml"

	protoKmaker "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/kmaker"
	protoOperator "gitlab.com/seabeam/schematic/platform/services/operator/model/types/services/operator"
)

func TestGenerateKernelProjectedTokensAndTokenReviewRBAC(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
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
		"- name: KMAKER_K8S_TOKEN_FILE\n          value: /var/run/secrets/schematic/kmaker/token",
		"- name: KERNEL_K8S_TOKEN_AUDIENCE\n          value: schematic-kernel",
		"- name: kmaker-token\n          mountPath: /var/run/secrets/schematic/kmaker\n          readOnly: true",
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
		Namespace: "schematic",
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
		Namespace: "schematic",
		Tls:       &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			SentinelNodes: 1,
			EventbusNodes: 1,
			GatewayNodes:  1,
			OauthNodes:    1,
			EchoNodes:     1,
			OpaNodes:      1,
			StratumNodes:  1,
		},
		OptionalServices: &protoOperator.OptionalServices{
			Oauth: true,
			Echo:  true,
		},
	})

	services := []string{
		"sentinel",
		"eventbus",
		"gateway",
		"opa",
		"stratum",
		"oauth",
		"letsencrypt",
		"echo",
	}
	for _, service := range services {
		t.Run(service, func(t *testing.T) {
			// stratum is a StatefulSet (its Hazelcast members need stable
			// per-pod DNS), so its workload file is named differently. The
			// projected-token assertions below apply to either shape.
			path := "templates/" + service + "/deployment.yaml"
			if _, ok := charts.Files[path]; !ok {
				path = "templates/" + service + "/statefulset.yaml"
			}
			deployment := chartFile(t, charts, path)
			for _, want := range []string{
				"serviceAccountName: " + service,
				"automountServiceAccountToken: false",
				"- name: KERNEL_K8S_TOKEN_FILE\n          value: /var/run/secrets/schematic/kernel/token",
				"- name: kernel-token\n          mountPath: /var/run/secrets/schematic/kernel\n          readOnly: true",
				"- name: kernel-token\n        projected:",
				"defaultMode: 0444",
				"path: token",
				"audience: schematic-kernel",
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
		"serviceAccountName: schematic-operator",
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

// stripYAMLComments drops comment lines so a "must NOT grant" assertion cannot
// be satisfied by a resource name that only appears in prose.
func stripYAMLComments(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestOperatorChartRBACCanManageGeneratedServiceRBAC(t *testing.T) {
	readTemplate := func(name string) string {
		data, err := os.ReadFile(filepath.Join("..", "..", "helm", "templates", name))
		if err != nil {
			t.Fatalf("read operator %s template: %v", name, err)
		}
		return stripYAMLComments(string(data))
	}
	clusterRole := readTemplate("clusterrole.yaml")
	role := readTemplate("role.yaml")

	// Cluster-scoped objects the operator genuinely has to reach: the
	// <service>-tokenreview ClusterRole/ClusterRoleBinding pair the generated
	// service chart emits, plus the TokenReview permission RBAC escalation
	// prevention requires it to hold before it can mint that ClusterRole.
	for _, want := range []string{
		"- clusterroles",
		"- clusterrolebindings",
		"- tokenreviews",
		`resources: ["nodes"]`,
		`resources: ["namespaces"]`,
	} {
		if !strings.Contains(clusterRole, want) {
			t.Fatalf("operator ClusterRole missing %q\n--- clusterrole ---\n%s", want, clusterRole)
		}
	}

	// Everything else is namespaced, including the <service>-pod-reader Role the
	// generated chart emits and the pods/get the operator must hold to mint it.
	for _, want := range []string{
		"- roles",
		"- rolebindings",
		"- pods",
		"- deployments",
		"- statefulsets",
		"- services",
		"- serviceaccounts",
		"- configmaps",
		"- secrets",
		"- persistentvolumeclaims",
		`resources: ["leases"]`,
		`resources: ["events"]`,
		// The harness CronJobs (D27), and the pods/delete the operator must hold
		// to mint the harness-chaos Role that grants it.
		"- cronjobs",
		`verbs: ["get", "delete"]`,
	} {
		if !strings.Contains(role, want) {
			t.Fatalf("operator Role missing %q\n--- role ---\n%s", want, role)
		}
	}

	// The point of the split: none of the namespaced grants may leak back into
	// the ClusterRole, where they would apply to every namespace in the cluster.
	for _, forbidden := range []string{
		"- roles",
		"- rolebindings",
		"- pods",
		"- deployments",
		"- statefulsets",
		"- services",
		"- serviceaccounts",
		"- configmaps",
		"- secrets",
		"- persistentvolumeclaims",
		`resources: ["leases"]`,
		`resources: ["events"]`,
	} {
		if strings.Contains(clusterRole, forbidden) {
			t.Fatalf("operator ClusterRole grants %q cluster-wide; it belongs in the namespaced Role\n--- clusterrole ---\n%s", forbidden, clusterRole)
		}
	}

	// The Role must be bound in every managed namespace, with the subject
	// pinned to the namespace the operator ServiceAccount actually lives in.
	for _, want := range []string{
		".Values.operator.managedNamespaces",
		"kind: RoleBinding",
		"namespace: {{ $ns }}",
		"namespace: {{ $.Values.operator.namespace }}",
	} {
		if !strings.Contains(role, want) {
			t.Fatalf("operator Role template missing %q\n--- role ---\n%s", want, role)
		}
	}
}

// TestSingleNodeKernelValuesAndTemplates verifies that a 1-node kernel topology
// produces a values.yaml with a kernel0 placeholder and templates that reference
// .Values.kernel0.config — the combination that previously caused a nil-pointer
// error during helm install dry-run.
func TestSingleNodeKernelValuesAndTemplates(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
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

	// secret-0.yaml (kernel's config holds key material) must reference the
	// kernel0 config value, not kernel's. Values lookups render as
	// `index .Values "<key>" "config"` so that hyphenated per-tenant keys parse.
	cm := chartFile(t, charts, "templates/kernel/secret-0.yaml")
	if !strings.Contains(cm, `index .Values "kernel0" "config"`) {
		t.Fatalf("kernel secret-0 must reference the kernel0 config value\n--- secret ---\n%s", cm)
	}

	// statefulset.yaml must checksum the kernel0 config value in its annotations.
	ss := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if !strings.Contains(ss, `index .Values "kernel0" "config"`) {
		t.Fatalf("kernel statefulset must reference the kernel0 config value\n--- statefulset ---\n%s", ss)
	}
}

// TestStatefulServiceWithoutPerReplicaConfigUsesTheSharedKey pins the rule that
// per-replica config keys belong to kernel (distinct TLS cert per replica), not
// to StatefulSets in general.
//
// stratum is a multi-replica StatefulSet whose replicas all read ONE config, and
// the bundle supplies exactly one entry under "stratum". When the key was chosen
// by IsStatefulSet, its configmaps referenced .Values.stratum0.config — which
// nothing populates — and all three pods died at startup with
// "data is null or empty".
func TestStatefulServiceWithoutPerReplicaConfigUsesTheSharedKey(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			StratumNodes: 3,
		},
	})

	ss := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	if !strings.Contains(ss, "replicas: 3") {
		t.Fatalf("guard: expected a 3-replica stratum StatefulSet\n--- statefulset ---\n%s", ss)
	}

	for i := range 3 {
		cm := chartFile(t, charts, fmt.Sprintf("templates/stratum/configmap-%d.yaml", i))
		if !strings.Contains(cm, `index .Values "stratum" "config"`) {
			t.Errorf("stratum configmap-%d must reference the shared stratum config value\n--- configmap ---\n%s", i, cm)
		}
		if strings.Contains(cm, fmt.Sprintf(`index .Values "stratum%d" "config"`, i)) {
			t.Errorf("stratum configmap-%d references the stratum%d config value, which nothing populates", i, i)
		}
		// The StatefulSet's checksum annotation must name the same key.
		if strings.Contains(ss, fmt.Sprintf(`index .Values "stratum%d" "config"`, i)) {
			t.Errorf("stratum statefulset checksum references the stratum%d config value, which nothing populates\n--- statefulset ---\n%s", i, ss)
		}
	}

	// The headless Service must publish not-ready addresses, or the members
	// cannot resolve each other at startup and split into one-member clusters.
	hs := chartFile(t, charts, "templates/stratum/service-headless.yaml")
	if !strings.Contains(hs, "publishNotReadyAddresses: true") {
		t.Errorf("headless Service must set publishNotReadyAddresses\n--- service ---\n%s", hs)
	}

	// values.yaml must not carry per-replica placeholders for stratum.
	values := chartFile(t, charts, "values.yaml")
	if strings.Contains(values, "stratum0:") {
		t.Errorf("values.yaml has a stratum0 placeholder; stratum shares one config\n--- values.yaml ---\n%s", values)
	}
}

// TestStratumRendersBothHazelcastClusters pins the wiring for stratum's SECOND
// Hazelcast cluster — the working-set maps.
//
// Two clusters in one JVM cannot share a port, and the event store pins 5001
// with auto-increment off, so the working set needs its own named port and its
// own seed list. Without the seed list it falls back to standalone: each pod
// holds working sets nobody else can see, and the shared Service hands the next
// request to a pod that has never heard of the map.
//
// Kernel has a single cluster and must keep rendering exactly one port block.
func TestStratumRendersBothHazelcastClusters(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			StratumNodes: 3,
		},
	})

	ss := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	for _, want := range []string{
		`- name: STRATUM_SEED_MEMBERS`,
		`"stratum-0.stratum:5001,stratum-1.stratum:5001,stratum-2.stratum:5001"`,
		`- name: STRATUM_WS_SEED_MEMBERS`,
		`"stratum-0.stratum:5701,stratum-1.stratum:5701,stratum-2.stratum:5701"`,
		"containerPort: 5001",
		"containerPort: 5701",
	} {
		if !strings.Contains(ss, want) {
			t.Errorf("stratum statefulset missing %q\n--- statefulset ---\n%s", want, ss)
		}
	}

	// Both ports must be reachable on the headless Service and on each per-pod
	// Service, or a seed address resolves to a member nothing can connect to.
	for _, f := range []string{
		"templates/stratum/service-headless.yaml",
		"templates/stratum/service-0.yaml",
		"templates/stratum/service-1.yaml",
		"templates/stratum/service-2.yaml",
	} {
		svc := chartFile(t, charts, f)
		if !strings.Contains(svc, "name: hazelcast\n") {
			t.Errorf("%s missing the event-store hazelcast port\n--- svc ---\n%s", f, svc)
		}
		if !strings.Contains(svc, "name: hazelcast-ws") || !strings.Contains(svc, "port: 5701") {
			t.Errorf("%s missing the working-set hazelcast port\n--- svc ---\n%s", f, svc)
		}
	}

	// Kernel is a HasHazelcast StatefulSet with ONE cluster: no ws port, no ws seeds.
	ks := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if strings.Contains(ks, "5701") || strings.Contains(ks, "STRATUM_WS_SEED_MEMBERS") {
		t.Errorf("kernel must not render a working-set cluster\n--- statefulset ---\n%s", ks)
	}
}

// The Hazelcast member protocol has no TLS and no member authentication, so the
// NetworkPolicy IS the authentication: it must name both member ports and admit
// only stratum's own pods on them, while leaving the gRPC service port and the
// kubelet's health port open — a policy that selects a pod default-denies
// everything it does not name, and forgetting 8444 takes the pod NotReady.
func TestStratumHazelcastNetworkPolicy(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  3,
			StratumNodes: 3,
		},
	})

	np := chartFile(t, charts, "templates/stratum/networkpolicy.yaml")
	for _, want := range []string{
		"kind: NetworkPolicy",
		"name: stratum-hazelcast",
		"  podSelector:\n    matchLabels:\n      app: stratum\n",
		"  - from:\n    - podSelector:\n        matchLabels:\n          app: stratum\n",
		"      port: 5001",
		"      port: 5701",
		"      port: 8443",
		"      port: 8444",
	} {
		if !strings.Contains(np, want) {
			t.Errorf("stratum networkpolicy missing %q\n--- networkpolicy ---\n%s", want, np)
		}
	}

	// Egress is deliberately not restricted: the exposure is inbound, and an
	// egress allowlist would have to enumerate every callee.
	if strings.Contains(np, "Egress") {
		t.Errorf("stratum networkpolicy must not restrict egress\n--- networkpolicy ---\n%s", np)
	}

	// Kernel has the same open member port but its policy is not verified yet, so
	// it must keep rendering none rather than acquire one by accident.
	if _, ok := charts.Files["templates/kernel/networkpolicy.yaml"]; ok {
		t.Error("kernel must not render a NetworkPolicy until its policy is verified")
	}
}

// stratum_search_nodes renders a SECOND, VOLUME-LESS stratum workload: search
// members, joining the same Hazelcast cluster as the writers with
// STRATUM_MEMBER_ROLE=search.
//
// The things that make it a search member rather than a fourth writer are all
// absences, so they are asserted as absences: no volumeClaimTemplate, no data
// mount, no storage value, no ServiceAccount or config of its own. A writer is
// what you get by forgetting any one of them, and a fourth writer with no volume
// would be counted for min_durable_copies and for the restore barrier while holding
// nothing durable at all.
func TestStratumSearchNodesRenderAVolumeLessWorkload(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:        1,
			StratumNodes:       2,
			StratumSearchNodes: 3,
		},
	})

	ss := chartFile(t, charts, "templates/stratum-search/statefulset.yaml")
	if !strings.Contains(ss, "replicas: 3") {
		t.Fatalf("guard: expected a 3-replica search StatefulSet\n--- statefulset ---\n%s", ss)
	}
	for _, want := range []string{
		`- name: STRATUM_MEMBER_ROLE`,
		`value: "search"`,
		"containerPort: 5001",
		"containerPort: 5701",
		`image: {{ index .Values "stratum-search" "image" }}`,
	} {
		if !strings.Contains(ss, want) {
			t.Errorf("search statefulset missing %q\n--- statefulset ---\n%s", want, ss)
		}
	}
	// The absences. A volume is the whole of what a writer has and a search member
	// does not.
	for _, unwanted := range []string{"volumeClaimTemplates", "mountPath: /app/data", "name: data"} {
		if strings.Contains(ss, unwanted) {
			t.Errorf("search statefulset carries %q; a search member has no volume\n--- statefulset ---\n%s",
				unwanted, ss)
		}
	}

	// The writers say writer OUT LOUD rather than leaning on the binary's default:
	// the durability-bearing half must not be the one left to a default.
	writers := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	if !strings.Contains(writers, `- name: STRATUM_MEMBER_ROLE`) ||
		!strings.Contains(writers, `value: "writer"`) {
		t.Errorf("the writers' statefulset does not declare its role\n--- statefulset ---\n%s", writers)
	}

	// Both seed lists span BOTH workloads, in both clusters. A search member that
	// could not resolve a writer at a cold start would form its own cluster of one.
	wantSeeds := []string{
		`"stratum-0.stratum:5001,stratum-1.stratum:5001,stratum-search-0.stratum-search:5001,` +
			`stratum-search-1.stratum-search:5001,stratum-search-2.stratum-search:5001"`,
		`"stratum-0.stratum:5701,stratum-1.stratum:5701,stratum-search-0.stratum-search:5701,` +
			`stratum-search-1.stratum-search:5701,stratum-search-2.stratum-search:5701"`,
	}
	for _, f := range []string{"templates/stratum/statefulset.yaml", "templates/stratum-search/statefulset.yaml"} {
		body := chartFile(t, charts, f)
		for _, want := range wantSeeds {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing seed list %s\n--- statefulset ---\n%s", f, want, body)
			}
		}
	}

	// It IS stratum: same identity to kernel, same config, so no ServiceAccount and
	// no config key of its own. A second ServiceAccount object named "stratum" is a
	// duplicate resource, and one named "stratum-search" is a principal kernel has
	// never heard of.
	if !strings.Contains(ss, "serviceAccountName: stratum\n") {
		t.Errorf("search pods must run as stratum's service account\n--- statefulset ---\n%s", ss)
	}
	if _, ok := charts.Files["templates/stratum-search/serviceaccount.yaml"]; ok {
		t.Error("the search workload emitted a ServiceAccount of its own")
	}
	for i := range 3 {
		cm := chartFile(t, charts, fmt.Sprintf("templates/stratum-search/configmap-%d.yaml", i))
		if !strings.Contains(cm, `index .Values "stratum" "config"`) {
			t.Errorf("search configmap-%d must read stratum's config\n--- configmap ---\n%s", i, cm)
		}
	}
	values := chartFile(t, charts, "values.yaml")
	if !strings.Contains(values, "stratum-search:") {
		t.Errorf("values.yaml has no stratum-search entry, so its image is unresolvable\n--- values.yaml ---\n%s", values)
	}
	if strings.Contains(values, "stratum-search:\n  image: {{ .Image }}") {
		t.Errorf("values.yaml did not render the search image\n--- values.yaml ---\n%s", values)
	}

	// The member protocol has no authentication, so each workload's policy must admit
	// the other's pods — separate podSelectors, because one matchLabels is an AND and
	// two app values there would match nothing.
	for _, f := range []string{"templates/stratum/networkpolicy.yaml", "templates/stratum-search/networkpolicy.yaml"} {
		np := chartFile(t, charts, f)
		for _, want := range []string{
			"        matchLabels:\n          app: stratum\n",
			"        matchLabels:\n          app: stratum-search\n",
			"      port: 5001",
			"      port: 5701",
			"      port: 8444",
		} {
			if !strings.Contains(np, want) {
				t.Errorf("%s missing %q\n--- networkpolicy ---\n%s", f, want, np)
			}
		}
	}
}

// The default is zero search members, and zero renders NOTHING — not an empty
// StatefulSet, not a dangling values key, not a seed address that resolves to no
// pod.
func TestNoSearchNodesRendersNoSearchWorkload(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			StratumNodes: 3,
		},
	})

	for path := range charts.Files {
		if strings.HasPrefix(path, "templates/stratum-search/") {
			t.Errorf("a fleet with no search nodes rendered %s", path)
		}
	}
	ss := chartFile(t, charts, "templates/stratum/statefulset.yaml")
	if strings.Contains(ss, "stratum-search") {
		t.Errorf("the writers' seed list names search pods that do not exist\n--- statefulset ---\n%s", ss)
	}
	values := chartFile(t, charts, "values.yaml")
	if strings.Contains(values, "stratum-search:") {
		t.Errorf("values.yaml carries a stratum-search entry for a workload that is not deployed\n--- values.yaml ---\n%s", values)
	}
}

func generateTestChart(t *testing.T, cfg *protoOperator.Config) *Charts {
	t.Helper()
	return generateTestChartWithTenantOwned(t, cfg, nil)
}

// generateTestChartWithTenantOwned is generateTestChart for a fleet where some
// services carry registry.json's (spec.service).tenant_owned annotation.
func generateTestChartWithTenantOwned(t *testing.T, cfg *protoOperator.Config, tenantOwned map[string]bool) *Charts {
	t.Helper()
	return generateTestChartWithPin(t, cfg, tenantOwned, nil)
}

// generateTestChartWithPin adds the (spec.service).pinned_tenant half: some of
// those tenant-owned services are pinned to a single tenant.
func generateTestChartWithPin(t *testing.T, cfg *protoOperator.Config, tenantOwned map[string]bool, pinnedTenant map[string]string) *Charts {
	t.Helper()
	charts, err := New("test", "1.2", nil, tenantOwned, pinnedTenant).Generate(cfg, 1, false)
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
		"schematic.grpcProbe5000", "schematic.startupProbe5000",
		"schematic.grpcProbe8443", "schematic.startupProbe8443",
	} {
		if !strings.Contains(body, probe) {
			t.Errorf("probe helper %q is missing", probe)
		}
	}
	// No probe helper may fall back to a TCP check.
	probeRegion := body
	if i := strings.Index(probeRegion, `"schematic.grpcProbe5000"`); i >= 0 {
		if j := strings.Index(probeRegion[i:], `"schematic.imagePullSecrets"`); j > 0 {
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

// TestPVCPodsSetFsGroupSoNonRootCanWrite guards a defect that kept the fleet's
// event bus dead while every probe reported it healthy.
//
// A freshly provisioned PersistentVolume is root:root 0755. eventbus runs as the
// distroless `nonroot` user (65532), so it could not create
// /app/data/__cluster_metadata-0 and its embedded Kafka broker failed to format
// KRaft storage on every single attempt. It was on retry 131 when this was
// found; stratum and timedb had spent that whole time logging "Kafka not yet
// initialized".
//
// Nothing caught it because the pod's probes only ask whether the gRPC health
// endpoint answers, and it did.
//
// fsGroup must be gated on HasPVC and applied by BOTH renderers -- the
// text/template path and the buffer-built path -- or whichever one a service
// happens to use decides whether its volume is writable.
func TestPVCPodsSetFsGroupSoNonRootCanWrite(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, "func pvcFsGroupLines(svc serviceSpec) string {") {
		t.Fatal("pvcFsGroupLines is gone; a PVC pod's volume is root-owned and a non-root container cannot write to it")
	}
	if !strings.Contains(body, "%s      containers:") {
		t.Error("the buffer-built Deployment must emit the fsGroup securityContext before containers:")
	}
	if !strings.Contains(body, "pvcFsGroupLines(svc)") {
		t.Error("the buffer-built Deployment must call pvcFsGroupLines")
	}
	if !strings.Contains(body, "{{- if $.HasPVC }}\n      securityContext:\n        fsGroup: 65532") {
		t.Error("the templated Deployment must set fsGroup for PVC-owning pods too")
	}
	if !strings.Contains(body, "if !svc.HasPVC {\n\t\treturn \"\"\n\t}") {
		t.Error("fsGroup must be gated on HasPVC — pods with no volume have nothing to chgrp")
	}
}

// TestAppServicesRenderWithoutAPlatformProtoField is the point of app_services.
//
// Every other replica count on Topology is a fixed proto field, so before this
// an application could not declare a service without editing the PLATFORM's
// operator.proto — and proto3 offers no way to add a field to a message owned by
// another repo. echo, a sample service, had to take field 17 there just to be
// deployable, which is the clearest evidence that "schematic is the reusable
// base" was not yet true.
//
// A service named only in the app's own config must now produce a full workload
// with no platform proto change at all.
func TestAppServicesRenderWithoutAPlatformProtoField(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"widgeter": 2},
		},
	})

	dep := chartFile(t, charts, "templates/widgeter/deployment.yaml")
	if !strings.Contains(dep, "widgeter") {
		t.Errorf("app service produced no Deployment naming it:\n%s", dep)
	}
	if _, ok := charts.Files["templates/widgeter/service-0.yaml"]; !ok {
		t.Error("app service produced no Service; it would be unreachable in-cluster")
	}
	// A count of 0 means "not deployed" — presence with a positive count is the
	// enablement, so an app service needs no parallel optional_services flag.
	off := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"widgeter": 0},
		},
	})
	for path := range off.Files {
		if strings.Contains(path, "widgeter") {
			t.Errorf("app service with 0 replicas still rendered %s", path)
		}
	}
}

// The app_services path must produce the SAME workload as the fixed-field path
// it replaces.
//
// echo is the only service that exists both ways: it has a legacy echo_nodes
// field and it is the model the app-service block was written from. Rendering it
// each way and diffing is the strongest available check that an application
// declaring a service by name gets what a platform service gets — without
// having to deploy one first.
//
// Any difference other than the source of the replica count is a bug in the
// generic path.
func TestAppServiceRendersIdenticallyToTheFixedFieldPath(t *testing.T) {
	viaField := generateTestChart(t, &protoOperator.Config{
		Namespace:        "schematic",
		Topology:         &protoOperator.Topology{KernelNodes: 1, KmakerNodes: 1, EchoNodes: 1},
		OptionalServices: &protoOperator.OptionalServices{Echo: true},
	})
	viaMap := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{KernelNodes: 1, KmakerNodes: 1,
			AppServices: map[string]int32{"echo": 1}},
	})

	// Without this the whole comparison passes vacuously when neither rendering
	// emits echo at all — which is exactly how a "both paths agree" test lies.
	compared := 0
	for path, want := range viaField.Files {
		if !strings.HasPrefix(path, "templates/echo/") {
			continue
		}
		compared++
		got, ok := viaMap.Files[path]
		if !ok {
			t.Errorf("app_services did not produce %s, which the fixed field does", path)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s differs between the two paths:\n--- via echo_nodes ---\n%s\n--- via app_services ---\n%s",
				path, want, got)
		}
	}
	if compared == 0 {
		t.Fatal("neither path rendered any echo templates; the comparison proved nothing")
	}

	// And the reverse: no extra files the fixed path does not create.
	for path := range viaMap.Files {
		if !strings.HasPrefix(path, "templates/echo/") {
			continue
		}
		if _, ok := viaField.Files[path]; !ok {
			t.Errorf("app_services produced %s that the fixed field does not", path)
		}
	}
}

// TestEveryWorkloadToleratesTheArmTaint guards the toleration that lets this
// fleet schedule at all.
//
// The whole fleet is arm64 by design (control/GKE.md: t2a Spot nodes) and GKE
// taints Arm nodes kubernetes.io/arch=arm64:NoSchedule. Missing the toleration
// is invisible until a node is REPLACED — a running pod is not evicted when its
// node gains a NoSchedule taint — so it stays latent until the worst moment.
//
// It surfaced on a Spot preemption: the replacement node carried the taint and
// 14 of 15 pods went Pending with "1 node(s) had untolerated taint(s)" while the
// node sat at 25% CPU. The single pod that stayed up was cert-manager's
// acme-solver, which ships this toleration itself.
func TestEveryWorkloadToleratesTheArmTaint(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			StratumNodes:  3,
			SentinelNodes: 1,
			GatewayNodes:  1,
			EventbusNodes: 1,
			AppServices:   map[string]int32{"echo": 1},
		},
	})

	checked := 0
	for path, body := range charts.Files {
		if !strings.HasSuffix(path, "/deployment.yaml") && !strings.HasSuffix(path, "/statefulset.yaml") {
			continue
		}
		checked++
		text := string(body)
		if !strings.Contains(text, "key: kubernetes.io/arch") ||
			!strings.Contains(text, "value: arm64") ||
			!strings.Contains(text, "effect: NoSchedule") {
			t.Errorf("%s has no arm64 toleration; it cannot schedule on a tainted Arm node\n%s", path, text)
		}
	}
	if checked == 0 {
		t.Fatal("guard: no workload manifests were examined")
	}
}

// TestMultiReplicaServicesAskToSpreadAcrossNodes pins the anti-affinity that
// makes replica count mean something.
//
// Three kernels or three stratums stacked on one node survive nothing, and
// their Hazelcast members would share exactly the fate they exist to be
// independent of. The rule is PREFERRED, not required: with more replicas than
// nodes a required rule leaves pods Pending forever, and redundancy that cannot
// schedule is worse than co-location.
func TestMultiReplicaServicesAskToSpreadAcrossNodes(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			StratumNodes:  3,
			SentinelNodes: 1,
		},
	})

	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/stratum/statefulset.yaml",
	} {
		body := chartFile(t, charts, path)
		for _, want := range []string{
			"podAntiAffinity",
			"preferredDuringSchedulingIgnoredDuringExecution",
			"topologyKey: kubernetes.io/hostname",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q — replicas would stack on one node\n%s", path, want, body)
			}
		}
		if strings.Contains(body, "requiredDuringSchedulingIgnoredDuringExecution") {
			t.Errorf("%s uses a REQUIRED anti-affinity rule; with replicas > nodes that leaves pods Pending forever", path)
		}
	}

	// A single-replica service gains nothing from the rule and should not carry it.
	single := chartFile(t, charts, "templates/sentinel/deployment.yaml")
	if strings.Contains(single, "podAntiAffinity") {
		t.Errorf("a 1-replica service should not carry anti-affinity\n%s", single)
	}
}

// A StatefulSet owns its volume through a volumeClaimTemplate, which is exactly
// the case that was missing fsGroup: the Deployment path called
// pvcFsGroupLines and the StatefulSet path did not, so kernel and stratum were
// the only two services that could not write to their own disk.
//
// It failed silently for as long as it existed. Kernel's persistence layer logs
// a WARN and returns -- "Failed to persist kernel disk snapshot to
// /app/data/kernel: AccessDeniedException" -- so every probe passed, the fleet
// was healthy, and the config state simply never reached the disk. Turn-down
// then looked like it was destroying state that had in fact never been written.
func TestStatefulSetsCanWriteToTheirOwnVolume(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			StratumNodes:  3,
			SentinelNodes: 1,
		},
	})

	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/stratum/statefulset.yaml",
	} {
		body := chartFile(t, charts, path)
		if !strings.Contains(body, "volumeClaimTemplates") {
			t.Fatalf("%s has no volumeClaimTemplates — this test is asserting against the wrong file", path)
		}
		if !strings.Contains(body, "fsGroup: 65532") {
			t.Errorf("%s mounts a PVC but sets no fsGroup; a distroless nonroot image cannot write to a root-owned volume\n%s", path, body)
		}
	}
}

// TestKernelStatefulSetStartsPodsOneAtATime: kernel's state lives only in
// memory, replicated between members. With Parallel pod management every
// missing kernel pod is created at once — the 0→3 scale on 2026-09-23 put the
// whole cluster up together with nobody holding state. OrderedReady does not
// create pod N+1 until pod N is Running and Ready.
func TestKernelStatefulSetStartsPodsOneAtATime(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 3},
	})

	ss := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	if !strings.Contains(ss, "podManagementPolicy: OrderedReady") {
		t.Errorf("kernel StatefulSet must use OrderedReady pod management:\n%s", ss)
	}
}

// TestKeyMaterialConfigsArePackagedAsSecrets: kernel's per-node config carries
// the node TLS key and its signing CA key, kmaker's carries the cluster CA
// signing key, and gateway's carries the external TLS key. All three were
// packaged as ConfigMaps, which are not treated as secret data by Kubernetes —
// no encryption-at-rest by default, and readable by anything with configmap get.
func TestKeyMaterialConfigsArePackagedAsSecrets(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   3,
			KmakerNodes:   1,
			GatewayNodes:  1,
			SentinelNodes: 1,
		},
	})

	for _, tc := range []struct {
		svc      string
		replicas int
	}{
		{"kernel", 3},
		{"kmaker", 1},
		{"gateway", 1},
	} {
		for i := range tc.replicas {
			if _, ok := charts.Files[fmt.Sprintf("templates/%s/configmap-%d.yaml", tc.svc, i)]; ok {
				t.Errorf("%s config %d is still emitted as a ConfigMap; it holds private key material", tc.svc, i)
			}
			raw, ok := charts.Files[fmt.Sprintf("templates/%s/secret-%d.yaml", tc.svc, i)]
			if !ok {
				t.Errorf("generated chart has no templates/%s/secret-%d.yaml", tc.svc, i)
				continue
			}
			secret := string(raw)
			for _, want := range []string{"kind: Secret", "type: Opaque", "stringData:", "config.yaml:"} {
				if !strings.Contains(secret, want) {
					t.Errorf("%s secret-%d missing %q\n--- secret ---\n%s", tc.svc, i, want, secret)
				}
			}
			if strings.Contains(secret, "kind: ConfigMap") {
				t.Errorf("%s secret-%d is a ConfigMap\n--- secret ---\n%s", tc.svc, i, secret)
			}
		}
	}

	// The workloads must mount the Secret, not a ConfigMap of the same name.
	statefulSet := chartFile(t, charts, "templates/kernel/statefulset.yaml")
	for i := range 3 {
		want := fmt.Sprintf("- name: config-%d\n        secret:\n          secretName: kernel-%d-config", i, i)
		if !strings.Contains(statefulSet, want) {
			t.Errorf("kernel StatefulSet does not mount config-%d from a Secret\n--- statefulset ---\n%s", i, statefulSet)
		}
	}
	for i := range 3 {
		if strings.Contains(statefulSet, fmt.Sprintf("configMap:\n          name: kernel-%d-config", i)) {
			t.Errorf("kernel StatefulSet still mounts config-%d from a ConfigMap\n--- statefulset ---\n%s", i, statefulSet)
		}
	}
	for _, svc := range []string{"kmaker", "gateway"} {
		deployment := chartFile(t, charts, fmt.Sprintf("templates/%s/deployment.yaml", svc))
		want := fmt.Sprintf("- name: config\n        secret:\n          secretName: %s0-config", svc)
		if !strings.Contains(deployment, want) {
			t.Errorf("%s Deployment does not mount its config from a Secret\n--- deployment ---\n%s", svc, deployment)
		}
		if strings.Contains(deployment, fmt.Sprintf("configMap:\n          name: %s0-config", svc)) {
			t.Errorf("%s Deployment still mounts its config from a ConfigMap\n--- deployment ---\n%s", svc, deployment)
		}
	}

	// A service with no key material in its config stays a ConfigMap.
	sentinel := chartFile(t, charts, "templates/sentinel/configmap-0.yaml")
	if !strings.Contains(sentinel, "kind: ConfigMap") {
		t.Errorf("sentinel config should still be a ConfigMap\n--- configmap ---\n%s", sentinel)
	}
}

// TestWriteToDiskRemovesSupersededConfigMapTemplate: an output directory
// generated before the key-material configs moved to Secrets still holds
// templates/kernel/configmap-0.yaml. Nothing else prunes that directory, and
// helm would apply the stale ConfigMap alongside the new Secret.
func TestWriteToDiskRemovesSupersededConfigMapTemplate(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "schematic", "templates", "kernel", "configmap-0.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("kind: ConfigMap\n"), 0644); err != nil {
		t.Fatal(err)
	}

	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology:  &protoOperator.Topology{KernelNodes: 1},
	})
	if err := New("test", "1.2", nil, nil, nil).WriteToDisk(charts, dir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale kernel configmap-0.yaml still on disk (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "schematic", "templates", "kernel", "secret-0.yaml")); err != nil {
		t.Errorf("kernel secret-0.yaml not written: %v", err)
	}
}

// TestChartLabelsWorkloadsWithTheOperatorManagedByValue guards the contract the
// reconciler's stale-deployment pruning selects on. Helm 3 does not set
// app.kubernetes.io/managed-by by itself, so the value is whatever this chart's
// helper writes -- and the reconciler must select on exactly that.
func TestChartLabelsWorkloadsWithTheOperatorManagedByValue(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
		},
	})

	want := ManagedByLabel + ": " + ManagedByValue
	helpers := chartFile(t, charts, "templates/_helpers.tpl")
	if !strings.Contains(helpers, want) {
		t.Fatalf("_helpers.tpl missing %q\n--- helpers ---\n%s", want, helpers)
	}
	if strings.Contains(helpers, ManagedByLabel+": Helm") {
		t.Fatalf("_helpers.tpl still labels resources managed-by Helm:\n%s", helpers)
	}

	// Every workload pulls the label in via the shared helper.
	for _, path := range []string{
		"templates/kernel/statefulset.yaml",
		"templates/gateway/deployment.yaml",
	} {
		body := chartFile(t, charts, path)
		if !strings.Contains(body, `include "schematic.labels"`) {
			t.Fatalf("%s does not include the schematic.labels helper:\n%s", path, body)
		}
	}
}

// TestWriteToDiskWritesChartFilesPrivate: the rendered chart holds the GitLab
// registry token (templates/registry-secret.yaml) and the service configs
// carrying kernel's and kmaker's private keys (templates/<svc>/secret-N.yaml).
// Every file was written 0644, so all of that was world-readable on the
// operator host. It also has to tighten a file already on disk: the tree is
// routinely regenerated in place and os.WriteFile leaves an existing file's
// mode alone, so a chart first rendered by a pre-fix binary would stay 0644.
func TestWriteToDiskWritesChartFilesPrivate(t *testing.T) {
	dir := t.TempDir()
	preExisting := filepath.Join(dir, "schematic", "templates", "registry-secret.yaml")
	if err := os.MkdirAll(filepath.Dir(preExisting), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(preExisting, []byte("kind: Secret\n"), 0644); err != nil {
		t.Fatal(err)
	}

	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:           "schematic",
		GitlabRegistryToken: "glpat-not-a-real-token",
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
		},
	})
	if _, ok := charts.Files["templates/registry-secret.yaml"]; !ok {
		t.Fatal("generated chart has no templates/registry-secret.yaml — test would not cover the token")
	}
	if _, ok := charts.Files["templates/kernel/secret-0.yaml"]; !ok {
		t.Fatal("generated chart has no templates/kernel/secret-0.yaml — test would not cover key material")
	}

	if err := New("test", "1.2", nil, nil, nil).WriteToDisk(charts, dir); err != nil {
		t.Fatalf("WriteToDisk() error = %v", err)
	}

	for relPath := range charts.Files {
		info, err := os.Stat(filepath.Join(dir, "schematic", relPath))
		if err != nil {
			t.Errorf("stat %s: %v", relPath, err)
			continue
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Errorf("%s mode = %#o, want 0600 (the chart tree carries the registry token and kernel's private keys)", relPath, got)
		}
	}
}

// TestTenantOwnedServiceRendersOneWorkloadPerTenant is the chart half of goal A
// item 4: a service registry.json marks (spec.service).tenant_owned gets one
// Deployment and one ServiceAccount per tenant, named "<svc>-<tenant>", instead
// of one shared instance for the whole cluster.
func TestTenantOwnedServiceRendersOneWorkloadPerTenant(t *testing.T) {
	charts := generateTestChartWithTenantOwned(t, &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme", "beta"},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"docmanager": 1, "tasks": 1},
		},
	}, map[string]bool{"docmanager": true})

	// The shared service the cluster also runs is untouched: still one instance
	// under its bare name, and no per-tenant object anywhere.
	if _, ok := charts.Files["templates/tasks/deployment.yaml"]; !ok {
		t.Fatalf("shared app service tasks lost its deployment; files: %v", chartFileNames(charts))
	}
	for _, tenant := range []string{"acme", "beta"} {
		if _, ok := charts.Files[fmt.Sprintf("templates/tasks-%s/deployment.yaml", tenant)]; ok {
			t.Errorf("tasks is not tenant_owned but got a per-tenant deployment for %q", tenant)
		}
	}

	// The tenant-owned service is deployed once per tenant, and NOT once under
	// its bare name.
	if _, ok := charts.Files["templates/docmanager/deployment.yaml"]; ok {
		t.Errorf("tenant-owned docmanager still rendered a shared deployment; the tenants collapsed into one")
	}
	for _, tenant := range []string{"acme", "beta"} {
		name := "docmanager-" + tenant

		dep := chartFile(t, charts, fmt.Sprintf("templates/%s/deployment.yaml", name))
		if !strings.Contains(dep, "name: "+name+"0") {
			t.Errorf("%s deployment is not named %s0\n--- deployment ---\n%s", name, name, dep)
		}
		// It runs as its own ServiceAccount — the one the kernel delegate for
		// (docmanager, /tenants/<tenant>) matches on.
		if !strings.Contains(dep, "serviceAccountName: "+name) {
			t.Errorf("%s deployment does not run as service account %q\n--- deployment ---\n%s", name, name, dep)
		}
		// Same image for every tenant: instances differ in identity, not build.
		if !strings.Contains(dep, fmt.Sprintf(`image: {{ index .Values "%s" "image" }}`, name)) {
			t.Errorf("%s deployment image ref is not the hyphen-safe values lookup\n--- deployment ---\n%s", name, dep)
		}

		sa := chartFile(t, charts, fmt.Sprintf("templates/%s/serviceaccount.yaml", name))
		if !strings.Contains(sa, "name: "+name) {
			t.Errorf("%s serviceaccount is not named %q\n--- serviceaccount ---\n%s", name, name, sa)
		}

		// values.yaml must carry the per-instance key the templates look up,
		// and the configmap must reach it through index (a dashed key is not
		// addressable as .Values.docmanager-acme.config).
		values := chartFile(t, charts, "values.yaml")
		if !strings.Contains(values, name+":") {
			t.Errorf("values.yaml missing the %q key\n--- values.yaml ---\n%s", name, values)
		}
		cm := chartFile(t, charts, fmt.Sprintf("templates/%s/configmap-0.yaml", name))
		if !strings.Contains(cm, fmt.Sprintf(`index .Values "%s" "config"`, name)) {
			t.Errorf("%s configmap does not reference its own config value\n--- configmap ---\n%s", name, cm)
		}
	}

	// The reconciler prunes deployments not in this set; per-tenant workloads
	// have to be in it or every reconcile would delete them.
	names := New("test", "1.2", nil, map[string]bool{"docmanager": true}, nil).ExpectedDeploymentNames(&protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme", "beta"},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"docmanager": 1, "tasks": 1},
		},
	}, 1)
	for _, want := range []string{"docmanager-acme0", "docmanager-beta0", "tasks0"} {
		if !names[want] {
			t.Errorf("ExpectedDeploymentNames missing %q: %v", want, names)
		}
	}
	if names["docmanager0"] {
		t.Errorf("ExpectedDeploymentNames still expects the collapsed docmanager0: %v", names)
	}
}

// TestPinnedTenantOwnedServiceRendersOneWorkload covers the other cardinality
// (spec.service) can declare: a tenant-owned service that also carries
// pinned_tenant is ONE instance, in that tenant, however many tenants the
// cluster lists. A service whose rows are keyed by something other than the
// tenant — one row per person, not per company — is split in half by the
// per-tenant replication tenant_owned alone gives it.
func TestPinnedTenantOwnedServiceRendersOneWorkload(t *testing.T) {
	cfg := &protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme", "beta", "people"},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"profile": 1, "docmanager": 1},
		},
	}
	charts := generateTestChartWithPin(t, cfg,
		map[string]bool{"profile": true, "docmanager": true},
		map[string]string{"profile": "people"})

	// The pinned service exists exactly once, in the tenant it names.
	dep := chartFile(t, charts, "templates/profile-people/deployment.yaml")
	if !strings.Contains(dep, "serviceAccountName: profile-people") {
		t.Errorf("pinned profile does not run as service account profile-people\n--- deployment ---\n%s", dep)
	}
	if _, ok := charts.Files["templates/profile/deployment.yaml"]; ok {
		t.Errorf("pinned profile also rendered a shared deployment")
	}
	for _, tenant := range []string{"acme", "beta"} {
		if _, ok := charts.Files[fmt.Sprintf("templates/profile-%s/deployment.yaml", tenant)]; ok {
			t.Errorf("pinned profile rendered an instance for tenant %q as well", tenant)
		}
	}

	// The unpinned tenant-owned service in the same cluster is unaffected: one
	// instance per configured tenant, exactly as before.
	for _, tenant := range []string{"acme", "beta", "people"} {
		if _, ok := charts.Files[fmt.Sprintf("templates/docmanager-%s/deployment.yaml", tenant)]; !ok {
			t.Errorf("unpinned tenant-owned docmanager lost its %q instance; files: %v", tenant, chartFileNames(charts))
		}
	}

	// The reconciler prunes what ExpectedDeploymentNames does not list, so the
	// single pinned workload has to be in it and the per-tenant ones must not.
	names := New("test", "1.2", nil,
		map[string]bool{"profile": true, "docmanager": true},
		map[string]string{"profile": "people"}).ExpectedDeploymentNames(cfg, 1)
	if !names["profile-people0"] {
		t.Errorf("ExpectedDeploymentNames missing profile-people0: %v", names)
	}
	for _, unwanted := range []string{"profile0", "profile-acme0", "profile-beta0"} {
		if names[unwanted] {
			t.Errorf("ExpectedDeploymentNames expects %q for a pinned service: %v", unwanted, names)
		}
	}
}

// TestPinnedTenantNotConfiguredFailsGeneration: a pin names where the service's
// one instance lives, so a pin to a tenant the config does not list has no
// honest rendering — dropping the service silently would take it out of the
// fleet, and rendering it anyway would give it an identity kernel mints for
// nobody. Reconciliation fails instead.
func TestPinnedTenantNotConfiguredFailsGeneration(t *testing.T) {
	_, err := New("test", "1.2", nil,
		map[string]bool{"profile": true},
		map[string]string{"profile": "people"}).Generate(&protoOperator.Config{
		Namespace: "schematic",
		Tenants:   []string{"acme", "beta"},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"profile": 1},
		},
	}, 1, false)
	if err == nil {
		t.Fatal("Generate accepted a service pinned to a tenant config.tenants does not list")
	}
	for _, want := range []string{"profile", "people", "pinned_tenant"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
}

// TestTenantOwnedServiceWithNoTenantsRendersNothing pins the degenerate case: a
// tenant-owned service in a cluster that lists no tenants has no instance to
// deploy, rather than silently falling back to one shared instance.
func TestTenantOwnedServiceWithNoTenantsRendersNothing(t *testing.T) {
	charts := generateTestChartWithTenantOwned(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			GatewayNodes: 1,
			AppServices:  map[string]int32{"docmanager": 1},
		},
	}, map[string]bool{"docmanager": true})

	for path := range charts.Files {
		if strings.HasPrefix(path, "templates/docmanager") {
			t.Errorf("tenant-owned docmanager rendered %q with no tenants configured", path)
		}
	}
}

func chartFileNames(charts *Charts) []string {
	names := make([]string, 0, len(charts.Files))
	for name := range charts.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestPersonalauthRendersOnlyWhenEnabled pins the personalauth workload to its
// optional_services flag. It is a login broker for PERSONAL principals and is
// disabled by default, so a cluster that never asks for it must not get a
// Deployment pulling an image it does not run.
func TestPersonalauthRendersOnlyWhenEnabled(t *testing.T) {
	on := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:       1,
			KmakerNodes:       1,
			PersonalauthNodes: 2,
		},
		OptionalServices: &protoOperator.OptionalServices{Personalauth: true},
	})

	dep := chartFile(t, on, "templates/personalauth/deployment.yaml")
	if !strings.Contains(dep, "personalauth") {
		t.Errorf("personalauth produced no Deployment naming it:\n%s", dep)
	}
	if _, ok := on.Files["templates/personalauth/service-0.yaml"]; !ok {
		t.Error("personalauth produced no Service; it would be unreachable in-cluster")
	}

	off := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:       1,
			KmakerNodes:       1,
			PersonalauthNodes: 2,
		},
	})
	for path := range off.Files {
		if strings.Contains(path, "personalauth") {
			t.Errorf("personalauth rendered %s while optional_services.personalauth is false", path)
		}
	}
}

// TestLlmCredentialServiceConfigsArePackagedAsSecrets guards the generalized
// version of the plaintext-credential exposure: pkg/bootstrap/deploy.go injects
// the real llm_credential value into the bootstrap config of every service named
// in llm_credential_services, but ConfigHasKeyMaterial used to be hardcoded on
// kernel/kmaker/gateway only — so any app service opted into that list had its
// credential rendered into a plain ConfigMap by construction. The list is
// config-driven, so this cannot be fixed by naming more services.
func TestLlmCredentialServiceConfigsArePackagedAsSecrets(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:             "schematic",
		LlmCredentialServices: []string{"tasks"},
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"tasks": 1, "widgeter": 1},
		},
	})

	// tasks holds the credential: Secret, never a ConfigMap.
	if _, ok := charts.Files["templates/tasks/configmap-0.yaml"]; ok {
		t.Error("tasks config is emitted as a ConfigMap; it holds the llm_credential value")
	}
	secret := chartFile(t, charts, "templates/tasks/secret-0.yaml")
	for _, want := range []string{"kind: Secret", "type: Opaque", "stringData:", "config.yaml:"} {
		if !strings.Contains(secret, want) {
			t.Errorf("tasks secret-0 missing %q\n--- secret ---\n%s", want, secret)
		}
	}
	tasksDep := chartFile(t, charts, "templates/tasks/deployment.yaml")
	if !strings.Contains(tasksDep, "- name: config\n        secret:\n          secretName: tasks0-config") {
		t.Errorf("tasks Deployment does not mount its config from a Secret\n--- deployment ---\n%s", tasksDep)
	}
	if strings.Contains(tasksDep, "configMap:\n          name: tasks0-config") {
		t.Errorf("tasks Deployment still mounts its config from a ConfigMap\n--- deployment ---\n%s", tasksDep)
	}

	// widgeter is not in the list: unchanged, still a plain ConfigMap.
	if _, ok := charts.Files["templates/widgeter/secret-0.yaml"]; ok {
		t.Error("widgeter config became a Secret; it carries no key material")
	}
	widgeterCM := chartFile(t, charts, "templates/widgeter/configmap-0.yaml")
	if !strings.Contains(widgeterCM, "kind: ConfigMap") {
		t.Errorf("widgeter config should still be a ConfigMap\n--- configmap ---\n%s", widgeterCM)
	}
	widgeterDep := chartFile(t, charts, "templates/widgeter/deployment.yaml")
	if !strings.Contains(widgeterDep, "configMap:\n          name: widgeter0-config") {
		t.Errorf("widgeter Deployment no longer mounts its config from a ConfigMap\n--- deployment ---\n%s", widgeterDep)
	}
}

// A tenant-owned service in llm_credential_services gets one config per tenant
// instance, and deploy.go injects the credential into every one of them.
func TestLlmCredentialTenantOwnedServiceConfigsArePackagedAsSecrets(t *testing.T) {
	charts := generateTestChartWithTenantOwned(t, &protoOperator.Config{
		Namespace:             "schematic",
		LlmCredentialServices: []string{"tasks"},
		Tenants:               []string{"acme", "globex"},
		Topology: &protoOperator.Topology{
			KernelNodes: 1,
			KmakerNodes: 1,
			AppServices: map[string]int32{"tasks": 1},
		},
	}, map[string]bool{"tasks": true})

	for _, tenant := range []string{"acme", "globex"} {
		inst := TenantInstanceName("tasks", tenant)
		if _, ok := charts.Files[fmt.Sprintf("templates/%s/configmap-0.yaml", inst)]; ok {
			t.Errorf("%s config is emitted as a ConfigMap; it holds the llm_credential value", inst)
		}
		secret := chartFile(t, charts, fmt.Sprintf("templates/%s/secret-0.yaml", inst))
		if !strings.Contains(secret, "kind: Secret") {
			t.Errorf("%s secret-0 is not a Secret\n--- secret ---\n%s", inst, secret)
		}
	}
}

// TestEventbusGetsALongerLivenessBudget guards the k8s half of the 2026-09-13
// eventbus crash loop.
//
// eventbus embeds its Kafka broker in its own JVM, so restarting the pod also
// restarts the broker: every downstream Subscribe RPC reconnects at once (~25
// sessions inside one second on the live fleet) and the consumer groups
// rebalance. eventbus's health check probes Kafka, so it answers NOT_SERVING
// for as long as that takes — measured at 87-135s.
//
// The shared helper allows periodSeconds 10 x failureThreshold 3 = 30s, so the
// kubelet killed eventbus mid-rebalance on every restart, and each kill made
// the next rebalance dirtier: 7 restarts into CrashLoopBackOff. A post-restart
// rebalance is functionally a second cold start and needs a cold-start budget.
//
// eventbus's own UNHEALTHY_STREAK_THRESHOLD reports UNHEALTHY at ~240s, so the
// pod is normally restarted by its own verdict and this probe is the backstop —
// which is why this budget must stay LONGER than that 240s.
func TestEventbusGetsALongerLivenessBudget(t *testing.T) {
	src, err := os.ReadFile("generator.go")
	if err != nil {
		t.Fatalf("read generator.go: %v", err)
	}
	body := string(src)

	if !strings.Contains(body, `{{- define "schematic.grpcProbeEventbus" -}}`) {
		t.Fatal("the schematic.grpcProbeEventbus helper is missing")
	}
	if !strings.Contains(body, `if svc.Name == "eventbus" {`) ||
		!strings.Contains(body, `probeHelper = "schematic.grpcProbeEventbus"`) {
		t.Error("eventbus must select schematic.grpcProbeEventbus as its probe helper")
	}

	// Pull the helper body out and check the budget it actually encodes.
	i := strings.Index(body, `{{- define "schematic.grpcProbeEventbus" -}}`)
	j := strings.Index(body[i:], "{{- end }}")
	if j < 0 {
		t.Fatal("schematic.grpcProbeEventbus is not terminated")
	}
	helper := body[i : i+j]

	if !strings.Contains(helper, "grpc:\n  port: 8444") {
		t.Error("the eventbus probe must query grpc.health.v1 on the plaintext port 8444")
	}
	if strings.Contains(helper, "tcpSocket") {
		t.Error("a TCP accept stays true while eventbus's guts are dead")
	}
	if !strings.Contains(helper, "timeoutSeconds: 5") {
		t.Error("the eventbus probe needs timeoutSeconds above the 1s default, or the JVM flaps")
	}

	period := probeFieldSeconds(t, helper, "periodSeconds")
	threshold := probeFieldSeconds(t, helper, "failureThreshold")
	budget := period * threshold

	// Must clear the worst rebalance actually measured (135786ms), with margin.
	if budget < 270 {
		t.Errorf("eventbus liveness budget is %ds (period %d x threshold %d); needs >= 270s to "+
			"clear the observed 136s rebalance with margin", budget, period, threshold)
	}
	// Must stay behind eventbus's own 240s UNHEALTHY_STREAK_THRESHOLD so the
	// service's own verdict is what restarts it and this stays the backstop.
	if budget <= 240 {
		t.Errorf("eventbus liveness budget is %ds, which trips at or before eventbus's own "+
			"240s health verdict; the kubelet must be the backstop, not the trigger", budget)
	}
}

// probeFieldSeconds reads an integer "<field>: N" out of a rendered probe helper.
func probeFieldSeconds(t *testing.T, helper, field string) int {
	t.Helper()
	for _, line := range strings.Split(helper, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, field+":") {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, field+":")), "%d", &n); err != nil {
			t.Fatalf("parse %s from %q: %v", field, line, err)
		}
		return n
	}
	t.Fatalf("probe helper does not set %s:\n%s", field, helper)
	return 0
}

// TestGatewayLoadBalancerCarriesBothPorts guards the shape of the edge: ONE
// LoadBalancer on the reserved address, carrying 443 and 80. gateway0-lb held
// 34.70.146.195 in the live cluster only because it was kubectl apply'd by
// hand — the generator rendered no loadBalancerIP at all, so the next routine
// re-render would have handed the gateway a fresh ephemeral address and broken
// the A record pointing at it. Port 80 must ride the SAME Service: ACME
// HTTP-01 validation connects to port 80 on the NAME, which resolves to this
// one address, so a second LoadBalancer for port 80 takes an address of its
// own and is never reached.
func TestGatewayLoadBalancerCarriesBothPorts(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:  "schematic",
		Fqdn:       "console.seabeam.net",
		Port:       443,
		ExternalIp: "34.70.146.195",
		Tls:        &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			KmakerNodes:  1,
			GatewayNodes: 1,
		},
	})

	gatewayLB := chartFile(t, charts, "templates/gateway/service-0-lb.yaml")
	if !strings.Contains(gatewayLB, "  type: LoadBalancer\n  loadBalancerIP: 34.70.146.195\n") {
		t.Errorf("gateway -lb Service does not pin the reserved address:\n%s", gatewayLB)
	}
	if !strings.Contains(gatewayLB, "  - name: https\n    port: 443\n    targetPort: 8443\n") {
		t.Errorf("gateway -lb Service does not serve 443:\n%s", gatewayLB)
	}
	if !strings.Contains(gatewayLB, "  - name: http\n    port: 80\n    targetPort: 8080\n") {
		t.Errorf("gateway -lb Service does not serve 80 — HTTP-01 validation has nowhere to land:\n%s", gatewayLB)
	}

	// The gateway pod must actually declare the port the Service targets.
	gatewayDeploy := chartFile(t, charts, "templates/gateway/deployment.yaml")
	if !strings.Contains(gatewayDeploy, "        - containerPort: 8080\n          name: http\n") {
		t.Errorf("gateway Deployment declares no http containerPort:\n%s", gatewayDeploy)
	}
}

// TestNoExternalAcmeLoadBalancer is the other half of the one-LB design: the
// letsencrypt service keeps its HTTP-01 responder, but only in-cluster. The
// gateway holds port 80 and proxies /.well-known/acme-challenge/* to it, so a
// second externally-exposed Service would be an address nothing points at and
// a second public surface to defend.
func TestNoExternalAcmeLoadBalancer(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:  "schematic",
		Fqdn:       "console.seabeam.net",
		Port:       443,
		ExternalIp: "34.70.146.195",
		Tls:        &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			KmakerNodes:  1,
			GatewayNodes: 1,
		},
	})

	for name := range charts.Files {
		if strings.HasSuffix(name, "-acme.yaml") {
			t.Errorf("chart still renders an external ACME Service: %s", name)
		}
	}
	for name, body := range charts.Files {
		if strings.Contains(string(body), "type: LoadBalancer") && name != "templates/gateway/service-0-lb.yaml" {
			t.Errorf("%s is a second LoadBalancer; the design has exactly one:\n%s", name, body)
		}
	}

	// The responder still has to be reachable by the gateway.
	leSvc := chartFile(t, charts, "templates/letsencrypt/service-0.yaml")
	if strings.Contains(leSvc, "type: LoadBalancer") {
		t.Errorf("letsencrypt Service must stay ClusterIP:\n%s", leSvc)
	}
	if !strings.Contains(leSvc, "  - name: acme\n    port: 8080\n    targetPort: 8080\n") {
		t.Errorf("letsencrypt ClusterIP Service does not expose the ACME responder in-cluster:\n%s", leSvc)
	}
}

// TestCertChainModeRendersNoPort80 keeps port 80 tied to the reason it exists.
// Without letsencrypt there is no challenge to carry and no listener bound on
// 8080, so a port-80 forwarding rule would point into a closed port.
func TestCertChainModeRendersNoPort80(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace:  "schematic",
		Fqdn:       "console.seabeam.net",
		Port:       443,
		ExternalIp: "34.70.146.195",
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			KmakerNodes:  1,
			GatewayNodes: 1,
		},
	})

	gatewayLB := chartFile(t, charts, "templates/gateway/service-0-lb.yaml")
	if strings.Contains(gatewayLB, "port: 80\n") {
		t.Errorf("cert_chain mode must not render a port 80 entry:\n%s", gatewayLB)
	}
}

// TestUnsetExternalIPRendersNoLoadBalancerIP keeps clusters that reserve no
// address on the provider-assigned one. An empty external_ip rendering
// "loadBalancerIP:" with no value is a chart that does not install.
func TestUnsetExternalIPRendersNoLoadBalancerIP(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Fqdn:      "cavalier.localtest.me",
		Port:      8443,
		Tls:       &protoOperator.TLSConfig{Letsencrypt: true},
		Topology: &protoOperator.Topology{
			KernelNodes:  1,
			KmakerNodes:  1,
			GatewayNodes: 1,
		},
	})

	if body := chartFile(t, charts, "templates/gateway/service-0-lb.yaml"); strings.Contains(body, "loadBalancerIP") {
		t.Errorf("gateway -lb names loadBalancerIP while external_ip is unset:\n%s", body)
	}
}

// TestBlobstoreRendersAMirroredStatefulSetPair is the operator half of
// blob-store-2026-09-25.md: a 2-replica StatefulSet, a PVC per pod, both
// in-cluster ports published on the headless Service so the pods can dial each
// other by name, the peer list in the environment, and the config as a Secret.
//
// Each assertion is a way this service silently does not work. Rendered as
// Deployments the pods would have no stable name to mirror to; without the peer
// port on the HEADLESS Service `blobstore-1.blobstore:8081` would not resolve;
// without the PVC the blobs would live on an ephemeral filesystem and every write
// would still succeed.
func TestBlobstoreRendersAMirroredStatefulSetPair(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:    1,
			KmakerNodes:    1,
			BlobstoreNodes: 2,
		},
		OptionalServices: &protoOperator.OptionalServices{Blobstore: true},
	})

	ss := chartFile(t, charts, "templates/blobstore/statefulset.yaml")
	if !strings.Contains(ss, "kind: StatefulSet") {
		t.Fatalf("blobstore must be a StatefulSet:\n%s", ss)
	}
	if !strings.Contains(ss, "replicas: 2") {
		t.Errorf("blobstore must render 2 replicas — the mirror is a pair:\n%s", ss)
	}
	if !strings.Contains(ss, "volumeClaimTemplates") {
		t.Errorf("blobstore must own a volumeClaimTemplate; each pod holds its own copy of every blob:\n%s", ss)
	}
	for _, want := range []string{"name: bytes", "name: peer"} {
		if !strings.Contains(ss, want) {
			t.Errorf("the StatefulSet is missing container port %q:\n%s", want, ss)
		}
	}
	// The peer list names both pods at their stable per-pod DNS, this pod's own
	// included: the service picks itself out by hostname.
	if !strings.Contains(ss, "BLOBSTORE_PEERS") {
		t.Errorf("the StatefulSet does not carry BLOBSTORE_PEERS; neither pod would find the other:\n%s", ss)
	}
	for _, want := range []string{"blobstore-0.blobstore:8081", "blobstore-1.blobstore:8081"} {
		if !strings.Contains(ss, want) {
			t.Errorf("BLOBSTORE_PEERS does not name %s:\n%s", want, ss)
		}
	}
	// And NOT the blobstore0 form a Deployment would use — that name resolves to
	// nothing for a StatefulSet.
	if strings.Contains(ss, "blobstore0.blobstore") {
		t.Errorf("BLOBSTORE_PEERS uses the Deployment pod-name form, which never resolves:\n%s", ss)
	}

	headless := chartFile(t, charts, "templates/blobstore/service-headless.yaml")
	if !strings.Contains(headless, "clusterIP: None") {
		t.Errorf("blobstore has no headless Service, so per-pod DNS would not exist:\n%s", headless)
	}
	for _, want := range []string{"name: peer", "port: 8081", "name: bytes", "port: 8080"} {
		if !strings.Contains(headless, want) {
			t.Errorf("the headless Service is missing %q — a peer could not reach that port:\n%s", want, headless)
		}
	}

	// The gateway reaches the bytes port through the per-replica ClusterIP Service.
	svc := chartFile(t, charts, "templates/blobstore/service-0.yaml")
	if !strings.Contains(svc, "name: bytes") || !strings.Contains(svc, "port: 8080") {
		t.Errorf("the per-replica Service does not publish the bytes port; the gateway could not proxy /b/:\n%s", svc)
	}

	// blobstore's config is a plain ConfigMap. It carries no key material: the
	// upload token is raw randomness verified by a record lookup, so there is no
	// signing key to package. It WAS a Secret when it carried token_hmac_key, and
	// that field no longer exists (decided 2026-09-27).
	cm := chartFile(t, charts, "templates/blobstore/configmap-0.yaml")
	if !strings.Contains(cm, "kind: ConfigMap") {
		t.Errorf("blobstore's config is not a ConfigMap:\n%s", cm)
	}
	if _, ok := charts.Files["templates/blobstore/secret-0.yaml"]; ok {
		t.Error("blobstore's config is still packaged as a Secret; it holds no key material")
	}
	// And no chart file names a signing key for it anywhere.
	for path, body := range charts.Files {
		if strings.Contains(string(body), "token_hmac_key") || strings.Contains(string(body), "tokenHmacKey") {
			t.Errorf("%s references an upload-token signing key; no such key exists", path)
		}
	}

	// Nothing is exposed outside the cluster: neither byte path has a
	// LoadBalancer, because the gateway owns the edge.
	for path, body := range charts.Files {
		if strings.Contains(path, "blobstore") && strings.Contains(string(body), "type: LoadBalancer") {
			t.Errorf("%s exposes blobstore outside the cluster; the gateway is the only route to it", path)
		}
	}
}

// TestBlobstoreRendersNothingWhenDisabled — it costs two PersistentVolumeClaims,
// so a cluster that has not asked for it must not get them.
func TestBlobstoreRendersNothingWhenDisabled(t *testing.T) {
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:    1,
			KmakerNodes:    1,
			BlobstoreNodes: 2,
		},
	})
	for path := range charts.Files {
		if strings.Contains(path, "blobstore") {
			t.Errorf("blobstore rendered %s while optional_services.blobstore is false", path)
		}
	}
}

// A digest in image_digests pins that image to <repository>@<digest> in place
// of its tag; images with no entry keep the tag they had.
func TestImageDigestReplacesTagForPinnedImageOnly(t *testing.T) {
	t.Setenv("SCHEMATIC_LOCAL_IMAGES", "localhost:5001")
	digest := "sha256:" + strings.Repeat("ab", 32)
	charts := generateTestChart(t, &protoOperator.Config{
		Namespace: "schematic",
		Topology: &protoOperator.Topology{
			KernelNodes:   1,
			SentinelNodes: 1,
		},
		ImageDigests: map[string]string{"kernel": digest},
	})

	values := chartFile(t, charts, "values.yaml")
	for _, want := range []string{
		"kernel:\n  image: localhost:5001/schematic/kernel@" + digest + "\n",
		"sentinel:\n  image: localhost:5001/schematic/sentinel:local\n",
	} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml missing %q\n--- values.yaml ---\n%s", want, values)
		}
	}
}

// Two builds pushed under the same :local tag differ only in digest. With the
// digests pinned, the second build must change the pod template so the
// rollout happens on its own instead of by `rollout restart`.
func TestNewImageDigestChangesRenderedPodTemplate(t *testing.T) {
	t.Setenv("SCHEMATIC_LOCAL_IMAGES", "us-central1-docker.pkg.dev/proj/seabeam")
	kernelTemplate := func(digest string) string {
		cfg := &protoOperator.Config{
			Namespace:    "schematic",
			Topology:     &protoOperator.Topology{KernelNodes: 1},
			ImageDigests: map[string]string{"kernel": digest},
		}
		var st appsv1.StatefulSet
		decodeManifest(t, renderedManifests(t, generateTestChart(t, cfg)), "StatefulSet/kernel", &st)
		var image string
		for _, c := range st.Spec.Template.Spec.Containers {
			if c.Name == "kernel" {
				image = c.Image
			}
		}
		if want := "us-central1-docker.pkg.dev/proj/seabeam/schematic/kernel@" + digest; image != want {
			t.Fatalf("kernel image = %q, want %q", image, want)
		}
		out, err := k8syaml.Marshal(st.Spec.Template)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}

	first := kernelTemplate("sha256:" + strings.Repeat("11", 32))
	second := kernelTemplate("sha256:" + strings.Repeat("22", 32))
	if first == second {
		t.Fatal("a new build's digest left kernel's pod template unchanged: the StatefulSet would not roll")
	}
}
